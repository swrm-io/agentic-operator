# `hack/setup-credentials.sh` design

## Problem

Setting up OAuth credentials for `ClaudeJob`/`ClaudeSession` today is a manual,
multi-step process (documented in README.md):

1. Run `claude login` on your own machine.
2. Extract the organization UUID from `~/.claude.json`.
3. `kubectl create secret generic claude-credentials --from-file=... --from-literal=organizationId=...`
4. Label the secret `agentic.swrm.io/token-refresh=enabled`.
5. Run `claude logout` locally afterward — required because the OAuth refresh
   token rotates on every use, and if the user's local Claude Code and the
   operator's `TokenRefresher` both hold the same refresh token, one refresh
   invalidates the other.

This is error-prone (easy to skip the logout step, mistype the secret
structure, or forget the label) and it borrows the user's real local
`~/.claude` session, which they may not want touched. It is also the exact
manual recovery path needed after the busy-loop/`invalid_grant` bug fixed
earlier in this project — there's no tooling to make that recovery easy.

## Goals

- One command that performs the full login → extract → create/update Secret →
  label flow.
- Never touch the user's real `~/.claude` or `~/.claude.json` — login happens
  in an isolated sandbox.
- Works both for first-time setup (Secret doesn't exist) and rotation/recovery
  (Secret already exists, e.g. after a refresh token has died).
- Fail fast, before asking the user to go through an interactive browser
  login, if the target cluster/namespace isn't reachable.

## Non-goals

- Does not touch `ClaudeSession`'s Remote Control setup beyond producing the
  same Secret shape it already consumes — no controller changes.
- Does not replace API-key auth setup (`anthropic-api-key` Secret) — OAuth
  only, matching this script's purpose.
- Does not attempt the `CLAUDE_CODE_ORGANIZATION_UUID` env var simplification
  documented as a separate follow-up (project memory
  `claude_code_org_uuid_env_var.md`) — that's a controller-side change,
  unrelated to this script.

## Design

### Isolation mechanism

`CLAUDE_CONFIG_DIR` is an env var read by the `claude` CLI (confirmed by
running it directly in a temp directory — not documented in Anthropic's public
docs, discovered the same way other OAuth internals in this repo were:
inspecting the binary, consistent with the existing "OAuth token internals"
precedent in CLAUDE.md). Setting it redirects **both** `.credentials.json` and
`.claude.json` into the given directory; it does not require also overriding
`$HOME`. Verified empirically:

```bash
TESTDIR=$(mktemp -d)
CLAUDE_CONFIG_DIR="$TESTDIR" claude auth login
# writes $TESTDIR/.credentials.json and $TESTDIR/.claude.json
# does NOT touch $HOME/.claude or $HOME/.claude.json
```

The script uses this to run the login fully sandboxed, then deletes the
directory. Since the sandboxed credential is discarded immediately after being
read (never reused, never left on disk), there is no refresh-token race to
guard against and no `claude logout` step is needed — unlike the manual flow,
which reuses the user's persistent local session.

### Flow

1. **Preflight.**
   - `command -v claude` and `command -v kubectl`; exit with a clear message
     naming whichever is missing.
   - `kubectl auth can-i create secrets -n <namespace>` (or equivalent) to
     confirm the cluster is reachable and the user has permission, before
     starting the interactive login.
2. **Sandbox setup.** `mktemp -d`; register `trap 'rm -rf "$TMPDIR"' EXIT` so
   the directory is always cleaned up, including on failure or Ctrl-C.
3. **Login.** Run `CLAUDE_CONFIG_DIR="$TMPDIR" claude auth login`, inheriting
   stdin/stdout/stderr so the user sees the browser URL and any prompts.
4. **Extract.**
   - Read `$TMPDIR/.credentials.json` — this is the full `claudeAiOauth`
     object (`accessToken`, `refreshToken`, `expiresAt`, etc.), written
     verbatim into the Secret's credentials key (matches
     `TokenRefresher`'s expected shape in `tokenrefresher.go`).
   - Read `$TMPDIR/.claude.json`, field `.oauthAccount.organizationUuid`, for
     the `organizationId` Secret key.
   - If either file is missing or fails to parse, exit non-zero with a clear
     error (e.g. login was cancelled).
5. **Create or update the Secret.**
   - If the named Secret does not exist: `kubectl create secret generic`
     with `--from-file=<key>=<credentials file>` and
     `--from-literal=organizationId=<uuid>`.
   - If it already exists: patch its `data` in place (e.g.
     `kubectl create secret ... --dry-run=client -o yaml | kubectl apply -f -`,
     or `kubectl patch`) so re-running the script rotates an existing
     credential — this is the recovery path for a dead refresh token.
   - Apply the label `agentic.swrm.io/token-refresh=enabled` in both cases.
6. **Report.** Print the Secret's namespace/name/key and a note that it is
   labelled for automatic refresh by the operator's `TokenRefresher` — no
   further manual steps needed.

### Flags

| Flag | Default | Purpose |
|---|---|---|
| `--namespace` | `agentic-operator-system` | Target namespace for the Secret |
| `--secret-name` | `claude-credentials` | Secret name |
| `--key` | `credentials.json` | Data key for the credentials JSON |

Matches the defaults already used in README.md's manual instructions and the
live cluster's existing `claude-credentials` Secret.

### Error handling

- Any failed step (login cancelled, missing/unparseable files, `kubectl`
  failure) exits non-zero with a message identifying what failed.
- The `trap`-based cleanup runs regardless of how the script exits, so the
  sandboxed credential is never left behind on disk.

### Testing

Shell script — no Go test suite applies. Verification is manual/functional:
run the script end-to-end against a real (or `kind`/test) cluster and confirm:
- First run with no existing Secret creates it correctly, labelled, with both
  keys populated.
- Second run against an existing Secret updates it in place (rotation path).
- Missing `claude`/`kubectl`, and an unreachable cluster, each fail before the
  login step with a clear message.
- The sandbox directory does not survive the script (check `/tmp` afterward)
  and the user's real `~/.claude`/`~/.claude.json` are unmodified (mtime
  check before/after).

## Open questions / follow-ups

- Whether `kubectl create secret ... | kubectl apply -f -` or `kubectl patch`
  is the cleaner update mechanism is an implementation detail, not a design
  decision — resolve during implementation, matching existing shell
  conventions in the repo if any exist.
