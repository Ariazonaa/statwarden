# CLAUDE.md

Guidance for AI coding assistants (Claude Code and others — `AGENTS.md` points here) working in this
repository. Human contributors are welcome to read it too: it is the shortest accurate description of
how the codebase fits together and which rules the build enforces.

Statwarden is self-hosted uptime monitoring: one Go binary that embeds a SvelteKit SPA, backed by
PostgreSQL. `docs/architecture.md` is the reference for subsystem detail; `docs/deployment.md` for
running it; `docs/plugins.md` for adding monitor types and notification channels.

## Working in this repo with an AI assistant

AI-assisted contributions are welcome. The person opening the pull request is its author and is
responsible for every line in it, whoever or whatever typed it. In practice:

- **Understand the change before proposing it.** Read the code you are about to modify and the tests
  around it; do not pattern-match from a similar file.
- **Keep changes small and on-topic.** One concern per pull request. No drive-by refactors, renames or
  reformatting of code the task did not touch.
- **Run the checks** listed under "Commands" before claiming something works, and report failures
  honestly. "The tests pass" means you ran them.
- **Never invent APIs, config keys or file paths.** If something is unclear, search the codebase or ask.
- **Do not weaken a check to make it pass** — no deleted tests, no `t.Skip`, no lowered lint or
  security thresholds, no new allowlist entries in `.gitleaks.toml` for real values.
- **Never commit secrets**, real hostnames, personal data or production URLs, not even in tests or
  mock data. Use `example.com`, `example.test` and obviously synthetic values.
- Say in the pull request description that an AI assistant was used and how you verified the result.

## Commands

```sh
make build          # frontend, then Go binary with web/dist embedded
make backend        # Go binary only (embeds whatever web/dist currently holds)
make dev            # backend in DEV_MODE (in-memory store, no database) with text logs
make test           # go test ./...
make vet            # go vet ./...
make lint           # golangci-lint v2, pinned via `go run` — no local install needed
make test-integration  # tests tagged `integration` against a real PostgreSQL (see Makefile)
make migrate        # apply migrations and exit (needs DATABASE_URL)
make docker         # production image

cd web && npm run dev      # SvelteKit dev server, proxies /api to :3001
cd web && npm run check    # svelte-kit sync && svelte-check — the type gate
cd web && npm run lint     # prettier --check
cd web && npm test         # vitest
cd web && npm run build
```

A single Go test: `go test ./internal/api/ -run TestStatusPageLogoUpload -v -count=1`.
Use `-count=1` when re-running — Go caches results per package and a cached `ok` hides a change.

PostgreSQL for local work: `docker compose -f docker-compose.dev.yml up -d` (database only; run the app
natively for hot reload).

Before a pull request, the minimum is: `make test vet lint` and, if `web/` changed,
`npm run check && npm run lint && npm test` in `web/`. CI runs all of these plus the integration tests,
`govulncheck`, `npm audit`, gitleaks and Trivy.

## Architecture rules the build enforces

### The Store interface has two implementations, and both are compile-time enforced

`internal/api/store.go` declares the persistence surface; `PgStore` (real) and `MemStore` (DEV_MODE and
every unit test) both implement it, asserted at the top of `store.go`. **Adding a method to the
interface without implementing it in both breaks the entire `internal/api` package, including all its
tests.** Interface, `pgstore*.go` and `memstore.go` change together in one edit.

`cmd/statwarden/main.go` picks `MemStore` when there is no database pool (DEV_MODE), so a feature that
only works against PostgreSQL is unusable in the project's own dev mode.

### The unit test suite never touches a real database

Every `internal/api` unit test runs against `MemStore` via `newHarness(t)` in `harness_test.go`. SQL
placeholder/argument mismatches, `FOR UPDATE` semantics and constraint violations are invisible to
`go test` — a renumbered placeholder is a silent column swap at runtime, not a compile error. When you
change SQL in `pgstore*.go`, re-read the finished statement, count its placeholders against the
argument list by hand, and add or extend an integration test (`//go:build integration`).

### sqlc exists but `internal/api` does not use it

`internal/db/sqlcgen` is generated from `internal/db/queries` and is stale relative to later
migrations. `internal/api` hand-writes its SQL. Do not run `make sqlc` as a side effect of unrelated
work — it drags in unrelated model changes across the tree.

### Route registration order matters

`internal/api/spa.go` serves `index.html` with HTTP 200 for any unmatched path. Any new non-API route
must be registered **before** `r.Handle("/*", o.Static)` in `router.go`, or the SPA catch-all swallows
it. Public routes belong inside the `publicRateLimit` group; mutating routes in the writer + CSRF group.

The REST API lives entirely under `/api/v1/...`, and an unknown API path answers
`application/problem+json` (404), never HTML. The only non-API routes the server owns are `/ws`,
`/uploads/logos/{name}`, `/uploads/avatars/{name}` and `/status/*`; everything else falls through to
the SPA, which does client-side routing.

Every route must be described in `internal/api/openapi.json` — `TestOpenAPIMatchesRouter` walks the
live router and fails on any difference.

