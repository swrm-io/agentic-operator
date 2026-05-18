package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// claudeTokenURL is extracted from the Claude Code binary.
	claudeTokenURL = "https://platform.claude.com/v1/oauth/token"

	// claudeClientID is the OAuth client ID extracted from the Claude Code binary.
	claudeClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"

	// claudeTokenScope is the full scope string required for Claude Code.
	claudeTokenScope = "user:inference user:profile user:sessions:claude_code user:mcp_servers user:file_upload"

	// RefreshLabel is applied to Secrets managed by the token refresher.
	// kubectl label secret my-secret agentic.swrm.io/token-refresh=enabled
	RefreshLabel = "agentic.swrm.io/token-refresh"

	// refreshBefore is how early to refresh before the token expires.
	refreshBefore = 5 * time.Minute
)

// claudeCredentials mirrors the structure of ~/.claude/.credentials.json
type claudeCredentials struct {
	ClaudeAiOauth claudeOauthToken `json:"claudeAiOauth"`
}

type claudeOauthToken struct {
	AccessToken      string   `json:"accessToken"`
	RefreshToken     string   `json:"refreshToken"`
	ExpiresAt        int64    `json:"expiresAt"` // Unix ms
	Scopes           []string `json:"scopes"`
	SubscriptionType string   `json:"subscriptionType"`
	RateLimitTier    string   `json:"rateLimitTier"`
}

// tokenRefreshRequest is the JSON body sent to the token endpoint.
type tokenRefreshRequest struct {
	GrantType    string `json:"grant_type"`
	RefreshToken string `json:"refresh_token"`
	ClientID     string `json:"client_id"`
	Scope        string `json:"scope"`
}

// tokenRefreshResponse is the JSON body returned by the token endpoint.
type tokenRefreshResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int64  `json:"expires_in"` // seconds
	Scope        string `json:"scope"`
}

// cacheEntry holds the cached expiry time for a single credentials Secret.
type cacheEntry struct {
	key       string    // data key within the Secret (e.g. "credentials.json")
	expiresAt time.Time // when the current access token expires
}

// TokenRefresher watches Secrets labelled with RefreshLabel and proactively
// refreshes the Claude OAuth token before it expires.
//
// Rather than polling on a fixed interval, it maintains an in-memory cache of
// expiresAt per secret. It sleeps until the next token is due for refresh,
// wakes only then, refreshes that one secret, and reschedules. Secrets are
// added/removed from the cache via a controller-runtime watch — no periodic
// list calls are made after the initial startup sync.
type TokenRefresher struct {
	client     client.Client
	httpClient *http.Client

	mu      sync.Mutex
	cache   map[types.NamespacedName]cacheEntry
	wakeup  chan struct{} // closed and replaced to wake the sleep loop early
}

// NewTokenRefresher creates a TokenRefresher using the manager's client.
func NewTokenRefresher(c client.Client) *TokenRefresher {
	return &TokenRefresher{
		client:     c,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		cache:      make(map[types.NamespacedName]cacheEntry),
		wakeup:     make(chan struct{}),
	}
}

// SetupWithManager registers a lightweight controller that watches Secrets
// labelled with RefreshLabel and keeps the in-memory cache up to date.
// Must be called before the manager starts.
func (t *TokenRefresher) SetupWithManager(mgr ctrl.Manager) error {
	labelPredicate, err := predicate.LabelSelectorPredicate(metav1.LabelSelector{
		MatchLabels: map[string]string{RefreshLabel: "enabled"},
	})
	if err != nil {
		return fmt.Errorf("build label predicate: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		Named("token-refresher-watch").
		Watches(
			&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []reconcile.Request {
				return []reconcile.Request{{
					NamespacedName: types.NamespacedName{
						Namespace: obj.GetNamespace(),
						Name:      obj.GetName(),
					},
				}}
			}),
			builder.WithPredicates(labelPredicate),
		).
		Complete(reconcile.Func(func(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
			return reconcile.Result{}, t.syncSecret(ctx, req.NamespacedName)
		}))
}

// Start implements manager.Runnable. It seeds the cache from a one-time List,
// then sleeps until the next token needs refreshing.
func (t *TokenRefresher) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("token-refresher")
	logger.Info("starting token refresher")

	if err := t.seedCache(ctx); err != nil {
		logger.Error(err, "failed to seed token cache — will retry on next watch event")
	}

	for {
		sleep, key, ok := t.nextRefreshIn()

		if !ok {
			// No secrets in cache — wait for a wakeup signal.
			select {
			case <-ctx.Done():
				return nil
			case <-t.wakeup:
				continue
			}
		}

		if sleep <= 0 {
			// Due now.
			if err := t.refreshSecret(ctx, key); err != nil {
				logger.Error(err, "failed to refresh token", "secret", key)
			}
			continue
		}

		logger.Info("next token refresh scheduled", "secret", key, "in", sleep.Round(time.Second))
		timer := time.NewTimer(sleep)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-t.wakeup:
			// Cache changed (new secret added or expiry updated) — recalculate.
			timer.Stop()
		case <-timer.C:
			if err := t.refreshSecret(ctx, key); err != nil {
				logger.Error(err, "failed to refresh token", "secret", key)
				// Back off briefly before retrying so we don't hammer the API.
				select {
				case <-ctx.Done():
					return nil
				case <-time.After(30 * time.Second):
				}
			}
		}
	}
}

// seedCache does the initial List to populate the cache on startup.
func (t *TokenRefresher) seedCache(ctx context.Context) error {
	secretList := &corev1.SecretList{}
	if err := t.client.List(ctx, secretList, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(labels.Set{RefreshLabel: "enabled"}),
	}); err != nil {
		return err
	}
	for i := range secretList.Items {
		t.updateCache(&secretList.Items[i])
	}
	return nil
}

