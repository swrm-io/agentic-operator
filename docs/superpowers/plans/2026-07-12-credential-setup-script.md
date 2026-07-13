# hack/setup-credentials.sh Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `hack/setup-credentials.sh`, a standalone shell script that automates OAuth credential setup for `ClaudeJob`/`ClaudeSession` — replacing the manual `claude login` → extract → `kubectl create secret` → label flow documented in README.md — using a sandboxed `CLAUDE_CONFIG_DIR` so it never touches the user's real `~/.claude` session.

**Architecture:** A single bash script with: (1) a preflight check block, (2) a sandboxed login step using `mktemp -d` + `CLAUDE_CONFIG_DIR`, (3) a `jq`-based extraction step reading the sandboxed `.credentials.json`/`.claude.json`, (4) a create-or-update `kubectl` step, (5) a trap-based cleanup. No Go code is touched — this is purely a new shell script plus a README update pointing to it.

**Tech Stack:** bash, `claude` CLI (`auth login` subcommand), `kubectl`, `jq`.

## Global Constraints

- Script must never write to the user's real `$HOME`, `~/.claude`, or `~/.claude.json` — verified in the design spec that `CLAUDE_CONFIG_DIR` alone (no `$HOME` override) is sufficient.
- No `claude auth logout` step — the sandboxed credential is discarded by deleting its directory, not by logging out.
- Preflight (tool checks + cluster reachability) must run and fail fast **before** the interactive login step.
- Default namespace: `agentic-operator-system`. Default secret name: `claude-credentials`. Default key: `credentials.json`. All three overridable via flags (`--namespace`, `--secret-name`, `--key`).
- Must support create-or-update: if the named Secret already exists, update its data in place rather than erroring.
- Cleanup (`rm -rf` of the sandbox tmpdir) must happen on any exit path (success, failure, or interrupt) via `trap ... EXIT`.
- Use `jq` for JSON parsing (not `python3`), per user decision — this introduces a new dependency, so it must be checked in preflight alongside `claude` and `kubectl`.

---

## File Structure

- Create: `hack/setup-credentials.sh` — the entire script. Single file; this is a small, single-purpose tool and splitting it would add indirection without benefit (YAGNI).
- Modify: `README.md` — add a short section pointing to the script as the recommended path, keeping the existing manual steps as a documented fallback/explanation of what the script automates.

No Go files are touched. No `make generate`/`make manifests` needed (no `*_types.go` changes).

---

### Task 1: Preflight checks

**Files:**
- Create: `hack/setup-credentials.sh`

**Interfaces:**
- Produces: a script invocable as `hack/setup-credentials.sh [--namespace NS] [--secret-name NAME] [--key KEY]`, which at this task's end only performs preflight checks and argument parsing, then exits 0 (later tasks add the real behavior after preflight passes).

- [ ] **Step 1: Create the script file with argument parsing and defaults**

```bash
#!/usr/bin/env bash
set -euo pipefail

NAMESPACE="agentic-operator-system"
SECRET_NAME="claude-credentials"
SECRET_KEY="credentials.json"

usage() {
  cat <<EOF
Usage: $(basename "$0") [--namespace NS] [--secret-name NAME] [--key KEY]

Automates OAuth credential setup for ClaudeJob/ClaudeSession: runs
'claude auth login' in an isolated sandbox, then creates or updates the
named Kubernetes Secret with the resulting credentials.

  --namespace NS      Target namespace (default: ${NAMESPACE})
  --secret-name NAME  Secret name (default: ${SECRET_NAME})
  --key KEY           Data key for the credentials JSON (default: ${SECRET_KEY})
  -h, --help          Show this help
EOF
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace)
      NAMESPACE="$2"
      shift 2
      ;;
    --secret-name)
      SECRET_NAME="$2"
      shift 2
      ;;
    --key)
      SECRET_KEY="$2"
      shift 2
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown argument: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done
```

- [ ] **Step 2: Add tool-presence preflight checks**

Append to `hack/setup-credentials.sh`:

