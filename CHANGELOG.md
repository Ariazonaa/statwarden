# Changelog

All notable changes to Statwarden are documented in this file.
Format: [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) · Versioning: [SemVer](https://semver.org).

## [Unreleased]

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

[Unreleased]: https://github.com/Ariazonaa/statwarden/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/Ariazonaa/statwarden/releases/tag/v0.1.0