// syncSecret is called by the controller-runtime watch whenever a Secret is
// added, updated, or deleted. It keeps the cache in sync.
func (t *TokenRefresher) syncSecret(ctx context.Context, key types.NamespacedName) error {
	secret := &corev1.Secret{}
	err := t.client.Get(ctx, key, secret)
	if err != nil {
		if client.IgnoreNotFound(err) == nil {
			t.removeFromCache(key)
			return nil
		}
		return err
	}

	if secret.Labels[RefreshLabel] != "enabled" {
		t.removeFromCache(key)
		return nil
	}

	t.updateCache(secret)
	return nil
}

// updateCache parses expiresAt from the secret and stores it in the cache,
// then signals the sleep loop to recalculate.
func (t *TokenRefresher) updateCache(secret *corev1.Secret) {
	key := types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}
	credKey := credentialsKeyForSecret(secret)

	raw, ok := secret.Data[credKey]
	if !ok {
		return
	}
	var creds claudeCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return
	}

	t.mu.Lock()
	t.cache[key] = cacheEntry{
		key:       credKey,
		expiresAt: time.UnixMilli(creds.ClaudeAiOauth.ExpiresAt),
	}
	t.mu.Unlock()

	t.signal()
}

// removeFromCache removes a secret from the cache and wakes the loop.
func (t *TokenRefresher) removeFromCache(key types.NamespacedName) {
	t.mu.Lock()
	delete(t.cache, key)
	t.mu.Unlock()
	t.signal()
}

// signal wakes the sleep loop by replacing the wakeup channel.
func (t *TokenRefresher) signal() {
	t.mu.Lock()
	old := t.wakeup
	t.wakeup = make(chan struct{})
	t.mu.Unlock()
	close(old)
}

// nextRefreshIn returns how long to sleep until the next token needs
// refreshing, and which secret that is. Returns ok=false if the cache is empty.
func (t *TokenRefresher) nextRefreshIn() (time.Duration, types.NamespacedName, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var earliest time.Time
	var earliestKey types.NamespacedName
	first := true

	for k, e := range t.cache {
		due := e.expiresAt.Add(-refreshBefore)
		if first || due.Before(earliest) {
			earliest = due
			earliestKey = k
			first = false
		}
	}

	if first {
		return 0, types.NamespacedName{}, false
	}

	return time.Until(earliest), earliestKey, true
}

// refreshSecret fetches the secret from the API, calls the token endpoint,
// and patches the secret with the new credentials.
func (t *TokenRefresher) refreshSecret(ctx context.Context, key types.NamespacedName) error {
	logger := log.FromContext(ctx).WithName("token-refresher")

	secret := &corev1.Secret{}
	if err := t.client.Get(ctx, key, secret); err != nil {
		return fmt.Errorf("get secret: %w", err)
	}

	credKey := credentialsKeyForSecret(secret)
	raw, ok := secret.Data[credKey]
	if !ok {
		return fmt.Errorf("secret %s has no key %q", key, credKey)
	}

	var creds claudeCredentials
	if err := json.Unmarshal(raw, &creds); err != nil {
		return fmt.Errorf("unmarshal credentials: %w", err)
	}

	logger.Info("refreshing token", "secret", key,
		"expiresAt", time.UnixMilli(creds.ClaudeAiOauth.ExpiresAt).Format(time.RFC3339))

	newToken, err := t.doRefresh(ctx, creds.ClaudeAiOauth.RefreshToken)
	if err != nil {
		return fmt.Errorf("token refresh API call: %w", err)
	}

	creds.ClaudeAiOauth.AccessToken = newToken.AccessToken
	creds.ClaudeAiOauth.RefreshToken = newToken.RefreshToken
	creds.ClaudeAiOauth.ExpiresAt = time.Now().UnixMilli() + newToken.ExpiresIn*1000

	updated, err := json.Marshal(creds)
	if err != nil {
		return fmt.Errorf("marshal updated credentials: %w", err)
	}

	patch := client.MergeFrom(secret.DeepCopy())
	secret.Data[credKey] = updated
	if err := t.client.Patch(ctx, secret, patch); err != nil {
		return fmt.Errorf("patch secret: %w", err)
	}

	newExpiresAt := time.UnixMilli(creds.ClaudeAiOauth.ExpiresAt)
	logger.Info("token refreshed successfully", "secret", key,
		"newExpiresAt", newExpiresAt.Format(time.RFC3339))

	// Update the cache with the new expiry so the loop reschedules correctly.
	t.mu.Lock()
	if e, ok := t.cache[key]; ok {
		e.expiresAt = newExpiresAt
		t.cache[key] = e
	}
	t.mu.Unlock()

	return nil
}

// doRefresh calls the Claude OAuth token endpoint and returns the new tokens.
func (t *TokenRefresher) doRefresh(ctx context.Context, refreshToken string) (*tokenRefreshResponse, error) {
	body, err := json.Marshal(tokenRefreshRequest{
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
		ClientID:     claudeClientID,
		Scope:        claudeTokenScope,
	})
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeTokenURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("token endpoint returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result tokenRefreshResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("unmarshal token response: %w", err)
	}

	return &result, nil
}

// credentialsKeyForSecret returns the data key to use within the Secret.
// Defaults to "credentials.json"; can be overridden via an annotation.
func credentialsKeyForSecret(secret *corev1.Secret) string {
	if key, ok := secret.Annotations["agentic.swrm.io/credentials-key"]; ok && key != "" {
		return key
	}
	return "credentials.json"
}
