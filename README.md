# agentic-operator

> **This project is AI-generated and AI-maintained.**
> The code, documentation, and architecture were designed and implemented by [Claude Code](https://claude.ai/code) (Anthropic). Human oversight is applied at a high level — reviewing goals, answering design questions, and approving changes — but the implementation is written and maintained by AI. Treat it accordingly: read the code, understand what it does, and don't trust it blindly.

A Kubernetes operator for running [Claude Code](https://code.claude.com) workloads on-cluster. It provides two custom resources:

- **`ClaudeJob`** — runs Claude Code on a cron schedule, passing a prompt to `claude --print`
- **`ClaudeSession`** — runs a persistent Claude Code [Remote Control](https://code.claude.com/docs/en/remote-control) session in a Pod, accessible from claude.ai or the Claude mobile app

## Features

- One `ClaudeJob` per scheduled task, mapped 1:1 to a Kubernetes `CronJob`
- One `ClaudeSession` per persistent remote session, mapped 1:1 to a Kubernetes `Pod`
- Two authentication modes for `ClaudeJob`: OAuth credentials or API key
- Automatic OAuth token refresh — a single controller goroutine proactively refreshes credentials before expiry, preventing the race condition that would occur if multiple pods each attempted to refresh the shared refresh token
- Namespace-scoped resources

## Authentication

### OAuth (claude.ai login)

Required for `ClaudeSession`. Supported for `ClaudeJob`. Enables automatic token refresh.

```bash
# Log in on any machine
claude /login

# Get your organization UUID (required for Remote Control eligibility)
ORG_ID=$(cat $HOME/.claude.json | python3 -c "import json,sys; print(json.load(sys.stdin)['oauthAccount']['organizationUuid'])")

# Create the secret
kubectl create secret generic claude-credentials \
  --from-file=credentials.json=$HOME/.claude/.credentials.json \
  --from-literal=organizationId=$ORG_ID

# Opt in to automatic token refresh
kubectl label secret claude-credentials agentic.swrm.io/token-refresh=enabled
```

### API Key

Supported for `ClaudeJob` only. Cannot be used with `ClaudeSession` (Remote Control requires a claude.ai OAuth token).

```bash
kubectl create secret generic anthropic-api-key \
  --from-literal=key=sk-ant-...
```

## Custom Resources

### ClaudeJob

```yaml
apiVersion: agentic.swrm.io/v1alpha1
kind: ClaudeJob
metadata:
  name: daily-review
spec:
  schedule: "0 9 * * *"
  prompt: "Review open PRs and summarize findings"
  image: ghcr.io/swrm-io/claude-code:latest
  auth:
    # OAuth mode
    credentialsSecret:
      name: claude-credentials
      key: credentials.json
    # -- OR --
    # apiKeySecret:
    #   name: anthropic-api-key
    #   key: key
  workDir: /workspace
```

### ClaudeSession

```yaml
apiVersion: agentic.swrm.io/v1alpha1
kind: ClaudeSession
metadata:
  name: my-session
spec:
  sessionName: "My Remote Session"
  image: ghcr.io/swrm-io/claude-code:latest
  credentialsSecret:
    name: claude-credentials
    key: credentials.json
  workDir: /workspace
  spawn: same-dir  # same-dir | worktree | session
```

After creating a `ClaudeSession`, connect to it from [claude.ai/code](https://claude.ai/code) or the Claude mobile app — the session will appear in the session list under the configured `sessionName`.

## Token Refresh

The operator maintains an in-memory cache of `expiresAt` per credentials Secret and sleeps until the next token is actually due for refresh — no periodic polling. When a token is within 5 minutes of expiry it calls `POST https://platform.claude.com/v1/oauth/token` with `grant_type=refresh_token` and writes the new access token, refresh token, and expiry back to the Secret.

The cache is kept up to date via a controller-runtime watch on labelled Secrets — any add, update, or delete wakes the refresh loop immediately so it can recalculate the next scheduled refresh.

Since the refresh token rotates on each use, only the operator ever refreshes it — pods mount credentials read-only and never attempt a refresh themselves.

To opt a Secret in:

```bash
kubectl label secret claude-credentials agentic.swrm.io/token-refresh=enabled
```

If your credentials key within the Secret is not `credentials.json`, annotate it:

```bash
kubectl annotate secret my-secret agentic.swrm.io/credentials-key=my-key.json
```

## Images

Anthropic does not publish a Docker image for Claude Code. This project builds its own from `Dockerfile.claude`, which installs `@anthropic-ai/claude-code` from npm into a `node:22-slim` base.

```bash
# Build with the latest stable Claude Code release
make claude-build CLAUDE_IMG=ghcr.io/your-org/claude-code:latest

# Pin to a specific version for reproducibility
make claude-build CLAUDE_IMG=ghcr.io/your-org/claude-code:2.1.86 CLAUDE_VERSION=2.1.86

# Push
make claude-push CLAUDE_IMG=ghcr.io/your-org/claude-code:latest

# Multi-platform build (amd64 + arm64) and push in one step
make claude-buildx CLAUDE_IMG=ghcr.io/your-org/claude-code:latest
```

Set the resulting image in your CRs via `spec.image`.

## Getting Started

### Prerequisites

- Go v1.25+
- Docker v17.03+
- kubectl v1.11.3+
- Access to a Kubernetes v1.33+ cluster (built against 1.35)
- operator-sdk v1.39+

### Helm (recommended)

```bash
# Install into a dedicated namespace (creates it if it doesn't exist)
helm install agentic-operator charts/agentic-operator \
  --namespace agentic-operator-system \
  --create-namespace \
  --set image.repository=ghcr.io/swrm-io/agentic-operator \
  --set image.tag=0.0.1

# Or via make
make helm-install IMG=ghcr.io/swrm-io/agentic-operator:0.0.1

# Uninstall
make helm-uninstall
```

Key values:

| Value | Default | Description |
|---|---|---|
| `image.repository` | `ghcr.io/swrm-io/agentic-operator` | Operator image |
| `image.tag` | chart `appVersion` | Image tag |
| `replicaCount` | `1` | Manager replicas |
| `leaderElection` | `true` | Enable leader election |
| `installCRDs` | `true` | Install CRDs with the chart |
| `resources` | see values.yaml | Manager resource requests/limits |

### Run locally against your cluster

```bash
# Install CRDs
make install

# Run the operator
make run
```

### Deploy to cluster

```bash
# Build and push the image
make docker-build docker-push IMG=<registry>/agentic-operator:tag

# Deploy
make deploy IMG=<registry>/agentic-operator:tag
```

### Apply samples

```bash
kubectl apply -k config/samples/
```

### Uninstall

```bash
kubectl delete -k config/samples/
make uninstall
make undeploy
```

## Project Distribution

Build a single-file installer:

```bash
make build-installer IMG=<registry>/agentic-operator:tag
kubectl apply -f dist/install.yaml
```

