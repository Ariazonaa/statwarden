# Changelog

All notable changes to Statwarden are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) · Versioning: [SemVer](https://semver.org).

## [Unreleased]

## [0.2.1] - 2026-11-11

### Fixed

- **Push monitors show their heartbeat URL.** After creating a push monitor,
  its detail page shows the URL and a cron-ready `curl` example. The token is
  stored only as a hash, so the URL is shown this once; every push monitor
  also has a "Generate new URL" action, which is the way to get a URL for push
  monitors created before this release (the old URL stops working).
- The development mock could not create or edit monitors.

## [0.2.0] - 2026-11-11

A hardening release. A full review of the code base turned up five high and
about a hundred medium and low findings; this release fixes them and adds two
opt-in protections for instances shared with other people.

### Upgrade notes

- **First-run setup needs a token.** While no account exists, the server logs
  a one-time setup token at startup (`docker compose logs statwarden | grep
  "setup token"`), and the setup page asks for it. Set `SETUP_TOKEN` for
  headless provisioning. Existing instances are not affected.
- **A TOTP code is accepted once.** A second sign-in within the same 30-second
  window needs the next code.
- **The login, setup and password-reset endpoints require
  `Content-Type: application/json`** and answer 415 otherwise.
- **New write limits:** monitor and folder names 200 characters, tag names 100,
  descriptions 4000; intervals, timeouts and retries are bounded. Values over
  the limit are refused with 400.
- **`.env.example` ships no default database password** and keeps comments on
  their own lines, so it now works as a systemd `EnvironmentFile`. Set
  `POSTGRES_PASSWORD` explicitly.
- **Configuration exports use bundle version 2.** Older servers refuse them
  instead of importing password-protected status pages as public.

### Added

- `BLOCK_PRIVATE_TARGETS` (off by default): monitor checks and notification
  channels refuse loopback, private, link-local (cloud metadata) and similar
  addresses, checked before every check and again when connecting.
- `SETUP_TOKEN`, `LISTEN_ADDR` (bind to one address, e.g. behind a reverse
  proxy).
- Release image tags `X.Y.Z` and `X.Y` are the recommended way to pin the
  image; `latest` now only ever follows `main`.

### Security

- A writer could redirect a stored monitor password to another host with a
  Unicode look-alike of the `host` key; such keys are now refused.
- Bot tokens and webhook URLs no longer appear in logs, the delivery log or the
  test-send response.
- Login rate limits and lockout count per IPv6 /64; password hashing runs
  behind a concurrency limit; re-authentication (step-up, password and email
  change, TOTP verify) is rate limited; backup codes are consumed atomically.
- Status-page passwords are rate limited and locked out per page and address;
  only real status pages can be framed; anonymous live updates are limited to
  pages the visitor may see.
- `metrics_token` is stored sealed and read per request, so rotating it takes
  effect without a restart.
- Backups no longer put the database password on the command line and are
  created with mode 0600.
- The systemd unit and the compose file are hardened; the unused `CAP_NET_RAW`
  grant is gone.

### Fixed

- Notifications: a delivery retried after the monitor recovered (or went down
  again) no longer opens a phantom outage or resolves the current one; one
  slow channel no longer delays every other monitor's alerts; PagerDuty and
  Opsgenie keep certificate, latency and flapping alerts separate from
  outages; Twilio tries every recipient once; SMTP accepts display-name
  addresses.
- Engine: a push monitor used as a parent suppresses its children correctly;
  no duplicate check chains after an edit; pausing during a running check
  sticks; shutdown no longer records false DOWN results; bounded reads for
  WebSocket and Docker checks; DNS, SNMP, MQTT and ping checks honour their
  timeouts.
- Web UI: switching monitors in the list no longer leaves the previous monitor
  on screen while actions target the new one; more than 200 monitors are
  listed; an expired session signs the tab out; maintenance times use the
  configured time zone; uptime is truncated instead of rounded up to 100 %.
- Export and import keep monitor tags, channels, parents and folders, and
  status-page protection and layout.
- Status pages: no N+1 queries per public request; recurring maintenance
  shows its next occurrence; a custom domain's root opens its page.
- Rollup backfill no longer overwrites correct daily figures after a restart;
  heartbeat retention deletes in batches; expired sessions and reset tokens
  are cleaned up.

## [0.1.0] - 2026-10-30

First public release. Statwarden was developed privately before this
repository was opened; this entry summarises what it contains.

### Added

- **Engine:** bounded worker pool with drift-corrected timers, UP → PENDING →
  DOWN state machine with retries and recovery checks, flapping damping, check
  backoff for long outages, parent/child dependencies, maintenance windows.
- **19 monitor types:** HTTP(S) with status-code, keyword, JSON-path, header
  and response-time assertions; WebSocket, TCP, UDP, Ping, DNS, NTP, SNMP,
  RADIUS, TLS certificate, PostgreSQL, MySQL/MariaDB, MongoDB, Redis, Docker,
  gRPC, MQTT, Steam and passive Push monitors.
- **19 notification channels** with resend, escalation, quiet hours, digests,
  templating and a delivery log; channel credentials are encrypted at rest.
- **Users and security:** admin / user / viewer roles, OIDC single sign-on,
  TOTP two-factor, scoped and expiring API keys, CSRF protection, login rate
  limiting and lockout, audit log with CSV export and SIEM forwarding.
- **API:** a complete REST API under `/api/v1`, described by an OpenAPI 3.1
  document and an interactive reference at `/api/v1/docs`; live updates over
  WebSockets; `statwarden-cli` for the terminal.
- **Status pages:** incidents with update timelines, post-mortems, custom
  domains, password protection, logo and accent colour, a multi-year
  availability archive, SVG badges.
- **Reporting:** uptime over 24 h / 30 d / 1 y with hourly and daily roll-ups,
  latency percentiles, anomaly detection, SLA reports as CSV and PDF,
  Prometheus metrics.
- **Dashboard:** Uptime-Kuma-style layout with a persistent monitor list,
  per-check status bars, quick stats and an event log; arrangeable widgets;
  bulk actions, folders, favourites and a recycle bin; English and German UI.
- **Operations:** single Go binary with the frontend embedded, Docker image for
  amd64 and arm64, docker-compose stack, systemd unit, scheduled `pg_dump`
  backups, heartbeat retention, configuration export/import and an Uptime Kuma
  importer.

[Unreleased]: https://github.com/Ariazonaa/statwarden/compare/v0.2.1...HEAD
[0.2.1]: https://github.com/Ariazonaa/statwarden/compare/v0.2.0...v0.2.1
[0.2.0]: https://github.com/Ariazonaa/statwarden/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/Ariazonaa/statwarden/releases/tag/v0.1.0
