# multica-code-notifier

Relays [multica](https://github.com/multica-ai/multica) login codes to Telegram.

Multica's self-hosted login emails a six-digit code. Without an email provider
configured, that code only exists in the multica backend's pod log, so every
login needs a `kubectl logs`. This listens to the `verification_code` table
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

Runs in the `multica` namespace on the cluster, next to multica itself.

```bash
./deploy/secrets.sh                 # ghcr pull secret + Telegram creds
kubectl --context <cluster> apply -f deploy/20-deployment.yaml
```

`deploy/secrets.sh` reads the Telegram credentials out of the existing
`notifications/telegram` Secret so there is one bot token in the
cluster rather than two copies. Override with `TELEGRAM_BOT_TOKEN` /
`TELEGRAM_CHAT_ID` / `TELEGRAM_SOURCE_NAMESPACE`.

Verify — a real end-to-end run:

```bash
curl -s -X POST https://multica-api.internal.example.com/auth/send-code \
  -H 'Content-Type: application/json' -d '{"email":"you@example.com"}'

kubectl --context <cluster> logs -n multica deploy/multica-code-notifier -f
# msg="new verification code" email=...
# msg="code delivered" email=...
```

## Build

[ko](https://ko.build), not a Dockerfile: it cross-compiles a static binary and
produces a multi-arch index (`linux/amd64` for the `micro` node, `linux/arm64`
for the Pis) with an SPDX SBOM in one step.

```bash
KO_DOCKER_REPO=ghcr.io/loafoe ko build --sbom --bare --platform=linux/arm64,linux/amd64 .
```

Releases go through `.github/workflows/release.yaml` (push a `v*` tag), which
also **cosign-signs keylessly** via GitHub OIDC — no signing key exists on any
machine, and the job verifies its own signature before it finishes:

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/loafoe/multica-code-notifier/' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/loafoe/multica-code-notifier:v0.1.0
```

## Configuration

| Env | Default | Notes |
|---|---|---|
| `PGHOST` | `multica-db-rw.multica.svc.cluster.local` | CNPG read-write Service |
| `PGPORT` / `PGDATABASE` / `PGUSER` | `5432` / `multica` / `multica` | |
| `PGPASSWORD` | *(required)* | read live from the `multica-db-app` Secret |
| `PGSSLMODE` | `disable` | CNPG's cert is self-signed and in-cluster |
| `PG_SCHEMA` | `public` | |
| `TELEGRAM_BOT_TOKEN` | *(required)* | |
| `TELEGRAM_CHAT_ID` | *(required)* | |
| `TELEGRAM_API_URL` | `https://api.telegram.org` | override to point at a Bot API proxy |
| `NOTIFY_EMAIL_ALLOWLIST` | *(empty = all)* | comma-separated; set once others have accounts |

Flags: `-show-config` (redacted), `-setup-only` (install the trigger and exit),
`-send-test "<text>"` (send a message without touching the database), `-table`,
`-channel`.

## Design notes / sharp edges

**Replicas must stay 1.** LISTEN/NOTIFY delivers each notification to *every*
listening session, so a second replica delivers a duplicate of every code to the
same chat. `strategy: Recreate` for the same reason — a RollingUpdate briefly
runs two pods. Scaling out needs dedup (an advisory lock keyed on the
`verification_code` row id) that has not been written.

**The trigger is installed by the program, not by a migration.** On startup it
runs `CREATE OR REPLACE FUNCTION` + `DROP TRIGGER IF EXISTS`/`CREATE TRIGGER`
in one transaction, guarded by a `pg_try_advisory_xact_lock` so replicas racing
on startup cannot interleave. LISTEN is issued *after* the trigger exists, so
the first code requested post-startup is already covered. A code inserted while
the pod was down is not replayed — LISTEN is not durable — but the login page
will mint another on request.

**The channel name is interpolated into DDL** and so is validated against
`^[a-z_][a-z0-9_]*$` before use; the table and schema go through
`pgx.Identifier.Sanitize()`. There is no injection path.

**Reconnect is the whole program.** LISTEN is bound to one connection and
`multica-db` is a CNPG Cluster that can be restarted or switched over at any
time. Every session end triggers a full-jitter exponential backoff up to 30s,
then re-connects, re-checks the trigger and re-LISTENs.

**Delivery is at-most-once.** Telegram delivery is synchronous; if the send
fails the code is dropped and only logged. The row is still in the table, so a
new code can be requested.

**Telegram errors.** A 400/401/403 (bad token, blocked bot, unknown chat) fails
fast instead of retrying; 429 honours `retry_after`; transient 5xx retries with
backoff. The bot token is never logged — `-show-config` masks it.

**No ServiceAccount token.** `automountServiceAccountToken: false`: the program
never talks to the Kubernetes API, so there is nothing for a compromised pod to
escalate with.
