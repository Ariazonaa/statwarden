# Deployment & operations

Statwarden ships as **one Go binary** with the SvelteKit frontend embedded, plus a
PostgreSQL 16 database. Configuration is exclusively via environment variables
(12-factor) — the same image runs in every environment; see
[`.env.example`](../.env.example) for every variable.

There are three supported ways to run it:

1. **Docker Compose** (recommended) — app + PostgreSQL in one stack.
2. **Bare metal** — the binary under systemd + your own PostgreSQL.
3. **Release binaries** — the tarballs attached to a GitHub release.

TLS is always terminated by a reverse proxy in front — see
[`reverse-proxy.md`](reverse-proxy.md) (Caddy / Nginx / Traefik, including the
`/ws` WebSocket upgrade).

Whichever way you run it, two of those variables have to be **generated**
first — read the next section before you start anything.

---

## The two secrets

`SESSION_SECRET` and `ENCRYPTION_KEY` are the only settings you generate rather
than decide. One command per variable — run it twice, and never use the same
value for both:

```sh
openssl rand -base64 48          # -> SESSION_SECRET
openssl rand -base64 48          # -> ENCRYPTION_KEY
```

### Why their strength is checked at the door

Neither value is stretched before use. `ENCRYPTION_KEY` is hashed exactly once
with SHA-256 to produce the AES-256-GCM key that seals monitor configurations,
notification-channel configurations and TOTP secrets at rest;
`SESSION_SECRET` keys an HMAC directly. There is no key-derivation function and
no work factor in front of either, so whoever obtains a database dump can test
guesses against it at full hashing speed — the entropy of the string you
configure *is* the strength of the encryption.

The obvious remedy, a salted KDF, is not available: the derivation is baked into
every row any deployment has already encrypted, and changing it would not
strengthen those rows, it would make them permanently unreadable. That leaves
the input as the only place a floor can be enforced, which is what
`checkSecretStrength` in `internal/config/config.go` does at startup.

### What startup does with a weak value

Outside `DEV_MODE`, each of the two values is judged as follows:

| Finding                                     | Effect                                   |
| ------------------------------------------- | ---------------------------------------- |
| Looks like a placeholder                    | **Fatal** — the process refuses to start |
| Shorter than 32 characters                  | Warning on every startup, boots anyway   |
| Built from fewer than 5 distinct characters | Warning on every startup, boots anyway   |

The placeholder test folds the value first — lowercased, with every
non-alphanumeric character dropped — so `change-me`, `CHANGE_ME` and `changeme`
are one and the same marker. The fragments are `changeme`, `changethis`,
`replaceme`, `placeholder`, `example`, `insecure`, `supersecret`, `yourkey`,
`yoursecret` and `randomchars`, and they match **anywhere** in the value: a key
with your own `example.com` hostname worked into it is refused too. Genuinely
generated output does not trip this by accident.

**Both values shipped in `.env.example` are deliberately among the rejected
ones.** They are there to break `cp .env.example .env && docker compose up`, not
to be used — an install that boots on a copied key encrypts production
credentials under a value that is public knowledge. Replace both lines before
the first start, or the first thing the software does is refuse to run.

The asymmetry — fatal for a placeholder, a warning for a merely short key — is
about upgrades. Refusing to start on a short key would take a running instance
down on the release that introduced the check, and the operator could not fix it
without losing everything sealed under the old `ENCRYPTION_KEY`; taking down the
system that notices outages to improve a key already in service is the wrong
trade. Nobody has irreplaceable data under a placeholder, so that one stays
fatal.

Warnings are emitted once per start, at `WARN`, as `configuration warning` with
the detail attached — visible with `docker compose logs app` or
`journalctl -u statwarden`. Fix them at the next maintenance window rather than
never: the next section is what a delayed fix eventually costs.

`DEV_MODE=true` skips all of this, so `make dev` still starts with no
environment at all.

### `ENCRYPTION_KEY` is not freely rotatable

This is the most important operational fact about the variable. Anything sealed
under the old key cannot be read back under a new one, and that covers:

- **monitor configurations** — credentials embedded in check definitions;
- **notification-channel configurations** — SMTP passwords, webhook tokens, …;
- **TOTP secrets** — every 2FA enrolment.

No migration can re-encrypt them, because the key lives only in the application
process and never in the database. Nothing is silently emptied either: an
unreadable value stays exactly as it was found and simply fails where it is
used. So if you do change `ENCRYPTION_KEY`, plan for the aftermath:

1. **Re-enter the affected credentials by hand** — every monitor and every
   notification channel whose configuration carried a secret.
