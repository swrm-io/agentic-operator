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
