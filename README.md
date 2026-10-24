# multica-code-notifier

Relays [multica](https://github.com/multica-ai/multica) login codes to Telegram.

Multica's self-hosted login emails a six-digit code. With no email provider
configured, that code only exists in the multica backend's pod log — so every
login needs a `kubectl logs`. This watches the `verification_code` table
directly and messages the code to the right person's Telegram chat.

```
POST /auth/send-code ──▶ multica backend ──▶ INSERT verification_code
                                                    │
                              AFTER INSERT trigger ──┴─▶ pg_notify('multica_verification_code', {email, code, expires_at})
                                                            │
                                       LISTEN ──▶ multica-code-notifier ──▶ email → chat map ──▶ Bot API ──▶ the right person
```

It talks to Postgres rather than the multica API on purpose: the code never has
to be read out of a pod log, and nothing breaks when the multica Deployment is
rescheduled. One bot serves a whole team: `deploy/10-configmap.yaml` maps each
address to its own Telegram chat (or several), and an address that is not in the
map is skipped.

## Deploy

Runs as a single Deployment next to multica. It needs no Service, no Ingress
and no Kubernetes API access.

```bash
cp deploy/10-configmap.example.yaml deploy/10-configmap.yaml
# edit deploy/10-configmap.yaml: replace the example address/chat id
kubectl apply -f deploy/10-configmap.yaml  # email -> chat id routing
./deploy/secrets.sh                        # Telegram bot token only
kubectl apply -f deploy/20-deployment.yaml
```

`deploy/10-configmap.example.yaml` is only a template. The filled-in
`deploy/10-configmap.yaml` is gitignored on purpose: real addresses and chat ids
are deployment-specific and should not be published. `deploy/secrets.sh` needs a
Telegram bot token from one of:

| Source | How |
|---|---|
| A Secret already in the cluster | `TELEGRAM_SOURCE_NAMESPACE=<ns> TELEGRAM_SOURCE_SECRET=<name> ./deploy/secrets.sh` |
| A value you supply yourself | `TELEGRAM_BOT_TOKEN=<token> ./deploy/secrets.sh` |

Adjust `PGHOST` and the `multica-db-app` secret reference in
`deploy/20-deployment.yaml` to match your database.

Verify the resolved routing without sending anything:

```bash
kubectl -n multica exec deploy/multica-code-notifier -- \
  /ko-app/multica-code-notifier -show-config
# recipients    1 address(es)
#   you@example.com    123456789
```

Then send a real message to one mapped chat (the binary lives at `/ko-app/`
because the image is built with `ko --bare`):

```bash
kubectl -n multica exec deploy/multica-code-notifier -- \
  /ko-app/multica-code-notifier -send-test "routing test" \
  -send-test-email you@example.com
```

Finally, an end-to-end run — ask the login page for a code and watch it arrive:

```bash
curl -s -X POST "$MULTICA_API/auth/send-code" \
  -H 'Content-Type: application/json' -d '{"email":"you@example.com"}'

kubectl logs -n multica deploy/multica-code-notifier -f
# msg="new verification code" email=...
# msg="code delivered" email=... chat_id=...
```

## Licence

Apache 2.0 — see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).

## Build

