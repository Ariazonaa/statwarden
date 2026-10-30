# Statwarden — build & development tasks.
# Requires: Go 1.26+, Node 22+ and (optionally) Docker.

MODULE  := github.com/Ariazonaa/statwarden
# Pinned so every machine and CI run lints with the same version — bump here only.
GOLANGCI_LINT_VERSION := v2.12.2
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
# Where `make test-integration` looks for PostgreSQL — matches docker-compose.dev.yml.
INTEGRATION_DATABASE_URL ?= postgres://statwarden:statwarden@localhost:5432/statwarden?sslmode=disable

ifeq ($(OS),Windows_NT)
BINARY := statwarden.exe
CLI_BINARY := statwarden-cli.exe
else
BINARY := statwarden
CLI_BINARY := statwarden-cli
endif

LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT) \
	-X $(MODULE)/internal/version.Date=$(DATE)

.PHONY: all build backend frontend cli dev test test-integration vet lint migrate sqlc docker clean

all: build

## build: build the frontend, then the Go binary with the dist embedded
build: frontend backend

## backend: build only the Go binary (embeds whatever web/dist contains)
backend:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BINARY) ./cmd/statwarden

## frontend: install web dependencies and build web/dist (real backend, no mock)
# `npm ci` for a reproducible install from the lockfile (matches Docker/CI).
# VITE_USE_MOCK=false so `make build` never embeds the in-browser mock backend
# into the release artefact; verify:no-mock asserts the dist is mock-free.
frontend:
	cd web && npm ci && VITE_USE_MOCK=false npm run build && npm run verify:no-mock

## cli: build the statwarden-cli companion binary
# Deliberately NOT part of `build`: that target produces the release artefact,
# and the CLI is an optional convenience that ships separately. Build it on
# demand with `make cli`.
cli:
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(CLI_BINARY) ./cmd/statwarden-cli

## dev: run the backend in DEV_MODE (start the frontend with `cd web && npm run dev`)
dev:
	DEV_MODE=true LOG_FORMAT=text go run ./cmd/statwarden

## test: run all Go tests (no database required — the integration tag is off)
test:
	go test ./...

## test-integration: run the live-PostgreSQL suite (build tag `integration`)
# Needs a reachable PostgreSQL; the dev stack is the obvious one:
#   docker compose -f docker-compose.dev.yml up -d
# The suite creates a throwaway database on that server and drops it again, so
# pointing it at the dev stack never touches development data.
#
# -timeout is explicit because Go's default is 10 minutes and this suite runs
# close enough to it that LOAD decides the outcome rather than the code. The CI
# runners are single-CPU and host the PostgreSQL service container beside the
# tests; successful runs land between 8 and 12 minutes of job time, and one
# commit that changed nothing but two Markdown files timed out while its parent,
# carrying the actual feature, had passed. A test that fails for being on a busy
# machine teaches nobody anything.
#
# 25 minutes is headroom, not a blank cheque: a genuinely hung test still fails
# instead of running until the job is killed.
test-integration:
	DATABASE_URL="$(INTEGRATION_DATABASE_URL)" go test -tags=integration -timeout=25m -count=1 ./internal/api/... ./internal/monitor/... ./internal/backup/...

## vet: run go vet
vet:
	go vet ./...

## lint: run golangci-lint (v2, pinned — no local install needed)
lint:
	go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION) run

## migrate: apply pending database migrations and exit (needs DATABASE_URL)
migrate:
	go run ./cmd/statwarden --migrate-only

## sqlc: regenerate type-safe query code from internal/db/queries
sqlc:
	go run github.com/sqlc-dev/sqlc/cmd/sqlc@latest generate

## docker: build the production image
docker:
	docker build \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) \
		-t statwarden:$(VERSION) -t statwarden:latest .

## clean: remove build artefacts
clean:
	rm -f statwarden statwarden.exe statwarden-cli statwarden-cli.exe coverage.out coverage.html
