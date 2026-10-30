# Statwarden — Architecture Reference

One Go binary, one PostgreSQL. The binary embeds the built SvelteKit SPA, runs the monitor scheduler, and serves REST + WebSocket on a single port. Monitor types and notification providers are registry-registered plugins behind small interfaces — adding one never touches core code.

## Repository layout

```
statwarden/
├── cmd/statwarden/main.go          # entrypoint: config → logger → DB → migrations → scheduler → HTTP/WS
├── internal/
│   ├── config/                 # env-var config, validation, fail-fast on missing secrets (outside DEV_MODE)
│   ├── db/                     # pgxpool + sqlc-generated queries
│   ├── migrations/             # golang-migrate .up/.down.sql, embedded, auto-run at startup
│   ├── domain/                 # shared core types (Monitor, Heartbeat, User, …)
│   ├── monitor/
│   │   ├── engine/             # scheduler, worker pool, retry state machine
│   │   ├── registry/           # Checker interface + type registry
│   │   ├── push/               # last-seen tracking + watchdog for passive push monitors
│   │   ├── types/              # one subpackage per family: httpcheck, websocketcheck, tcpcheck,
│   │   │                       # udpcheck, ntpcheck, pingcheck, dnscheck, snmpcheck, radiuscheck, certcheck,
│   │   │                       # postgrescheck, mysqlcheck, mongocheck, redischeck,
│   │   │                       # dockercheck, grpccheck, mqttcheck, steamcheck, pushcheck
│   │   └── sslcheck/           # shared TLS cert inspection + daily expiry sweep
│   ├── notify/
│   │   ├── registry/           # Notifier interface + provider registry
│   │   ├── providers/          # one subpackage per provider
│   │   ├── secrets/            # AES-256-GCM sealing of channel configs at the persistence boundary
│   │   └── dispatcher.go       # concurrent fan-out, resend, escalation
│   ├── auth/                   # sessions, argon2id, TOTP 2FA, API keys
│   ├── api/                    # chi REST v1 — incl. status pages, incidents, maintenance handlers
│   ├── ws/                     # topic-based WebSocket hub
│   ├── report/                 # uptime, percentiles, downtime log, flakiness, rollup job
│   ├── metrics/  badge/        # Prometheus exposition · Shields-style SVG badges
│   ├── importexport/           # config bundle + Uptime-Kuma importer
│   ├── webhookq/               # shared bounded, order-preserving JSON delivery queue (outbound + siem)
│   ├── siem/                    # audit-event forwarder: one JSON POST per audit_log row
│   └── outbound/  retention/  version/   # state-change webhook · heartbeat pruning · build metadata
├── web/                        # SvelteKit app (i18n catalogues in src/lib/i18n); build output web/dist is go:embed'd from repo root
├── Dockerfile                  # multi-stage build; compose files sit beside it in the root
└── docs/
```

## Monitor engine

- Scheduler owns `map[monitorID]*ScheduledJob` with drift-corrected `time.Timer` re-arming (no overlapping runs of the same monitor).
- Bounded worker pool (default 4×NumCPU — checks are I/O-bound) executes `CheckTask`s; each carries a context with per-type timeout.
- State machine: `UP → PENDING` (first failure, shorter `retryInterval`) `→ DOWN` (after `maxRetries`) `→ UP`. **Transitions** drive notifications; raw heartbeats are always recorded.
- Parent/child: child alerts are suppressed while the parent is DOWN. Maintenance windows suppress alerts, never heartbeats.
- Push monitors are passive: `POST /api/v1/push/{token}` (GET also accepted) resets last-seen; a watchdog goroutine marks stale monitors DOWN.
- TLS-capable checks capture cert data; a daily sweep raises expiry alerts at day thresholds (21/14/7/3/1) independent of check interval.

```go
type Checker interface {
    Type() string
    Validate(cfg json.RawMessage) error
    Check(ctx context.Context, cfg json.RawMessage) (CheckResult, error)
}

type Notifier interface {
    Type() string
    Validate(cfg json.RawMessage) error
    Send(ctx context.Context, cfg json.RawMessage, event NotificationEvent) error
}
```

Both registries are populated by `init()` self-registration in each subpackage; the scheduler/API only know type-name strings stored on rows. [docs/plugins.md](plugins.md) is the contributor guide, with a worked example of each — including the frontend field spec, which is a second contract the backend interface does not imply.

## WebSocket hub

Topic-based subscriptions (`monitor:{id}`, `dashboard:all`, `status-page:{slug}`); session-cookie auth at upgrade; unauthenticated connections may only subscribe to public status-page topics; slow clients are dropped rather than blocking the hub.

## REST API

`/api/v1` on chi. problem+json (RFC 7807) errors, pagination on lists, ETag on heartbeat reads. Public endpoints: push token, status-page read, badges, `/metrics` (token-gateable), `/healthz`, `/readyz`, `/api/v1/version`, `/api/v1/openapi.json`, `/api/v1/docs`.

### Self-documentation