[ko](https://ko.build) cross-compiles a static binary and publishes a multi-arch
index (`linux/amd64` and `linux/arm64`) to ghcr.io. There is no Dockerfile.

```bash
KO_DOCKER_REPO=ghcr.io/<owner>/multica-code-notifier \
  ko build --sbom=none --bare --platform=linux/amd64,linux/arm64 --tags v0.2.0 .
```

Releases go through `.github/workflows/release.yaml`, which **cosign-signs
keylessly** via GitHub OIDC and verifies its own signature before reporting
success.

### Tags

| Trigger | Publishes | `latest` |
|---|---|---|
| push `v0.2.0` | `v0.2.0` | moved, if the version is stable |
| push `v0.2.0-rc.1` | `v0.2.0-rc.1` | untouched |
| push to `main` | `main` | untouched |

Tags are strict semver (`vMAJOR.MINOR.PATCH[-prerelease][+build]`, no leading
zeros); `v1.2` and `latest` are rejected. A published version tag is immutable,
and `latest` tracks the newest stable release only.

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/<owner>/multica-code-notifier/' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/<owner>/multica-code-notifier:v0.2.0
```

### The image is public

The package is published **public**, so the kubelet pulls it anonymously and the
Deployment needs no `imagePullSecrets`.

## Routing

The ConfigMap is mounted at `/etc/multica-code-notifier/chat-map.json`. It maps
a Multica email address to one Telegram chat id, or a list of them for an
address that should reach several people:

```json
{
  "you@example.com": "123456789",
  "team@example.com": ["111111", "222222"]
}
```

* Matching is case-insensitive; the map keys are normalised to lower case.
* Numeric ids may be JSON strings or bare numbers; group ids are negative.
* An address that is **not** in the map is skipped (logged, not delivered), so
  another user's code can never fall through to a default chat.
* A `TELEGRAM_CHAT_MAP` env var overrides the file, mainly for local runs.
* If neither the env var nor the file is present the process exits at startup
  rather than running with no recipients.

Keep real values out of git: copy `deploy/10-configmap.example.yaml` to the
gitignored `deploy/10-configmap.yaml` and edit that. Applying the example file
as-is points codes at a dummy chat.

After editing:

```bash
kubectl apply -f deploy/10-configmap.yaml
kubectl rollout restart -n multica deploy/multica-code-notifier
```

Chat ids are not credentials: without the bot token they cannot be used to send
anything. That is why routing lives in a ConfigMap while the token stays in
`Secret/telegram`.

## Configuration

| Env | Default | Notes |
|---|---|---|
| `PGHOST` | `postgres` | e.g. `<cluster>-rw.<ns>.svc.cluster.local` for CloudNativePG |
| `PGPORT` / `PGDATABASE` / `PGUSER` | `5432` / `multica` / `multica` | |
| `PGPASSWORD` | *(required)* | wire from the Secret the database operator owns |
| `PGSSLMODE` | `disable` | set `require` + a CA bundle for a managed database |
| `PG_SCHEMA` | `public` | |
| `TELEGRAM_BOT_TOKEN` | *(required)* | bot token; keep in a Secret |
| `TELEGRAM_CHAT_MAP_FILE` | `/etc/multica-code-notifier/chat-map.json` | mounted ConfigMap with the email -> chat id map |
| `TELEGRAM_CHAT_MAP` | *(empty)* | inline JSON override for the file (local dev) |
| `TELEGRAM_API_URL` | `https://api.telegram.org` | override to point at a Bot API proxy |

Flags: `-show-config` (redacted, prints every mapped address), `-setup-only`
(install the trigger and exit), `-send-test "<text>"` (send a message without
touching the database), `-send-test-email <address>` (required with `-send-test`
when more than one address is mapped), `-table`, `-channel`.

## Design notes / sharp edges

**Replicas must stay 1.** LISTEN/NOTIFY delivers each notification to *every*
listening session, so a second replica delivers a duplicate of every code to
each mapped chat. `strategy: Recreate` is set for the same reason — a
RollingUpdate briefly runs two pods. Scaling out needs deduplication (an
advisory lock keyed on the `verification_code` row id), which is not
implemented; do not simply raise `replicas`.

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

**Delivery is at-most-once per chat.** Telegram delivery is synchronous; if a
send fails for one recipient, that code is dropped for that chat and only
logged, while the remaining recipients still get it. The row is still in the
table, so a new code can be requested.

**Telegram errors.** A 400/401/403 (bad token, blocked bot, unknown chat) fails
fast instead of retrying; 429 honours `retry_after`; transient 5xx retries with
backoff. The bot token is never logged — `-show-config` masks it.

**No ServiceAccount token.** `automountServiceAccountToken: false`: the program
never talks to the Kubernetes API, so there is nothing for a compromised pod to
escalate with.