```bash
require_command() {
  local cmd="$1"
  local hint="$2"
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "Error: '${cmd}' is required but not found on PATH. ${hint}" >&2
    exit 1
  fi
}

require_command claude "Install it from https://code.claude.com/docs/en/quickstart."
require_command kubectl "Install it from https://kubernetes.io/docs/tasks/tools/."
require_command jq "Install it from https://jqlang.org/download/."
```

- [ ] **Step 3: Add cluster-reachability preflight check**

Append to `hack/setup-credentials.sh`:

```bash
if ! kubectl auth can-i create secrets -n "${NAMESPACE}" >/dev/null 2>&1; then
  echo "Error: cannot create Secrets in namespace '${NAMESPACE}'." >&2
  echo "Check that kubectl is configured for the right cluster and you have permission (kubectl auth can-i create secrets -n ${NAMESPACE})." >&2
  exit 1
fi

echo "Preflight checks passed (claude, kubectl, jq found; can create Secrets in '${NAMESPACE}')."
```

- [ ] **Step 4: Make the script executable and verify preflight behavior manually**

Run:
```bash
chmod +x hack/setup-credentials.sh
./hack/setup-credentials.sh --help
```
Expected: usage text printed, exit 0.

Run:
```bash
./hack/setup-credentials.sh --namespace this-namespace-should-not-exist-xyz
```
Expected: fails with the "cannot create Secrets in namespace" error, exit 1 — confirms the check runs and correctly rejects an inaccessible namespace before any login step (there is no login step yet at this task, but this confirms the gate itself works).

Run (against the real, accessible namespace from earlier in this session):
```bash
./hack/setup-credentials.sh --namespace agentic-operator-system
```
Expected: prints "Preflight checks passed..." and exits 0.

- [ ] **Step 5: Commit**

```bash
git add hack/setup-credentials.sh
git commit -m "$(cat <<'EOF'
Add preflight checks for credential setup script

First step of hack/setup-credentials.sh: verifies claude, kubectl, and
jq are available and that the target namespace is reachable, before
any interactive login step is added.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 2: Sandboxed login and cleanup trap

**Files:**
- Modify: `hack/setup-credentials.sh` (append after the preflight block from Task 1)

**Interfaces:**
- Consumes: nothing new from Task 1 beyond the parsed `NAMESPACE`/`SECRET_NAME`/`SECRET_KEY` variables and the fact that preflight has already passed by this point in the script.
- Produces: a `TMPDIR` variable holding the sandbox directory path, guaranteed removed on any exit (via `trap`), and a completed `claude auth login` run inside it. Later tasks read `"${TMPDIR}/.credentials.json"` and `"${TMPDIR}/.claude.json"`.

- [ ] **Step 1: Add the sandbox directory and cleanup trap**

Append to `hack/setup-credentials.sh`:

```bash
TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/claude-credentials-setup.XXXXXX")"
cleanup() {
  rm -rf "${TMPDIR}"
}
trap cleanup EXIT
```

- [ ] **Step 2: Run the sandboxed login**

Append to `hack/setup-credentials.sh`:

```bash
echo "Starting Claude login in an isolated sandbox (your real ~/.claude is not touched)..."
if ! CLAUDE_CONFIG_DIR="${TMPDIR}" claude auth login; then
  echo "Error: 'claude auth login' failed or was cancelled." >&2
  exit 1
