# Contributing

Thanks for wanting to help. Statwarden is maintained by one person in their
spare time, so a few ground rules keep it manageable for everyone.

## Before you start

- **Bugs:** open an issue with the version (`/api/v1/version`), what you did,
  what you expected and what happened. Logs help; redact secrets first.
- **Security problems:** do not open an issue — see [SECURITY.md](./SECURITY.md).
- **Features:** please open an issue to discuss it **before** writing code. A PR
  for an unannounced feature may be declined even if it is good, simply because
  every feature is something to maintain. Check the
  [open issues](https://github.com/Ariazonaa/statwarden/issues) first.
- **New monitor types and notification channels** are the easiest place to
  start: each is a self-contained package, see [docs/plugins.md](./docs/plugins.md).

## Development setup

```sh
docker compose -f docker-compose.dev.yml up -d   # PostgreSQL only
go run ./cmd/statwarden                          # backend, see .env.example
cd web && npm install && npm run dev             # frontend, proxies /api to :3001
```

`DEV_MODE=true` runs the backend without a database (in-memory store), and the
frontend runs against an in-browser mock API unless built with
`VITE_USE_MOCK=false`.

## What a pull request needs

All of these run in CI; running them locally first saves a round trip.

```sh
make test vet lint                    # Go: tests, go vet, golangci-lint (pinned)
cd web && npm run check && npm run lint && npm test
```

- **Tests** for the behaviour you change. The Go suite runs against an
  in-memory store; SQL changes additionally need an integration test
  (`-tags integration`, see the `Makefile`).
- **Both stores.** A new method on the `Store` interface must be implemented in
  `PgStore` and `MemStore` in the same change, or the package does not compile.
- **Both languages.** UI text goes into `web/src/lib/i18n/locales/en.ts` **and**
  `de.ts`; the type check fails on a missing key.
- **API changes** update `internal/api/openapi.json` — a test compares it with
  the router.
- **Migrations** come in pairs (`.up.sql` and `.down.sql`), numbered
  sequentially.
- **Commits** follow [Conventional Commits](https://www.conventionalcommits.org/)
  with a scope, e.g. `feat(monitor/dns): …` or `fix(web/settings): …`.

[CLAUDE.md](./CLAUDE.md) and [docs/architecture.md](./docs/architecture.md)
explain the conventions behind these rules in more detail.

## Using AI tools

AI-assisted contributions are welcome — Claude Code, Codex, Cursor, Copilot or
anything else. [CLAUDE.md](./CLAUDE.md) (and [AGENTS.md](./AGENTS.md) for other
tools) gives an assistant the project rules it would otherwise have to guess.
What matters:

- **You are the author.** You open the pull request, so you answer for every
  line in it and should be able to explain any of it in review.
- **Verify, do not trust.** Run the checks above yourself. Read the diff before
  you push it; an assistant will happily produce code that compiles and is wrong.
- **Keep it focused.** Small, on-topic changes. Reject unrelated refactors,
  renames and reformatting the assistant slips in.
- **Say so.** Tick the AI box in the pull request template and note briefly how
  you checked the result. It changes nothing about acceptance; it helps review.
- **No secrets or personal data** in prompts that leave your machine or in the
  code they produce.

Pull requests that are clearly unreviewed generated output — failing checks,
invented APIs, large unrelated changes — will be closed without detailed review.

## License

By contributing you agree that your contribution is licensed under the
[MIT License](./LICENSE), like the rest of the project.
