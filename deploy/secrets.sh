#!/usr/bin/env bash
#
# Create/refresh the two Secrets multica-code-notifier needs. Neither is
# committed: one is a registry credential, the other is the Telegram bot token.
# Both steps are idempotent -- re-running rotates them in place.
#
#   ./deploy/secrets.sh
set -euo pipefail

NAMESPACE="${NAMESPACE:-multica}"
CONTEXT="${CONTEXT:-$(kubectl config current-context)}"

# --- 1. Registry pull secret ----------------------------------------------
# The image lives under a private namespace on ghcr.io, and clusters commonly
# have no default registry credential: nodes only hold images cached at build
# time, so a cold node cannot pull it and the pod lands in ErrImagePull even
# though the repository exists.
echo "==> registry pull secret (from gh auth token)"
kubectl --context "$CONTEXT" -n "$NAMESPACE" create secret docker-registry ghcr \
  --docker-server="${REGISTRY:-ghcr.io}" \
  --docker-username="${REGISTRY_USERNAME:-$(gh api user --jq .login)}" \
  --docker-password="$(gh auth token)" \
  --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f -

# --- 2. Telegram credentials ----------------------------------------------
# Defaults to reading an existing bot token + chat id out of a Secret in the
# cluster, so a fleet shares one bot rather than accumulating near-duplicate
# copies that drift apart when the token is rotated.
#
# Set TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID to use values from somewhere else
# (a password manager, a sealed-secret store, your own bot) instead.
echo "==> Telegram credentials"
SOURCE_NS="${TELEGRAM_SOURCE_NAMESPACE:-}"
SOURCE_SECRET="${TELEGRAM_SOURCE_SECRET:-telegram}"

if [ -z "${TELEGRAM_BOT_TOKEN:-}" ]; then
  if [ -z "$SOURCE_NS" ]; then
    echo "error: set TELEGRAM_BOT_TOKEN, or TELEGRAM_SOURCE_NAMESPACE to copy from an existing Secret" >&2
    exit 1
  fi
  TELEGRAM_BOT_TOKEN=$(kubectl --context "$CONTEXT" -n "$SOURCE_NS" \
    get secret "$SOURCE_SECRET" -o jsonpath='{.data.bot_token}' | base64 -d)
fi
if [ -z "${TELEGRAM_CHAT_ID:-}" ]; then
  if [ -z "$SOURCE_NS" ]; then
    echo "error: set TELEGRAM_CHAT_ID, or TELEGRAM_SOURCE_NAMESPACE to copy from an existing Secret" >&2
    exit 1
  fi
  TELEGRAM_CHAT_ID=$(kubectl --context "$CONTEXT" -n "$SOURCE_NS" \
    get secret "$SOURCE_SECRET" -o jsonpath='{.data.chat_id}' | base64 -d)
fi

kubectl --context "$CONTEXT" -n "$NAMESPACE" create secret generic telegram \
  --from-literal=bot_token="$TELEGRAM_BOT_TOKEN" \
  --from-literal=chat_id="$TELEGRAM_CHAT_ID" \
  --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f -

unset TELEGRAM_BOT_TOKEN TELEGRAM_CHAT_ID
echo "done"
