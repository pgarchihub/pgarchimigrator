# syntax=docker/dockerfile:1

# --- Frontend build stage -------------------------------------------------
# Builds the React SPA (web/) fresh on every image build, rather than
# relying on the committed internal/api/webapp/ snapshot staying in sync
# with web/src by hand. That committed snapshot still exists and is used
# by plain `go build`/`go run` for local development (no Node toolchain
# required just to run the backend) — but a container image should never
# ship a UI that's silently one commit behind, so this stage always
# rebuilds it and the Go build stage below copies ITS output over the
# committed one before compiling.
FROM node:22-slim AS webbuild
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

# --- Build stage --------------------------------------------------------
# All real dependencies (pgx, pglogrepl, modernc.org/sqlite, cobra) are
# pure Go — no cgo required — so CGO_ENABLED=0 produces a fully static
# binary that can run on a minimal (even scratch) base image.
#
# Base image version note: pgx v5.9.1 (the version go.mod currently
# resolves to — it was deliberately left unpinned, see go.mod's comment)
# requires Go >= 1.25.0. Using an older golang:1.22 base here fails with
# "requires go >= 1.25.0 (running go 1.22.x; GOTOOLCHAIN=local)" — the
# official Docker Go images set GOTOOLCHAIN=local, so they will NOT
# auto-download a newer toolchain the way a developer's local `go` command
# might. Keep this in sync with go.mod's `go` directive.
FROM golang:1.25 AS build

# VERSION is injected into the binary via -ldflags below (see
# internal/version's doc comment for the full reasoning) — passed at
# build time with `docker build --build-arg VERSION=v1.0.0 .`, matching a
# real git tag in CI (see the launch guide's publish-image job). Left
# unset, it defaults to "dev", exactly like a plain `go build` would —
# nobody running `docker build .` locally without the arg gets a
# misleadingly specific-looking version string.
ARG VERSION=dev

WORKDIR /src

# Cache dependency downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# Remove the committed webapp snapshot's content before overlaying this
# build's fresh output. Vite's content-hashed filenames (index-<hash>.js)
# mean a plain COPY alone would just ADD the new files alongside an old
# build's — harmless (index.html only ever references the current
# hashes), but stale files would otherwise accumulate in the image
# forever across rebuilds.
RUN rm -rf internal/api/webapp/*

# Overwrite with this build's freshly built SPA — see webbuild's comment
# above for why. internal/api's //go:embed directive picks this up before
# the compile step below.
COPY --from=webbuild /web/dist/. internal/api/webapp/

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w -X github.com/pgarchihub/pgarchimigrator/internal/version.Version=${VERSION}" \
    -o /out/pgarchimigrator \
    ./cmd/pgarchimigrator

# --- Runtime stage -------------------------------------------------------
# distroless/static includes CA certificates (needed for TLS connections
# to managed PostgreSQL services) and a minimal /etc/passwd with a
# non-root "nonroot" user, but no shell, package manager, or other attack
# surface — appropriate for a fully static Go binary with no other runtime
# dependencies.
FROM gcr.io/distroless/static-debian12:nonroot

# IMPORTANT (TR-13): pgArchiMigrator's state store (internal/state,
# SQLite-backed) is explicitly single-instance only — running more than
# one replica against the same /data volume, or scaling out at all, will
# corrupt or split-brain the checkpoint database. This is enforced at the
# Kubernetes level in deploy/helm's StatefulSet (replicas hardcoded to 1),
# not just documented here — see that chart's comments for the full
# rationale.
WORKDIR /data
VOLUME ["/data"]

COPY --from=build /out/pgarchimigrator /usr/local/bin/pgarchimigrator

USER nonroot:nonroot

EXPOSE 8080

ENTRYPOINT ["/usr/local/bin/pgarchimigrator"]
# Default to `serve` with the state DB and audit log under the mounted
# /data volume; override CMD (or pass extra args after `docker run <image>`)
# to run other subcommands like `migrate`, `list`, or `sweep` instead.
CMD ["serve", "--addr", ":8080", "--state-db", "/data/pgarchimigrator-state.db"]
