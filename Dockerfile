# Statwarden — multi-stage build: SvelteKit frontend -> Go binary (frontend
# embedded via go:embed) -> minimal Alpine runtime.
#
# Build from the repository root:
#   docker build -t statwarden .
#
# Multi-arch: the frontend and Go stages run on the native BUILDPLATFORM and
# cross-compile the (CGO-free) binary to TARGETOS/TARGETARCH, so a single
# `docker buildx build --platform linux/amd64,linux/arm64` produces both
# images without emulation. Plain `docker build` also works (TARGET* default
# to the host platform).

# --- Stage 1: frontend -------------------------------------------------------
# The SvelteKit output is static and arch-independent, so always build it on
# the fast native platform.
FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
# VITE_USE_MOCK defaults to true (see web/src/lib/api/client.ts), which would
# ship a dashboard talking to the in-browser fake backend instead of the real
# REST API. It must be pinned to false for any real build.
RUN VITE_USE_MOCK=false npm run build

# --- Stage 2: backend --------------------------------------------------------
# Pinned to a patch release on purpose: CI reads the Go version from go.mod, and
# `go mod download` refuses to run under GOTOOLCHAIN=local when this image is
# older than the go directive. Bump the two together.
#
# CGO_ENABLED=0 below, so the alpine flavour of this image is irrelevant; only
# the Go version is.
FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Replace the placeholder dist with the real frontend build.
COPY --from=web /src/web/dist ./web/dist

# Injected build metadata (see internal/version).
#
# Two names for the timestamp, because two callers disagree: `make docker` and
# the compose build pass DATE, while the CI image workflow passes BUILT. Accepting only DATE meant every
# image the pipeline published reported `"date": "unknown"` at /api/v1/version,
# which is exactly the question you ask after a rollback.
ARG VERSION=dev
ARG COMMIT=none
ARG DATE=unknown
ARG BUILT=""
# Provided automatically by BuildKit; default to the build host for plain builds.
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
    -ldflags "-s -w \
      -X github.com/Ariazonaa/statwarden/internal/version.Version=${VERSION} \
      -X github.com/Ariazonaa/statwarden/internal/version.Commit=${COMMIT} \
      -X github.com/Ariazonaa/statwarden/internal/version.Date=${BUILT:-${DATE}}" \
    -o /out/statwarden ./cmd/statwarden

# --- Stage 3: runtime --------------------------------------------------------
FROM alpine:3.22
# ca-certificates: outbound TLS checks/notifications. tzdata: quiet-hours and
# scheduling in local time. wget (busybox, already present) powers the
# healthcheck — no extra package needed.
#
# postgresql17-client: pg_dump for the optional scheduled backups
# (BACKUP_ENABLED).
#
# VERSIONED ON PURPOSE, and it has to be. Alpine has no package called
# `postgresql-client` — v3.22 ships postgresql15-client, postgresql16-client and
# postgresql17-client, and nothing that aliases them. An unversioned name here
# fails the image build outright.
#
# 17 rather than 16: pg_dump refuses a server NEWER than itself, so the client
# has to be at least as new as the database it dumps. The compose stack runs
# postgres:16-alpine and a 17 client handles both 16 and 17. An operator running
# PostgreSQL 18 or later needs their own image with a matching client — and will
# find out at startup rather than at the first tick, because internal/backup's
# preflight reports an unusable pg_dump loudly and disables the job.
#
# Backups are OFF by default, so none of this costs anything until asked for.
#
# `apk upgrade` FIRST, and it is not redundant next to the add below.
#
# `apk add` only resolves what its named packages need, so anything that ships
# INSIDE the base image and already satisfies a dependency is left at whatever
# version the alpine:3.22 tag was built with. The image scan found exactly that:
# libcrypto3/libssl3 at 3.5.7-r0 with CVE-2026-14456 (OpenSSL, unbounded memory
# growth -> DoS) while 3.5.8-r0 had been in the 3.22 branch index the whole
# time. Nothing in this file asked for openssl, so nothing upgraded it.
#
# This does not loosen the pin: the base stays alpine:3.22 and the upgrade is
# bounded by that branch's index, which is where the `apk add` line below has
# always taken its packages from anyway. What changes is that a rebuild now
# picks up the branch's security fixes instead of freezing whatever the cached
# base layer happened to carry.
RUN apk upgrade --no-cache \
    && apk add --no-cache ca-certificates tzdata postgresql17-client \
    && addgroup -S -g 10001 statwarden && adduser -S -u 10001 -G statwarden statwarden \
    && mkdir -p /data/uploads /data/backups && chown -R statwarden:statwarden /data
COPY --from=build /out/statwarden /usr/local/bin/statwarden

USER statwarden
EXPOSE 3001
VOLUME ["/data/uploads", "/data/backups"]
# The config default (./uploads) would resolve to the working directory, which
# is neither the declared volume nor owned by the non-root statwarden user.
ENV UPLOADS_DIR=/data/uploads
# Same reasoning for the dumps, and a sibling of the uploads volume so the two
# share one mount concept. Writing dumps here has no effect until
# BACKUP_ENABLED=true.
ENV BACKUP_DIR=/data/backups
# Liveness against the app's own /healthz. Runs via /bin/sh, so ${PORT}
# expands; defaults to 3001 to match EXPOSE.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD wget -qO- "http://127.0.0.1:${PORT:-3001}/healthz" >/dev/null 2>&1 || exit 1

ENTRYPOINT ["statwarden"]