2. **Every 2FA user has to re-enrol.** Their authenticator app will no longer
   verify. Backup codes are hashed rather than encrypted, so those still work
   and are the way back in: log in with one, disable 2FA, enrol again.

Treat the key as data, not as configuration: back it up with the same care as
the database and keep the two together, because a restored dump is useless
without the key it was sealed under.

### Rotating `SESSION_SECRET` is cheap

`SESSION_SECRET` keys the HMAC over the OIDC login-flow cookie, which is what
stops its holder from rewriting the flow state it carries. Rotating it
invalidates only the login ceremonies currently in flight — those live five
minutes, and a user caught mid-redirect just clicks "log in" again. Sessions are
database rows and are not derived from it, so nobody is logged out.

It is therefore the one of the two you can replace at will — and worth
replacing if the value you are running is one you would not choose today.

---

## 1. Docker Compose

```sh
cp .env.example .env             # then generate the two secrets, see above
docker compose up -d             # pulls the published image, starts postgres + app
```

`docker-compose.yml` runs `ghcr.io/ariazonaa/statwarden:latest`, which CI builds,
scans and pushes on every push to `main` — and pushes only AFTER a clean Trivy
run, so an image carrying a CRITICAL or HIGH finding never reaches the registry.
The image is public; no `docker login` is needed.

To build locally anyway — an unmerged change, an air-gapped host, or
to check that a self-built image behaves like the published one:

```sh
docker compose -f docker-compose.yml -f docker-compose.build.yml up -d --build
```

For production, pin `STATWARDEN_IMAGE` to a commit tag rather than tracking
`latest`. Then what is running is legible, and a rollback is a tag change:

```sh
STATWARDEN_IMAGE=ghcr.io/ariazonaa/statwarden:<short-sha>
```

- `db` is `postgres:16-alpine` with a `pg_isready` healthcheck; `app` waits for
  `service_healthy` before starting (`depends_on … condition`).
- Named volumes `pgdata` (database) and `uploads` (logos/branding) survive
  container restarts.
- Set at least `SESSION_SECRET`, `ENCRYPTION_KEY` and `POSTGRES_PASSWORD` in
  `.env`; compose derives `DATABASE_URL` from `POSTGRES_PASSWORD`. The copied
  file will not boot as-is — the two secrets it ships are rejected as
  placeholders, on purpose.
- Back `.env` up alongside the `pgdata` volume. A database dump restored without
  the `ENCRYPTION_KEY` that sealed it hands you unreadable monitor and
  notification credentials and unusable 2FA enrolments.

Apply migrations explicitly (optional — the app auto-migrates at startup):

```sh
docker compose run --rm app --migrate-only
```

### Development stack (hot reload)

`docker-compose.dev.yml` runs **only** PostgreSQL (exposed on
`localhost:5432`); the app and frontend run natively for true hot reload:

```sh
docker compose -f docker-compose.dev.yml up -d          # postgres only
make dev            # Go backend in DEV_MODE with text logs
cd web && npm run dev   # SvelteKit dev server (HMR, proxies /api to :3001)
```

---

## 2. Bare metal (systemd)

Use the sample unit [`deploy/statwarden.service`](../deploy/statwarden.service); its
header documents the full install sequence. In short: drop the `statwarden` binary
in `/usr/local/bin`, put config in `/etc/statwarden/statwarden.env`, generate the two
secrets into it, then `systemctl enable --now statwarden`. The unit runs
`--migrate-only` as `ExecStartPre`, then the server, with hardening and
`CAP_NET_RAW` for ICMP ping monitors. Logs go to the journal as structured JSON.

If the service dies immediately on its first start, read the journal before
anything else: an environment file still carrying the `.env.example` secrets
fails validation and the reason is printed there. `/etc/statwarden/statwarden.env`
belongs in your backups next to the database, for the reason given above.

---

## 3. Release binaries

Releases are cut by pushing a semver tag (`vX.Y.Z`); the
[release workflow](../.github/workflows/release.yml) attaches static
`linux/amd64` and `linux/arm64` binaries to the GitHub release as `.tar.gz`, with
a `SHA256SUMS` covering both. The version, commit and build date are linked into
the binary and reported at `GET /api/v1/version`. The same tag also publishes a
`vX.Y.Z` image tag on `ghcr.io/ariazonaa/statwarden`.

To build an image yourself from the same tree:

```sh
make docker      # tags statwarden:<version> and statwarden:latest locally
```