fi
```

- [ ] **Step 3: Verify the trap fires on both normal exit and interrupt**

Run:
```bash
bash -c '
TMPDIR="$(mktemp -d /tmp/trap-test.XXXXXX)"
trap "rm -rf \"$TMPDIR\"" EXIT
echo "dir exists: $(test -d "$TMPDIR" && echo yes)"
exit 0
'
```
Expected: prints "dir exists: yes", and the directory is gone after the script exits (this validates the trap pattern in isolation before relying on it in the real script — the real script's login step requires interactive input so it can't be fully automated in this verification step).

Run the real script up through this point manually (this will prompt for a real login — use your own Claude account):
```bash
./hack/setup-credentials.sh --namespace agentic-operator-system
```
Expected: preflight passes, then the browser-login prompt from `claude auth login` appears. Complete the login. After the script finishes (it will error at the next unimplemented step, or exit 0 if this is the last step so far), confirm the sandbox directory no longer exists:
```bash
ls /tmp/claude-credentials-setup.* 2>&1
```
Expected: "No such file or directory" (glob doesn't match anything).

Also confirm the real local session was untouched:
```bash
stat -c '%Y' "$HOME/.claude.json"
```
Expected: mtime unchanged from before running the script.

- [ ] **Step 4: Commit**

```bash
git add hack/setup-credentials.sh
git commit -m "$(cat <<'EOF'
Add sandboxed claude auth login step

Runs 'claude auth login' with CLAUDE_CONFIG_DIR pointed at a temp
directory so it never touches the user's real ~/.claude session. The
temp directory is removed via a trap on any exit path.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 3: Extract credentials and organization UUID

