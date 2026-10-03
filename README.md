# multica-code-notifier

Relays [multica](https://github.com/multica-ai/multica) login codes to Telegram.

Multica's self-hosted login emails a six-digit code. With no email provider
configured, that code only exists in the multica backend's pod log — so every
login needs a `kubectl logs`. This watches the `verification_code` table
directly and messages the code to a Telegram chat instead.

```
POST /auth/send-code ──▶ multica backend ──▶ INSERT verification_code
                                                    │
                              AFTER INSERT trigger ──┴─▶ pg_notify('multica_verification_code', {email, code, expires_at})
                                                            │
                                       LISTEN ──▶ multica-code-notifier ──▶ Bot API ──▶ you
```

It talks to Postgres rather than the multica API on purpose: the code never has
to be read out of a pod log, and nothing breaks when the multica Deployment is
rescheduled.

## Deploy

Runs as a single Deployment next to multica. It needs no Service, no Ingress
and no Kubernetes API access.

```bash
./deploy/secrets.sh                       # registry pull secret + Telegram credentials
kubectl apply -f deploy/20-deployment.yaml
```

`deploy/secrets.sh` needs a GitHub token (`gh auth token`) for the registry
secret, and Telegram credentials from one of:

| Source | How |
|---|---|
| A Secret already in the cluster | `TELEGRAM_SOURCE_NAMESPACE=<ns> TELEGRAM_SOURCE_SECRET=<name> ./deploy/secrets.sh` |
| Values you supply yourself | `TELEGRAM_BOT_TOKEN=<token> TELEGRAM_CHAT_ID=<id> ./deploy/secrets.sh` |

Adjust `PGHOST` and the `multica-db-app` secret reference in
`deploy/20-deployment.yaml` to match your database.

Verify with a real end-to-end run — ask the login page for a code and watch it
arrive:

```bash
curl -s -X POST "$MULTICA_API/auth/send-code" \
  -H 'Content-Type: application/json' -d '{"email":"you@example.com"}'

kubectl logs -n multica deploy/multica-code-notifier -f
# msg="new verification code" email=...
# msg="code delivered" email=...
```

## Build

[ko](https://ko.build), not a Dockerfile: it cross-compiles a static binary and
publishes a multi-arch index (`linux/amd64` and `linux/arm64`) with an SPDX
SBOM in a single step.

```bash
KO_DOCKER_REPO=ghcr.io ko build --sbom --platform=linux/arm64,linux/amd64 .
```

Releases go through `.github/workflows/release.yaml` (push a `v*` tag), which
also **cosign-signs keylessly** via GitHub OIDC — there is no long-lived signing
key on any machine or in the repository — and verifies its own signature before
reporting success:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/<owner>/multica-code-notifier/' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/<owner>/multica-code-notifier:v0.1.0
```

## Configuration

| Env | Default | Notes |
|---|---|---|
| `PGHOST` | `postgres` | e.g. `<cluster>-rw.<ns>.svc.cluster.local` for CloudNativePG |
| `PGPORT` / `PGDATABASE` / `PGUSER` | `5432` / `multica` / `multica` | |
| `PGPASSWORD` | *(required)* | wire from the Secret the database operator owns |
| `PGSSLMODE` | `disable` | set `require` + a CA bundle for a managed database |
| `PG_SCHEMA` | `public` | |
| `TELEGRAM_BOT_TOKEN` | *(required)* | |
| `TELEGRAM_CHAT_ID` | *(required)* | |
| `TELEGRAM_API_URL` | `https://api.telegram.org` | override to point at a Bot API proxy |
| `NOTIFY_EMAIL_ALLOWLIST` | *(empty = all)* | comma-separated; set once other accounts exist |

Flags: `-show-config` (redacted), `-setup-only` (install the trigger and exit),
`-send-test "<text>"` (send a message without touching the database), `-table`,
`-channel`.

## Design notes / sharp edges

**Replicas must stay 1.** LISTEN/NOTIFY delivers each notification to *every*
listening session, so a second replica delivers a duplicate of every code to the
same chat. `strategy: Recreate` is set for the same reason — a RollingUpdate
briefly runs two pods. Scaling out needs deduplication (an advisory lock keyed
on the `verification_code` row id), which is not implemented; do not simply
raise `replicas`.

**The trigger is installed by the program, not by a migration.** On startup it
runs `CREATE OR REPLACE FUNCTION` plus `DROP TRIGGER IF EXISTS`/`CREATE TRIGGER`
in a single transaction, guarded by `pg_try_advisory_xact_lock` so replicas
racing at startup cannot interleave. LISTEN is issued *after* the trigger
exists, so the first code requested post-startup is already covered. A code
inserted while the pod was down is not replayed — LISTEN is not durable — but
the login page will mint another on request.

**The channel name is interpolated into DDL** and so is validated against
`^[a-z_][a-z0-9_]*$` before use; the table and schema go through
`pgx.Identifier.Sanitize()`. There is no injection path.

**Reconnect is the whole program.** LISTEN is bound to one connection, and the
database can be restarted, rescheduled or failed over at any time. Every session
end triggers a full-jitter exponential backoff up to 30s, then re-connects,
re-checks the trigger and re-LISTENs.

**Delivery is at-most-once.** Telegram delivery is synchronous; if a send fails
the code is dropped and only logged. The row is still in the table, so a new
code can be requested.

**Telegram errors.** A 400/401/403 (bad token, blocked bot, unknown chat) fails
fast instead of retrying; 429 honours `retry_after`; transient 5xx retries with
backoff. The bot token is never logged — `-show-config` masks it.

**No ServiceAccount token.** `automountServiceAccountToken: false`: the program
never talks to the Kubernetes API, so there is nothing for a compromised pod to
escalate with.
