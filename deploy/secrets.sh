#!/usr/bin/env bash
#
# Create/refresh the two Secrets multica-code-notifier needs. Neither is
# committed: one is a registry credential, the other is the Telegram bot token.
# Both are idempotent -- re-running rotates them in place.
#
#   ./deploy/secrets.sh
set -euo pipefail

CONTEXT="${CONTEXT:-<cluster>}"
NAMESPACE=multica

# --- 1. ghcr pull secret ---------------------------------------------------
# The image lives under ghcr.io/loafoe, which is private, and this cluster has
# no default registry credential (nodes only have whatever was cached at image
# build time -- `containerd ctr images pull` on an uncached node asks for interactive
# auth and fails under a Deployment).
echo "==> ghcr pull secret (from gh auth token)"
kubectl --context "$CONTEXT" -n "$NAMESPACE" create secret docker-registry ghcr \
  --docker-server=ghcr.io \
  --docker-username="${GHCR_USERNAME:-$(gh api user --jq .login)}" \
  --docker-password="$(gh auth token)" \
  --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f -

# --- 2. Telegram credentials ----------------------------------------------
# Sourced from the existing notifications Secret so there is exactly one bot
# token and one chat id in the cluster, rather than a second copy drifting.
#
# Set TELEGRAM_BOT_TOKEN / TELEGRAM_CHAT_ID in the environment to override, e.g.
# when moving off the operator's personal bot onto a dedicated one.
echo "==> Telegram credentials"
SOURCE_NS="${TELEGRAM_SOURCE_NAMESPACE:-notifications}"
SOURCE_SECRET="${TELEGRAM_SOURCE_SECRET:-telegram}"

if [ -z "${TELEGRAM_BOT_TOKEN:-}" ]; then
  TELEGRAM_BOT_TOKEN=$(kubectl --context "$CONTEXT" -n "$SOURCE_NS" \
    get secret "$SOURCE_SECRET" -o jsonpath='{.data.bot_token}' | base64 -d)
fi
if [ -z "${TELEGRAM_CHAT_ID:-}" ]; then
  TELEGRAM_CHAT_ID=$(kubectl --context "$CONTEXT" -n "$SOURCE_NS" \
    get secret "$SOURCE_SECRET" -o jsonpath='{.data.chat_id}' | base64 -d)
fi

kubectl --context "$CONTEXT" -n "$NAMESPACE" create secret generic telegram \
  --from-literal=bot_token="$TELEGRAM_BOT_TOKEN" \
  --from-literal=chat_id="$TELEGRAM_CHAT_ID" \
  --dry-run=client -o yaml | kubectl --context "$CONTEXT" apply -f -

unset TELEGRAM_BOT_TOKEN TELEGRAM_CHAT_ID
echo "done (bot token read from $SOURCE_NS/$SOURCE_SECRET)"
