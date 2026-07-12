package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestSecret(name string, expiresAt time.Time) *corev1.Secret {
	creds := claudeCredentials{
		ClaudeAiOauth: claudeOauthToken{
			AccessToken:  "sk-ant-oat01-old",
			RefreshToken: "sk-ant-ort01-old",
			ExpiresAt:    expiresAt.UnixMilli(),
		},
	}
	raw, err := json.Marshal(creds)
	if err != nil {
		panic(err)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			Labels:    map[string]string{RefreshLabel: "enabled"},
		},
		Data: map[string][]byte{
			"credentials.json": raw,
		},
	}
}

func TestRefreshSecret_TransientFailureBacksOff(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"server_error"}`))
	}))
	defer server.Close()

	secret := newTestSecret("creds-transient", time.Now().Add(-1*time.Minute))
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(secret).Build()

	tr := NewTokenRefresher(c)
	tr.tokenURL = server.URL
	key := types.NamespacedName{Namespace: "default", Name: "creds-transient"}
	tr.updateCache(secret)

	ctx := context.Background()
	err := tr.refreshSecret(ctx, key)
	if err == nil {
		t.Fatal("expected error from refreshSecret, got nil")
	}
	tr.handleRefreshFailure(ctx, key, err)

	// Root cause of the busy loop: nextRefreshIn() must not report the
	// secret as immediately due again after a failed attempt, otherwise
	// the Start() loop spins with no delay between retries.
	sleep, gotKey, ok := tr.nextRefreshIn()
	if !ok {
		t.Fatal("expected secret to still be scheduled for retry")
	}
	if gotKey != key {
		t.Fatalf("expected key %v, got %v", key, gotKey)
	}
	if sleep <= 0 {
		t.Fatalf("expected positive backoff after failed refresh, got %v (busy-loop)", sleep)
	}
}

func TestRefreshSecret_InvalidGrantStopsRetrying(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error": "invalid_grant", "error_description": "Refresh token expired"}`))
	}))
	defer server.Close()

	secret := newTestSecret("creds-dead", time.Now().Add(-1*time.Minute))
	c := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(secret).Build()

	tr := NewTokenRefresher(c)
	tr.tokenURL = server.URL
	key := types.NamespacedName{Namespace: "default", Name: "creds-dead"}
	tr.updateCache(secret)

	ctx := context.Background()
	err := tr.refreshSecret(ctx, key)
	if err == nil {
		t.Fatal("expected error from refreshSecret, got nil")
	}
	if !isInvalidGrant(err) {
		t.Fatalf("expected invalid_grant error, got: %v", err)
	}
	tr.handleRefreshFailure(ctx, key, err)

	// An unrecoverable invalid_grant must stop the retry loop for this
	// secret entirely — there is no automated recovery, a human must
	// rotate the credential and the watch will pick it back up.
	_, _, ok := tr.nextRefreshIn()
	if ok {
		t.Fatal("expected secret to be removed from retry schedule after invalid_grant")
	}
}
