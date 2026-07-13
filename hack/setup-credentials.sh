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

if ! kubectl auth can-i create secrets -n "${NAMESPACE}" >/dev/null 2>&1; then
  echo "Error: cannot create Secrets in namespace '${NAMESPACE}'." >&2
  echo "Check that kubectl is configured for the right cluster and you have permission (kubectl auth can-i create secrets -n ${NAMESPACE})." >&2
  exit 1
fi

echo "Preflight checks passed (claude, kubectl, jq found; can create Secrets in '${NAMESPACE}')."

TMPDIR="$(mktemp -d "${TMPDIR:-/tmp}/claude-credentials-setup.XXXXXX")"
cleanup() {
  rm -rf "${TMPDIR}"
}
trap cleanup EXIT

echo "Starting Claude login in an isolated sandbox (your real ~/.claude is not touched)..."
if ! CLAUDE_CONFIG_DIR="${TMPDIR}" claude auth login; then
  echo "Error: 'claude auth login' failed or was cancelled." >&2
  exit 1
fi

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

echo ""
echo "Done. Secret '${SECRET_NAME}' in namespace '${NAMESPACE}' is ready"
echo "(key: ${SECRET_KEY}, plus organizationId)."
echo "It is labelled agentic.swrm.io/token-refresh=enabled, so the operator's"
echo "TokenRefresher will keep it up to date automatically — no further"
echo "manual steps needed."