The contract ships in two forms, both embedded in the binary and both public:

- **`/api/v1/openapi.json`** — a hand-authored OpenAPI 3.1 document (`internal/api/openapi.json`), served with `info.version` and `servers[0].url` patched in per instance. Hand-authored rather than generated: a generator would need annotations on every handler and would trade a reviewed document for a build step nobody reads. `TestOpenAPIMatchesRouter` walks the live chi tree instead and fails **both ways** — a route missing from the spec, or a spec operation the router does not serve. Adding a route means adding it to the spec; the test names the path.
- **`/api/v1/docs`** — an interactive reference (`internal/api/docs.go` + `docs.html`). It fetches the spec at runtime and renders operations grouped by tag with parameters, request and response schemas (`$ref` resolved, `allOf` merged), so it holds no copy of the API surface and cannot drift. A "try it" console issues same-origin requests carrying the session cookie, adding `X-CSRF-Token` from the readable `statwarden_csrf` cookie on mutating verbs, exactly as the SPA client does.

Both live under `/api/v1` rather than at a top level: the document is versioned, and the SPA catch-all owns the root path space. The page is hand-written rather than a vendored Swagger-UI/Redoc bundle — the binary is expected to run air-gapped, so a CDN is out, and vendoring a renderer would add megabytes of third-party JavaScript plus its supply chain to a binary whose selling point is having no runtime dependencies. It sets its own strict CSP (`default-src 'none'` with a per-response nonce on its two inline blocks), which both hardens the page and mechanically enforces the no-external-assets rule: a stray CDN reference added later simply would not load.

### Audit log

Every mutating handler funnels through `server.audit`, which writes the `audit_log` row and then — only on a successful write — hands the entry to `internal/siem` for forwarding to an operator-configured collector (`audit_webhook_url` in `settings`). Forwarding is queued, never inline: the local table is the record of truth, and a collector that is down must not fail, delay or roll back the operation being audited. `GET /audit-log.csv` is the pull half of the same export — admin-only, bounded by a row cap rather than a time span, flagging a truncated window in `X-Statwarden-Export-Truncated`, and itself recorded as `audit.export`.

### Outgoing webhooks and their signature

Two feeds POST JSON to operator-configured URLs through the shared queue in `internal/webhookq`: one per monitor state change (`internal/outbound`, `outbound_webhook_url`) and one per audit entry (`internal/siem`, `audit_webhook_url`). Each feed can have its own signing secret (`outbound_webhook_secret`, `audit_webhook_secret`). With one set, every request carries

```
X-Statwarden-Signature: t=<unix seconds>,v1=<lowercase hex HMAC-SHA256(secret, "<t>.<raw body>")>
```

A receiver recomputes the MAC over the raw body it received (never a re-encoded one), compares in constant time, and rejects a `t` more than five minutes from its own clock — that window is what stops a captured request being replayed. `webhookq.Verify` is the reference implementation. The secrets are write-only through the API: sealed with the ENCRYPTION_KEY box on write, and `GET /settings` reports each only as `true`/`false`. A secret that cannot be read or opened makes the queue DROP the payload with an error rather than send it unsigned, so a wrong ENCRYPTION_KEY can never quietly downgrade an authenticated feed.

### Monitor recycle bin

`DELETE /monitors/{id}` is a SOFT delete (migration 0018): `monitors.deleted_at` is stamped and every ordinary read in `internal/api` filters on it through the `liveMonitor` constant, so heartbeats, tags, channel links and status-page placements survive and a restore brings the history back with them. Three reads deliberately do not filter — the bin endpoints, `PurgeMonitor`/`RestoreMonitor`, and the parent-cycle walk, which has to see binned ancestors because a chain that loops through one is still a loop once it is restored. `GET /monitors/trash`, `POST /monitors/{id}/restore` (writer) and `DELETE /monitors/{id}/purge` (admin — the one irreversible step) are the way back.

A placement outliving its monitor is a real state, not an edge case, and the public page assembles from a map keyed by monitor id: it skips a placement it has no monitor for, rather than appending the zero value. That guard is the difference between a hidden component and a blank page.

The scheduler is deliberately not taught about the column. The delete also clears `active`, which is what `ListActiveMonitors`, the push recorder and the dispatcher already select on, so a binned monitor drops out of all three without regenerating sqlc. What that does NOT cover is the dispatcher's in-memory state: an outage that was open at the moment of the delete can never be closed, because the recovery event that would close it needs a check that no longer runs. So the delete, the bulk delete and the purge all call `Dispatcher.Forget`, which drops the outage, its resend timer and any mute AND records a tombstone, so an event already in flight — queued, or replayed from the durable outbox — cannot rebuild the state. Its counterpart `Dispatcher.Reset` clears the same state WITHOUT the tombstone and is what the restore, the pause, the bulk pause and a PUT that deactivates a monitor call: those monitors are still there and must be able to alert again immediately. Two consequences follow from that and are tested: a restored monitor comes back **paused** (nothing can know what `active` was before), and a binned monitor is not a parent — the engine looks parents up in the scheduled set, so deleting one un-suppresses its children rather than muting them forever. `TRASH_RETENTION_DAYS` (default 30, `0` = keep forever) drives the sweep in `internal/retention`, which is where every "delete things older than N days" policy lives.