The `Dockerfile` is the one CI uses, so a locally built image matches the
published one — with one caveat: the build metadata reaches it under two
argument names. `make docker` and the compose overlay pass `DATE`, the CI image
workflow passes `BUILT`. The Dockerfile accepts both; if you write your own
build command, pass one of them or `/api/v1/version` will report
`"date": "unknown"`.

---

## Health & readiness

| Endpoint   | Meaning                                        | Use for            |
| ---------- | ---------------------------------------------- | ------------------ |
| `/healthz` | Process is up and serving                      | liveness / restart |
| `/readyz`  | Process is up **and** the database is reachable | load-balancer gating / rollout |

The container `HEALTHCHECK` polls `/healthz`. Point your orchestrator's
readiness gate at `/readyz` so traffic only arrives once the DB is connected.

---

## Security headers

Every response carries a small set of hardening headers
(`internal/api/security_headers.go`); there is nothing to configure.

| Header                    | Value                            |
| ------------------------- | -------------------------------- |
| `Content-Security-Policy` | `object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors 'none'` |
| `X-Content-Type-Options`  | `nosniff`                        |
| `Referrer-Policy`         | `strict-origin-when-cross-origin` |

The policy deliberately sets no `script-src` and no `default-src`: the SPA
relies on an inline pre-paint theme script and on SvelteKit's inline hydration
bootstrap, and this build has no nonce or hash injection seam, so either
directive would break the dashboard. What is set are the directives that harden
without touching inline scripts — no plugin execution, no injected `<base>`
rewriting every relative URL, no injected `<form>` posting to a remote
endpoint.

The dashboard and the API are therefore no longer framable — previously no
security headers were set at all. **Public status pages are the exception**: the
SPA route `/status/*` relaxes `frame-ancestors` to `*`, exactly as it behaved
before, so anyone already embedding `/status/<slug>` in an intranet portal or
wiki keeps working. No regression, no migration. Only that one directive is
relaxed — the others still apply, which matters most on precisely that route,
since it renders operator-supplied titles, descriptions and footer text to
anonymous visitors.

---

## Credentials in URLs

Two endpoints accept a credential in the query string, and **both are deprecated**:

| Endpoint | Deprecated | Use instead |
| --- | --- | --- |
| `/metrics` | `?token=<scrape token>` | `Authorization: Bearer <token>` or `X-Metrics-Token: <token>` |
| `/api/v1/public/status/<slug>` | `?password=<page password>` | `X-Page-Password: <password>` |

A credential in a URL is copied into places nobody controls or can clear:
browser history, `Referer` headers, reverse-proxy access logs, and — for the
scrape token — Prometheus's own target metadata. Several systems end up holding
a secret that belongs to one.

**They still work.** Breaking monitoring in order to improve monitoring is a bad
trade, and the callers are usually configuration in somebody else's repository or
a link pasted into a wiki years ago. Instead, a request that uses one is answered
with the standard deprecation headers:

```
Deprecation: true
Sunset: Mon, 01 Mar 2027 00:00:00 GMT
Link: <…/docs/deployment.md#credentials-in-urls>; rel="deprecation"
```

**The parameters stop being accepted on 1 March 2027** (`internal/deprecation`).
Migrate before then:

```sh
# Prometheus — replace the query parameter with a bearer token
scrape_configs:
  - job_name: statwarden
    authorization:
      credentials: <METRICS_TOKEN>
    static_configs:
      - targets: ["statwarden:3001"]

# curl against a password-protected status page
curl -H "X-Page-Password: <password>" https://statwarden.example/api/v1/public/status/<slug>
```

A correct password also mints a short-lived, page-scoped unlock cookie, so a
browser only sends the password once rather than on every request.

The use of either parameter is logged: once per process for the scrape token (a
scrape runs every few seconds, so logging each one would drown the log), and per
request for a status-page password, naming the page and the client IP but never
the password itself.

---

## Database backups & restore

Scheduled logical backups are **off by default**. Many deployments already back
up at the platform level — a managed database, or snapshots of the volume — and a
job that writes gigabytes should be a decision rather than a surprise.

Turn them on with `BACKUP_ENABLED=true`. The job then writes a `pg_dump` in
PostgreSQL's custom format (`-Fc`) into `BACKUP_DIR` every `BACKUP_INTERVAL_HOURS`,
starting with one pass immediately at startup.

