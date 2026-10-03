#!/usr/bin/env bash
#
# Create/refresh the Secret multica-code-notifier needs. It is not committed:
# it holds the Telegram bot token. Re-running rotates it in place.
#
#   ./deploy/secrets.sh
set -euo pipefail

NAMESPACE="${NAMESPACE:-multica}"
CONTEXT="${CONTEXT:-$(kubectl config current-context)}"

# NOTE: no registry pull secret is created. The image is published to a PUBLIC
# ghcr.io package, so the kubelet pulls it anonymously. Note that testing this by
# curling /v2/<repo>/manifests/<tag> directly returns 401 whatever the package
# visibility -- that is the pre-auth WWW-Authenticate challenge every registry
# returns. To check visibility properly, exchange an anonymous token first:
#
#   t=$(curl -s "https://ghcr.io/token?service=ghcr.io&scope=repository:<owner>/<repo>:pull" | jq -r .token)
#   curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $t" \
#     -H "Accept: application/vnd.oci.image.index.v1+json" \
#     "https://ghcr.io/v2/<owner>/<repo>/manifests/<tag>"     # 200 = public

# The image itself needs no credentials: it is pulled anonymously from a public
# package.
#
# --- Telegram credentials ----------------------------------------------
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