### Checkers and notifiers self-register

Each `internal/monitor/types/*` and `internal/notify/providers/*` package calls `registry.Register`
from `init()`. The scheduler and API only see type-name strings from database rows, so adding a monitor
type or provider means a new package plus a blank import — never a change to core code. Without an
entry in `web/src/lib/api/registry.ts` a type cannot be configured in the UI. See `docs/plugins.md`.

### Migrations

`internal/migrations/NNNN_name.{up,down}.sql`, embedded, applied automatically at startup.
`migrations_test.go` enforces sequential numbering and up/down pairing. Both directions must be written.

## Frontend

### The mock is the API contract

`web/src/lib/api/client.ts` defines `export type Api = typeof mockApi`, so `mock.ts` and the real
client must stay in lockstep. This catches a **missing method** at compile time but never a **wrong
shape** — a mapper that emits the wrong field name compiles cleanly and fails silently at runtime.

`USE_MOCK` defaults to **true** unless `VITE_USE_MOCK=false` is set at build time. Any real build must
pin it (the Dockerfile and CI do; `npm run verify:no-mock` checks the bundle).

### camelCase in TypeScript, snake_case in Go

The TypeScript types are camelCase, the Go DTOs serialise snake_case. Explicit per-entity
`<entity>ToWire` / `<entity>FromWire` mappers in `client.ts` bridge the two, and every `realApi` method
routes through its mapper. Do not introduce a generic snake↔camel transform: it mangles fields like
`uptime24h` and destroys grep-ability.

Write and read DTOs can differ (`monitorRequest` vs `monitorView` in `internal/api/`), so a field
accepted on write is not necessarily returned on read.

### Svelte 5 runes only

`$state` / `$derived` / `$props` / `$bindable` throughout; there is no `svelte/store` usage and none
should be introduced. Design-system primitives live in `web/src/lib/design-system` and are previewed at
`/styleguide` — add new primitives there.

`class:` does not work on components: state classes must be interpolated into a component's `class`
prop, and the matching rules need `:global()` or Svelte silently strips them.

A bare top-level `:global(.x)` is document-wide and stays loaded after navigation. Never write
`:global(.card)` or any other generic name: anchor it to a scoped wrapper (`.page :global(.card)`) or
give it a page prefix (`.mt-row`).

### Layout

The UI follows Uptime Kuma's layout: a top header (bottom nav on phones); the monitor area
(`/dashboard`, `/list`, `/monitors/*`) is the `(monitors)` route group, whose layout keeps
`MonitorList.svelte` on the left while the right pane changes. `/settings` and `/profile` use
`SettingsShell.svelte` (section menu left, `?s=<id>` section right).

### AMOLED dark design system (no glass, no blur)

A pure-black background, opaque near-black cards (`#0a0a0a`–`#111111`) with a thin `#1f1f1f` border.
There is **no** `backdrop-filter`, no blur and no translucent surface — depth comes from borders and
shadows. All colours are CSS custom properties in `web/src/lib/design-system/tokens/tokens.css`. The
`blur` prop on `GlassPanel`/`GlassCard` is a legacy name that selects the surface fill.

### i18n catalogues are type-linked

`web/src/lib/i18n/locales/de.ts` is typed from `en.ts`, so a missing or stray key fails
`npm run check`. Both files change in the same commit. Some keys are reached dynamically —
``t(`status.${s}`)`` — so grep the namespace prefix before concluding a key is unused.

## Security

CSRF is double-submit: the server checks the `X-CSRF-Token` header against the non-HttpOnly
`statwarden_csrf` cookie. Mutating requests without it get 403.

All responses carry `Content-Security-Policy: frame-ancestors 'none'` except the public status pages,
which stay framable on purpose. If you `Set` a CSP on a route, include `frame-ancestors` or you erase
the global policy for that route.

Uploaded files are type-gated by `http.DetectContentType` on the leading bytes — never the declared
Content-Type and never the extension. SVG is rejected: logos are served same-origin with the session
cookie, where inline script would execute.

Secrets in monitor and notification configs are sealed with `ENCRYPTION_KEY` and redacted on every
read path (`safeMonitorView`); never serialise `toMonitorView` directly.

Some identifiers still carry the project's former name (Uptura) on purpose — the session-fingerprint
domain label in `internal/auth/token.go` and a few backwards-compatibility shims (legacy backup file
prefix, legacy browser storage key, `UPTURA_*` CLI variables). Leave them as they are.

## Conventions

Short-lived `feat/*` / `fix/*` branches off `main`, one pull request per change, Conventional Commits
with a scope (`feat(monitor/dns): …`, `fix(web/settings): …`).

Configuration is exclusively environment variables (`.env.example` documents every one); missing
secrets abort startup outside `DEV_MODE`. A new setting touches `internal/config/config.go`,
`config_test.go`'s tables, `.env.example` and `docker-compose.yml`, and uses the `addProblem`
accumulation pattern rather than an early return.

Code comments explain **why**, not what: the reasoning a later reader would otherwise have to
reconstruct. Match the density and tone of the surrounding code.
