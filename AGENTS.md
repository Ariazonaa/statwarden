# AGENTS.md

Instructions for AI coding agents (Codex, Cursor, OpenCode, Aider, Copilot and similar).

The full guidance lives in [CLAUDE.md](./CLAUDE.md) — it is written for every assistant, not only
Claude. Read it before changing anything. The short version:

- You are helping a human contributor, who is the author of the pull request and responsible for it.
- Keep changes small and on-topic; do not refactor or reformat code the task did not touch.
- Run the checks before claiming success: `make test vet lint`, and in `web/`:
  `npm run check && npm run lint && npm test`.
- A new `Store` method is implemented in both `PgStore` and `MemStore`; a new route is added to
  `internal/api/openapi.json`; UI text goes into both `en.ts` and `de.ts`; migrations come in
  up/down pairs.
- Never weaken tests, lint rules or security gates to get green, and never commit secrets, real
  hostnames or personal data.

See also [CONTRIBUTING.md](./CONTRIBUTING.md) and [docs/architecture.md](./docs/architecture.md).
