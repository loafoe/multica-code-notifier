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
./deploy/secrets.sh                       # Telegram credentials only
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

## Licence

Apache 2.0 — see [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).

## Build

[ko](https://ko.build), not a Dockerfile: it cross-compiles a static binary and
publishes a multi-arch index (`linux/amd64` and `linux/arm64`) with an SPDX
SBOM in a single step.

```bash
KO_DOCKER_REPO=ghcr.io/<owner>/multica-code-notifier \
  ko build --sbom-dir sbom --bare --platform=linux/arm64,linux/amd64 --tags v0.1.3 .
```

Releases go through `.github/workflows/release.yaml`, which **cosign-signs
keylessly** via GitHub OIDC — no signing key exists on any machine or in the
repository — and verifies its own signature before reporting success.

### Tags

| Trigger | Publishes | `latest` |
|---|---|---|
| push `v0.1.3` | `v0.1.3` | moved, if the version is stable |
| push `v0.2.0-rc.1` | `v0.2.0-rc.1` | untouched |
| push to `main` | `main` | untouched |

- Tags are validated as strict semver (`vMAJOR.MINOR.PATCH[-prerelease][+build]`,
  no leading zeros). `v1.2` and `latest` are rejected outright.
- **A published version is immutable.** Re-pushing an existing version fails the
  build: `git tag --force` makes accidental republishing trivial, and consumers
  pin tags. Fix forward with a new version instead.
- `main` is a development build and never masquerades as a version. `latest`
  tracks the newest **stable** release only, so it is not the moving target it
  would be if every commit updated it.

```bash
cosign verify \
  --certificate-identity-regexp '^https://github.com/<owner>/multica-code-notifier/' \
  --certificate-oidc-issuer 'https://token.actions.githubusercontent.com' \
  ghcr.io/<owner>/multica-code-notifier:v0.1.3
```

### The image is public — no credentials needed anywhere

The package is published **public**, so the kubelet pulls the image anonymously
and the Deployment needs no `imagePullSecrets`.

Check this properly, because the obvious test lies: requesting
`/v2/<repo>/manifests/<tag>` directly returns **401 whatever the visibility** —
that is the pre-auth `WWW-Authenticate` challenge every registry returns, and it
looks identical to "private". Exchange an anonymous token first:

```bash
t=$(curl -s "https://ghcr.io/token?service=ghcr.io&scope=repository:<owner>/<repo>:pull" | jq -r .token)
curl -s -o /dev/null -w '%{http_code}\n' -H "Authorization: Bearer $t" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  "https://ghcr.io/v2/<owner>/<repo>/manifests/<tag>"      # 200 = public
```

Beware also that the package's **web page** 404s when the linked source
repository is private, because GitHub gates that UI on repo visibility. Page
visibility and registry pull access are independent.

That is also why a plain `GITHUB_TOKEN` is enough here — no PAT secret is
needed, because the repository (and therefore the package) is public. On a
*private* package it would not be: ghcr answers a blob `HEAD` with `403` rather
than `404` (deliberately, so it does not leak which blobs exist) and
go-containerregistry — which ko uses to upload — treats that as fatal. Packages
created by `GITHUB_TOKEN` inherit the repository's visibility, so a private
repository produces a private package that the default token can only push while
every layer happens to be cached. In that case set a `GHCR_TOKEN` secret (a PAT
with `write:packages`); the workflow prefers it and falls back to `GITHUB_TOKEN`.

Two flags matter and both are load-bearing:

- `--sbom=none` — ko publishes SBOMs **by default**, and its upload path trips
  over exactly that 403. `--sbom-dir` alone does not help: ko still uploads when
  `--push` is on. Generate SBOMs locally with `--sbom-dir sbom --push=false`.
- `--bare` plus a **bare** `--tags` value — otherwise ko names the repository
  `<package>-<import-path-hash>`, or rejects a full `image:tag` with
  "repository can only contain the characters …".

The digest is read from `--image-refs`, whose lines are all **untagged** (the
multi-arch index first, then one line per platform) — only ko's stdout carries
the tag.

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