| Variable | Default | Meaning |
|---|---|---|
| `BACKUP_ENABLED` | `false` | Master switch. |
| `BACKUP_DIR` | `./backups` (Docker: `/data/backups`) | Target directory. Must not escape the working directory. |
| `BACKUP_INTERVAL_HOURS` | `24` | Time between dumps. |
| `BACKUP_KEEP` | `7` | Newest N dumps kept **regardless of age**. |
| `BACKUP_MAX_AGE_DAYS` | `30` | Delete older than this, but never below `BACKUP_KEEP`. `0` disables age pruning. |

`BACKUP_KEEP` is a floor, not a target, and the distinction matters: with age as
the only rule, an instance whose `pg_dump` had been failing for longer than
`BACKUP_MAX_AGE_DAYS` would prune its way to **zero** backups — deleting the only
copies that exist, at exactly the moment they are the only thing left, while
reporting nothing wrong because pruning itself succeeded.

### Requirements and failure behaviour

`pg_dump` must be on `PATH`, and it must be **at least as new as the server** it
dumps — it refuses a server newer than itself. The Docker image ships
`postgresql-client`; on a bare install, provide it yourself.

If `pg_dump` is missing or `BACKUP_DIR` is not writable, the job logs an ERROR at
startup and **disables itself**. It does not abort the process: a broken backup
configuration must not take monitoring down with it. That means a silent
non-backup is possible, so watch it — no successful run is ever recorded under
the `backup` job in `/metrics`, which is the condition to alert on.

Dumps are written to a `.partial` file and renamed only on success, so a crashed
or killed dump can never be mistaken for a restorable one.

### Restoring

A backup nobody has restored is a guess. The integration suite performs a real
dump-and-restore round trip (`internal/backup`, build tag `integration`), and
this is the same procedure by hand.

Restore into a **fresh, empty database** rather than over a running one:

```sh
# 1. Stop the app so nothing writes during the restore.
docker compose stop app

# 2. Create an empty target database.
docker compose exec -T db psql -U statwarden -d postgres   -c 'CREATE DATABASE statwarden_restored;'

# 3. Restore the dump into it. --no-owner/--no-acl because the dump was taken
#    that way: it restores as whoever is connecting, so the original role does
#    not have to exist.
docker compose exec -T db pg_restore --no-owner --no-acl   -U statwarden -d statwarden_restored < ./data/backups/statwarden-20260813T020000Z.dump

# 4. Point DATABASE_URL at statwarden_restored and start the app. Migrations are
#    applied at startup and are idempotent, so a dump from an older version is
#    brought forward automatically.
docker compose up -d app
```

Verify before you cut over: log in, and check that monitors, status pages and
recent heartbeats are present. Only then swap the database names, if you want the
original name back.

Two things the dump does **not** contain:

- **Uploaded status-page logos.** They live on the uploads volume
  (`UPLOADS_DIR`), not in the database. Back that directory up alongside.
- **Your secrets.** `ENCRYPTION_KEY` is not in the dump, and monitor
  configurations and TOTP secrets are stored sealed with it. **A restore without
  the original `ENCRYPTION_KEY` gives you a database whose credentials cannot be
  opened** — the monitors will be there and every check that needs a secret will
  fail. Keep the key wherever you keep the backups, and separately from them.

## Retention & background jobs

Raw heartbeats are pruned by a background job (`internal/retention`) that runs
one pass at startup and then every 6 hours, deleting rows older than
`HEARTBEAT_RETENTION_DAYS` (default 180). Set the variable to control the
window; the job logs how many rows it removes. It shuts down cleanly with the
rest of the process. Pruning cannot be switched off from the environment:
`HEARTBEAT_RETENTION_DAYS` is parsed with a floor of 1, so `0` is a startup
error rather than a way to keep heartbeats forever.

The same job also trims `notification_deliveries` — the record of which channel
was told what, and when — on its own window, `DELIVERY_RETENTION_DAYS`
(default 365). Longer than the heartbeat window deliberately: heartbeats are
samples and there are millions of them, deliveries are events that arrive only
on a state change, and this is the table an operator reads after an incident.
The two are independent, so setting one does not affect the other.

Keeping it bounded is no longer only about disk. `Dispatcher.Restore` asks this
table which channels are still awaiting a recovery, once per outage that was in
progress at startup, so an unbounded log puts work proportional to the age of
the deployment on the startup path.

That same lookup is why **a monitor that is currently down keeps all of its
delivery rows, whatever `DELIVERY_RETENTION_DAYS` says**. The rows are how the
dispatcher knows who was told the monitor is down and not yet told otherwise;
prune them and the answer becomes "nobody", so the recovery for that outage is
never announced. It only bites when an outage outlives the window — impossible
in practice at the 365-day default, ordinary if you set 7 to keep the table
small — so the rows the lookup needs are simply not eligible rather than relying
on the window being generous. The retained set is bounded by the resend interval:
a handful of rows per channel per outage, released as soon as the monitor
recovers.