**Files:**
- Modify: `hack/setup-credentials.sh` (append after Task 2's login step)

**Interfaces:**
- Consumes: `TMPDIR` from Task 2 (contains `.credentials.json` and `.claude.json` after a successful login).
- Produces: two shell variables used by Task 4 — `CREDENTIALS_FILE` (path to the credentials JSON to hand to `kubectl --from-file`) and `ORG_ID` (the extracted `organizationUuid` string).

- [ ] **Step 1: Extract and validate the credentials file**

Append to `hack/setup-credentials.sh`:

```bash
CREDENTIALS_FILE="${TMPDIR}/.credentials.json"
CLAUDE_JSON_FILE="${TMPDIR}/.claude.json"

if [[ ! -f "${CREDENTIALS_FILE}" ]]; then
  echo "Error: expected credentials file not found at ${CREDENTIALS_FILE} after login." >&2
  exit 1
fi

if ! jq -e '.claudeAiOauth.accessToken' "${CREDENTIALS_FILE}" >/dev/null 2>&1; then
  echo "Error: ${CREDENTIALS_FILE} does not contain the expected claudeAiOauth.accessToken field." >&2
  exit 1
fi
```

- [ ] **Step 2: Extract and validate the organization UUID**

Append to `hack/setup-credentials.sh`:

```bash
if [[ ! -f "${CLAUDE_JSON_FILE}" ]]; then
  echo "Error: expected config file not found at ${CLAUDE_JSON_FILE} after login." >&2
  exit 1
fi

ORG_ID="$(jq -r '.oauthAccount.organizationUuid // empty' "${CLAUDE_JSON_FILE}")"
if [[ -z "${ORG_ID}" ]]; then
  echo "Error: could not find oauthAccount.organizationUuid in ${CLAUDE_JSON_FILE}." >&2
  exit 1
fi

echo "Extracted credentials and organization UUID."
```

- [ ] **Step 3: Verify extraction against a known-good sample file**

Run this standalone check (does not require a real login — uses a synthetic fixture matching the real shape documented in CLAUDE.md's "OAuth token internals" section):

```bash
FIXTURE_DIR="$(mktemp -d)"
cat > "${FIXTURE_DIR}/.credentials.json" <<'EOF'
{
  "claudeAiOauth": {
    "accessToken": "sk-ant-oat01-test",
    "refreshToken": "sk-ant-ort01-test",
    "expiresAt": 1234567890000
  }
}
EOF
cat > "${FIXTURE_DIR}/.claude.json" <<'EOF'
{
  "oauthAccount": {
    "organizationUuid": "test-org-uuid-1234"
  }
}
EOF

jq -e '.claudeAiOauth.accessToken' "${FIXTURE_DIR}/.credentials.json"
jq -r '.oauthAccount.organizationUuid // empty' "${FIXTURE_DIR}/.claude.json"
rm -rf "${FIXTURE_DIR}"
```
Expected output:
```
"sk-ant-oat01-test"
test-org-uuid-1234
```
This confirms the `jq` filters used in Steps 1–2 correctly extract from the documented credential shape.

- [ ] **Step 4: Commit**

```bash
git add hack/setup-credentials.sh
git commit -m "$(cat <<'EOF'
Extract credentials and org UUID from sandboxed login

Reads accessToken/refreshToken/expiresAt from the sandboxed
.credentials.json and organizationUuid from .claude.json, validating
both are present before proceeding to write the Secret.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 4: Create-or-update the Kubernetes Secret

**Files:**
- Modify: `hack/setup-credentials.sh` (append after Task 3's extraction step)

**Interfaces:**
- Consumes: `NAMESPACE`, `SECRET_NAME`, `SECRET_KEY` from Task 1; `CREDENTIALS_FILE`, `ORG_ID` from Task 3.
- Produces: the final script behavior — a Secret named `${SECRET_NAME}` in `${NAMESPACE}` with keys `${SECRET_KEY}` (credentials JSON) and `organizationId` (the UUID), labelled `agentic.swrm.io/token-refresh=enabled`.

- [ ] **Step 1: Add create-or-update logic**

Append to `hack/setup-credentials.sh`:

```bash
if kubectl get secret "${SECRET_NAME}" -n "${NAMESPACE}" >/dev/null 2>&1; then
  echo "Secret '${SECRET_NAME}' already exists in '${NAMESPACE}' — updating it."
  kubectl create secret generic "${SECRET_NAME}" \
    --namespace "${NAMESPACE}" \
    --from-file="${SECRET_KEY}=${CREDENTIALS_FILE}" \
    --from-literal="organizationId=${ORG_ID}" \
    --dry-run=client -o yaml \
    | kubectl apply -f -
else
  echo "Creating Secret '${SECRET_NAME}' in '${NAMESPACE}'."
  kubectl create secret generic "${SECRET_NAME}" \
    --namespace "${NAMESPACE}" \
    --from-file="${SECRET_KEY}=${CREDENTIALS_FILE}" \
    --from-literal="organizationId=${ORG_ID}"
fi

kubectl label secret "${SECRET_NAME}" -n "${NAMESPACE}" \
  agentic.swrm.io/token-refresh=enabled --overwrite
```

- [ ] **Step 2: Add the final success report**

Append to `hack/setup-credentials.sh`:

```bash
echo ""
echo "Done. Secret '${SECRET_NAME}' in namespace '${NAMESPACE}' is ready"
echo "(key: ${SECRET_KEY}, plus organizationId)."
echo "It is labelled agentic.swrm.io/token-refresh=enabled, so the operator's"
echo "TokenRefresher will keep it up to date automatically — no further"
echo "manual steps needed."
```

- [ ] **Step 3: Run the full script end-to-end against the real cluster**

Run:
```bash
./hack/setup-credentials.sh --namespace agentic-operator-system --secret-name claude-credentials-test
```
Expected: preflight passes, interactive login prompt appears, complete it, then the script prints "Creating Secret 'claude-credentials-test'..." followed by the success report.

Verify the Secret directly:
```bash
kubectl get secret claude-credentials-test -n agentic-operator-system -o json \
  | jq '{labels: .metadata.labels, keys: (.data | keys)}'
```
Expected:
```json
{
  "labels": {
    "agentic.swrm.io/token-refresh": "enabled"
  },
  "keys": [
    "credentials.json",
    "organizationId"
  ]
}
```

- [ ] **Step 4: Verify re-running the script updates the existing Secret**

Run the same command again:
```bash
./hack/setup-credentials.sh --namespace agentic-operator-system --secret-name claude-credentials-test
```
Expected: preflight passes, login prompt appears again, then the script prints "Secret 'claude-credentials-test' already exists... — updating it." followed by the success report. Confirm via:
```bash
kubectl get secret claude-credentials-test -n agentic-operator-system -o json | jq '.data."credentials.json"' | base64 -d
```
Expected: valid JSON with a (likely different, since login was re-run) `accessToken`.

- [ ] **Step 5: Clean up the test secret**

Run:
```bash
kubectl delete secret claude-credentials-test -n agentic-operator-system
```

- [ ] **Step 6: Commit**

```bash
git add hack/setup-credentials.sh
git commit -m "$(cat <<'EOF'
Create or update the Kubernetes Secret from extracted credentials

Completes hack/setup-credentials.sh: writes the extracted credentials
and organization UUID into the target Secret (creating it if absent,
updating it in place if it already exists — the recovery path for a
dead refresh token), labels it for automatic refresh, and reports
success.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

### Task 5: Update README to reference the script

**Files:**
- Modify: `README.md` (the "OAuth (claude.ai login)" section, currently documenting the manual `claude login` → `kubectl create secret` → label → `claude logout` flow)

**Interfaces:**
- Consumes: nothing from prior tasks beyond the finished script's existence and its flags (`--namespace`, `--secret-name`, `--key`) to document accurately.
- Produces: updated documentation only — no behavior change.

- [ ] **Step 1: Read the current OAuth section**

Run:
```bash
grep -n "^### OAuth" -A 25 README.md
```
Note the exact line range to replace in the next step (do not guess — use what this command actually shows).

- [ ] **Step 2: Add a script-based quickstart above the existing manual steps**

Using `Edit`, insert immediately after the `### OAuth (claude.ai login)` heading line (before the existing manual `claude /login` instructions found in Step 1), the following paragraph and command block:

```markdown
The easiest way to set this up is `hack/setup-credentials.sh`, which runs
the login in an isolated sandbox (your real `~/.claude` is never touched)
and creates or updates the Secret for you:

```bash
hack/setup-credentials.sh
```

Run it again any time to rotate the credential — e.g. if the refresh token
has expired and the operator's logs show `invalid_grant` errors. It accepts
`--namespace`, `--secret-name`, and `--key` flags if you're not using the
defaults below.

Alternatively, to set it up manually:
```

Keep every existing line below this insertion (the `claude /login` block
through the `claude logout` warning) exactly as-is — the manual steps remain
as documented fallback/explanation.

- [ ] **Step 3: Verify the README renders sensibly**

Run:
```bash
grep -n "^### OAuth" -A 35 README.md
```
Expected: the new script-quickstart paragraph and command block appear first, immediately followed by "Alternatively, to set it up manually:" and then the pre-existing manual instructions, unchanged.

- [ ] **Step 4: Commit**

```bash
git add README.md
git commit -m "$(cat <<'EOF'
Document hack/setup-credentials.sh in README

Points users at the new automated credential-setup script as the
recommended path, keeping the manual steps documented as a fallback
and for understanding what the script automates.

Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
EOF
)"
```

---

## Plan Self-Review

**Spec coverage:**
- Preflight (tools + cluster reachability, before login) → Task 1. ✓
- Sandboxed login via `CLAUDE_CONFIG_DIR`, no `$HOME` override, no logout step → Task 2. ✓
- Extraction of credentials + org UUID → Task 3. ✓
- Create-or-update + label → Task 4. ✓
- Cleanup via trap on any exit path → Task 2, Step 1 (trap registered before login, so it covers every later task's exit too). ✓
- Flags with README-matching defaults → Task 1. ✓
- Final report of Secret name/namespace/key + refresh note → Task 4, Step 2. ✓
- README documentation → Task 5. ✓
- Non-goal (no `CLAUDE_CODE_ORGANIZATION_UUID` controller change) → correctly out of scope, not present in any task. ✓

**Placeholder scan:** No TBD/TODO markers; every step has literal code or exact commands with expected output.

**Type/naming consistency:** `NAMESPACE`, `SECRET_NAME`, `SECRET_KEY` (Task 1) are used identically in Tasks 2–4. `TMPDIR` (Task 2) is consumed by Task 3 to build `CREDENTIALS_FILE`/`CLAUDE_JSON_FILE`. `CREDENTIALS_FILE`/`ORG_ID` (Task 3) are consumed by Task 4 exactly as named. No renaming drift found.
