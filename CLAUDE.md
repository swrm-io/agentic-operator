# CLAUDE.md

This file provides context for Claude Code when working in this repository.

## Project overview

`agentic-operator` is a Kubernetes operator built with operator-sdk (Go). It manages two custom resources:

- **`ClaudeJob`** (`agentic.swrm.io/v1alpha1`) — runs Claude Code on a cron schedule via a Kubernetes `CronJob`. Supports OAuth credentials or API key auth.
- **`ClaudeSession`** (`agentic.swrm.io/v1alpha1`) — runs a persistent Claude Code Remote Control session in a `Pod`. OAuth only (Remote Control requires a claude.ai subscription token).

There is also a background `TokenRefresher` goroutine that proactively refreshes OAuth credentials in labelled Secrets before they expire, preventing refresh-token race conditions across pods.

## Repository layout

```
api/v1alpha1/
  claudejob_types.go       # ClaudeJob CRD types, ClaudeJobAuth, SecretKeyRef
  claudesession_types.go   # ClaudeSession CRD types, SpawnMode, SessionPhase
  zz_generated.deepcopy.go # Auto-generated — do not edit

internal/controller/
  claudejob_controller.go     # Reconciles ClaudeJob -> CronJob
  claudesession_controller.go # Reconciles ClaudeSession -> Pod
  tokenrefresher.go           # Background OAuth token refresh goroutine

cmd/main.go                  # Manager entrypoint, wires up controllers + refresher

config/
  crd/bases/                 # Generated CRD manifests — do not edit
  rbac/role.yaml             # Generated RBAC — do not edit
  samples/                   # Example ClaudeJob and ClaudeSession CRs

Dockerfile                   # Operator manager image (distroless)
Dockerfile.claude            # Claude Code pod image (node:22-slim + @anthropic-ai/claude-code)
Makefile                     # Build, generate, deploy targets
```

## Key design decisions

- **One resource per task/session** — `ClaudeJob` and `ClaudeSession` are 1:1 with a `CronJob` or `Pod`. No list-of-jobs inside one resource.
- **Auth is mutually exclusive** — `ClaudeJob.spec.auth` has exactly one of `credentialsSecret` (OAuth) or `apiKeySecret` (API key). `validateAuth()` enforces this at reconcile time.
- **Token refresh is centralised** — only the `TokenRefresher` goroutine ever writes back to credentials Secrets. Pods mount credentials read-only. This prevents the OAuth refresh-token race. The refresher caches `expiresAt` per secret and sleeps until the next token is due — no periodic polling.
- **No Anthropic-published image** — Anthropic does not publish a Docker image. `Dockerfile.claude` builds one from `@anthropic-ai/claude-code` on npm.

## OAuth token internals

Discovered by inspecting the Claude Code binary (`strings`):

- Token endpoint: `POST https://platform.claude.com/v1/oauth/token`
- Client ID: `9d1c250a-e61b-44d9-88ed-5944d1962f5e` (hardcoded in binary)
- Grant type: `refresh_token`
- Refresh tokens rotate on each use — racing two refreshes invalidates one

Credentials are stored in `~/.claude/.credentials.json` as:
```json
{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-...",
    "refreshToken": "sk-ant-ort01-...",
    "expiresAt": 1234567890000
  }
}
```

## Code generation

After modifying any `*_types.go` file, always run:

```bash
make generate   # regenerates zz_generated.deepcopy.go
make manifests  # regenerates CRD YAML and RBAC role.yaml
```

After modifying RBAC marker comments (`// +kubebuilder:rbac:...`) in controllers, run `make manifests`.

Never manually edit `zz_generated.deepcopy.go`, `config/crd/bases/`, or `config/rbac/role.yaml`.

## Build commands

```bash
make generate          # Regenerate deepcopy methods
make manifests         # Regenerate CRD + RBAC manifests
go build ./...         # Build operator
go vet ./...           # Vet

make install           # Install CRDs into current cluster
make run               # Run operator locally against current kubeconfig

make docker-build IMG=<registry>/agentic-operator:tag   # Build operator image
make docker-push  IMG=<registry>/agentic-operator:tag

make claude-build CLAUDE_IMG=<registry>/claude-code:latest   # Build claude pod image
make claude-push  CLAUDE_IMG=<registry>/claude-code:latest
make claude-buildx CLAUDE_IMG=<registry>/claude-code:latest  # Multi-platform

make deploy   IMG=<registry>/agentic-operator:tag   # Deploy to cluster
make undeploy
```

## controller-gen version

The project requires controller-gen `v0.17.3` or later. The scaffolded `v0.16.1` is incompatible with Go 1.25+. This is already pinned in the Makefile:

```makefile
CONTROLLER_TOOLS_VERSION ?= v0.17.3
```

Do not downgrade this.

## Token refresher opt-in

Only Secrets explicitly labelled `agentic.swrm.io/token-refresh=enabled` are touched by the refresher. If the credentials key inside the Secret is not `credentials.json`, add the annotation `agentic.swrm.io/credentials-key=<key>`.

## AI maintenance note

This project is AI-generated and AI-maintained. Implementation decisions are made by Claude Code with human oversight at a high level. See README.md for details.