### Incident post-mortems

`incidents.postmortem` and `incidents.postmortem_published_at` are written only by `PUT …/incidents/{incID}/postmortem`, never by the incident update handler — the same server-owned-column arrangement `status_pages.logo_path` uses, so a full-replace PUT from a stale editor cannot erase a write-up it never loaded. An unpublished draft is stripped from every public surface by `Incident.publicCopy`, applied in exactly two places: the public status-page payload and `announceIncident`, the WebSocket broadcast anonymous visitors subscribe to.

## Auth

Server-side sessions in Postgres + opaque cookie (instant revocation beats stateless JWT here); argon2id hashing; TOTP 2FA with backup codes; roles `admin`/`user`/`viewer`; CSRF via SameSite + custom header; rate limits on login and push endpoints.

## Database

pgx/pgxpool; golang-migrate embedded migrations auto-run at startup (advisory-locked, `--migrate-only` opt-out); sqlc for typed queries. `monitors.config` and `notifications.config` are `jsonb` validated by each plugin's `Validate()`. Notification secrets are AES-GCM-encrypted with `ENCRYPTION_KEY`. `heartbeats` is indexed on `(monitor_id, checked_at)` with a retention pruning job; hourly/daily rollup tables serve uptime aggregation. Native partitioning is a documented future option.

Core tables: `users, sessions, api_keys, monitors, tags, monitor_tags, heartbeats, heartbeat_rollups_hourly, heartbeat_rollups_daily, cert_info, notifications, monitor_notifications, notification_deliveries, maintenance_windows, maintenance_monitors, status_pages, status_page_groups, status_page_monitors, incidents, incident_updates, incident_monitors, status_page_subscribers, settings, audit_log`.

## Frontend

The public status page has a second route, `/status/{slug}/history`, backed by `GET /public/status/{slug}/history?months=`: month-by-month availability over years, behind the same password gate and omitting any monitor the page marks `show_uptime: false`. It reuses `buildBars` at month granularity, so the archive and the page's own bars cannot disagree about what a bucket means.

SvelteKit + TypeScript, `adapter-static` (pages/assets → `web/dist`, `fallback: index.html`), embedded via root-level `go:embed all:web/dist`. Design system in `web/src/lib/design-system` — tokens and primitives, previewed at `/styleguide`.

The authenticated shell follows Uptime Kuma's layout: a top header (status pages, dashboard, maintenance, profile menu) with a bottom nav on phones. The monitor area — `/dashboard`, `/list`, `/monitors/*` — is the `(monitors)` route group, whose layout keeps `lib/components/monitorlist/MonitorList.svelte` on the left while the right pane changes; search, filters, folders, bulk actions and the recycle bin all live in that list, and its filter state is the module store `monitorList.svelte.ts` so the dashboard's quick stats can drive it. Each row's per-check bar reads `recent_beats` from the monitor list (the newest 50 statuses, kept live from the `dashboard:all` heartbeats), and the dashboard's event log reads `GET /monitors/events`. `/settings` and `/profile` share `SettingsShell.svelte`: a section menu on the left, the active section (`?s=<id>`) on the right.

The dashboard home is a widget board. `web/src/lib/dashboard/layout.ts` owns the catalogue and the pure layout operations; the arrangement is persisted per browser in `prefs.svelte.ts` alongside density, favourites and the manual monitor order — see that module's header for why it is localStorage and not a table. Every widget renders from data the page already holds, so adding one has never needed an endpoint. A stored layout is sanitised on every read, because the same browser can load a newer build and then an older one.

Post-mortem prose is Markdown, rendered through `statuspage/markdown.ts`, which returns a block list rather than HTML. The public status page has never used `{@html}` and must not start: the text is authored by a writer-role account and read by anonymous visitors, so raw HTML there is stored XSS. The supported subset is headings, bullets, bold and inline code; everything else stays literal, links included.

Glass recipe: `backdrop-filter: blur() saturate(1.6)` + translucent layered backgrounds, hairline border, inset top-edge highlight; large-radius squircle corners; spring motion via `svelte/motion`; animated gradient backdrop (flat backgrounds make blur invisible); `@supports` fallback and `prefers-reduced-motion` fallback are mandatory.

## Deployment

Multi-stage Dockerfile (node build → go build → distroless/alpine). docker-compose: `app` + `postgres:16-alpine`, healthcheck-gated, volumes for pgdata/uploads. Config exclusively via env vars (see `.env.example`); missing secrets abort startup outside `DEV_MODE`. Graceful shutdown drains in-flight checks and closes WS connections.

## Git workflow

Trunk-based: protected `main`, short-lived `feat/*` / `fix/*` branches, Conventional Commits (`feat(monitor/dns): …`), PR per feature (CI gate), planned work tracked in GitHub issues.