The third window that job carries is the **monitor recycle bin**,
`TRASH_RETENTION_DAYS` (default 30). Deleting a monitor is a soft delete: it
disappears from every view but keeps its heartbeats, tags, channel links and
status-page placements, and can be restored from *Recycle bin* on the dashboard
— paused, so nothing starts checking or alerting again on its own. After the
window it is destroyed for good, along with its whole history, and the job logs
that at INFO because it is the one pass here that is irreversible.

Unlike the other two, `0` is valid and means **keep the bin forever**; nothing
is then hard-deleted on a timer and you empty it by hand. Purging a single
monitor immediately is admin-only, from the same dialog — the bin exists so that
the irreversible step is separate and deliberate.

All three passes also run independently: a failing heartbeat delete no longer stops
the delivery-log trim, and vice versa. They touch unrelated tables, and the
heartbeat delete is the one large enough to hit a statement timeout.

### The notification outbox

Every event the notifier can be handed — a state change, a certificate-expiry
warning, a maintenance notice — is written to `notification_outbox` before
anyone is notified. The row is deleted only once the event is genuinely
finished: delivered to every channel it was meant for, or deliberately not sent
(the monitor is muted, quiet hours are in force, no channel is assigned). A
provider that failed is not finished, so its row stays and is tried again.

In between, the alert survives a full dispatch queue, a provider outage and a
hard kill. A sweeper runs every 30 seconds, claims rows whose five-minute lease
has lapsed and re-dispatches them. A row is leased from the moment it is
written, by the process that wrote it — which is why a fresh row can look
"stuck" for a few seconds: it is claimed, not lost, and claiming it up front is
what stops the sweeper delivering an event its own process is still holding.

Delivery is therefore at-least-once, on purpose. A lease that lapses while a
send is genuinely still running produces a duplicate alert; a duplicate is a
nuisance and a missing alert is the bug.

One case is not covered: **digest mode**. With `Digest.Interval > 0` alerts are
batched in memory and the outbox row is cleared when the event joins the batch,
because keeping it would make the retry re-add the same event to a buffer that
still holds it and the digest would list the outage twice. A crash between
buffering and the flush therefore loses that batch. Individual (non-digest)
delivery has no such gap.

The table is normally empty or nearly so. A backlog that does not drain means
alerts are not going out, which makes it the one thing worth graphing:

```sql
SELECT count(*), min(created_at) FROM notification_outbox;
```

After ten attempts an event is given up on and deleted, logged at `ERROR` as
`notify: GIVING UP on an alert after repeated failures`, with its monitor id and
kind — the only place an alert is ever dropped on purpose, and it says so. Rows belonging to a deleted
monitor go with it.

---

## Scaling notes

Statwarden is designed as a **single-instance** application. Key constraints if you
outgrow one node:

- **One scheduler per deployment.** The monitor engine's scheduler holds
  in-memory timers and a bounded worker pool. Running two app instances against
  the same database would double every check and duplicate notifications —
  **do not** run more than one instance. Scale *up* (more CPU/RAM) before
  scaling out; the worker pool defaults to `4 × NumCPU` but never fewer than 16,
  and is tunable via `WORKER_POOL_SIZE`. Checks are I/O-bound, so the pool
  sizes concurrency rather than CPU — the floor exists because on a one- or
  two-core host `4 × NumCPU` is only 4 to 8 slots, and a handful of monitors
  sitting in their timeout then occupy every one of them.
- **Database is the shared state.** All monitors, heartbeats and sessions live
  in PostgreSQL. To grow, give Postgres more resources, ensure the
  `heartbeats (monitor_id, checked_at)` index is healthy, and lower
  `HEARTBEAT_RETENTION_DAYS` to bound table size. Hourly/daily rollup tables
  (todo §8) and native partitioning are the documented next steps for very large
  histories.
- **Stateless HTTP, but sticky WebSockets.** The REST surface is stateless; the
  WebSocket hub is per-process. A future multi-node design would need a shared
  pub/sub (e.g. Postgres `LISTEN/NOTIFY` or Redis) to fan out events — not
  required for the single-instance target.
- **If you must run redundantly**, run a warm standby that is **stopped** (or
  DB-only) and promote it on failure, rather than active-active. True
  horizontal scaling (multiple probing nodes with quorum) is tracked as the
  "Multi-Node-Probes" idea in todo §12.
