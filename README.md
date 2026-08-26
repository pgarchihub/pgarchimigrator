# pgArchiMigrator

[![CI](https://github.com/pgarchihub/pgarchimigrator/actions/workflows/ci.yml/badge.svg)](https://github.com/pgarchihub/pgarchimigrator/actions/workflows/ci.yml)
![Version](https://img.shields.io/badge/version-v1.0.0-blue)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-12--18-blue)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://github.com/pgarchihub/pgarchimigrator/blob/main/LICENSE)

A CLI/service tool for zero-downtime schema changes on PostgreSQL — 8
operation types (`ADD_COLUMN`, `DROP_COLUMN`, `ALTER_COLUMN_TYPE`,
`ADD_INDEX`, `DROP_INDEX`, `SET_NOT_NULL`, `ADD_CONSTRAINT`,
`RENAME_COLUMN`), each routed automatically to the cheapest safe strategy
(Direct DDL / Expand & Backfill / Shadow Table), with dry-run previews,
role-based auth, and a web dashboard. Originally scaffolded to mirror
`pgArchiMigrator_Migrator_-_Mimari_Tasarım_Dokümanı__v2.md` (the Architecture
Design Doc) one-to-one; that mapping below is still accurate for the
backend's package layout.

## Supported PostgreSQL Versions

**PostgreSQL 12 through 18** — every version in this range is validated
by this project's own CI test matrix (see `.github/workflows/ci.yml`),
not just documented as "should work."

- **PostgreSQL 10-11**: refused outright at connection time (TR-11) — a
  real, deliberate constraint, not an arbitrary cutoff:
  `ALTER TABLE ... ADD COLUMN ... DEFAULT <constant>` only became a fast,
  metadata-only operation in PostgreSQL 11; on 10 or earlier it triggers
  a full table rewrite under an `ACCESS EXCLUSIVE` lock for the entire
  table's size — exactly the kind of downtime this tool exists to avoid.
  PostgreSQL 12 (one version past that boundary) is the actual floor,
  giving a safety margin.
- **PostgreSQL 19+**: not blocked, but not yet validated by this
  project's own CI either — the web dashboard shows a "newer than
  tested" note if you connect to one (see `internal/db.ClassifyVersion`).
  PostgreSQL's own query planner has, in this project's own testing
  history, occasionally behaved differently than expected even between
  versions well within the previously-tested range — "probably fine" is
  deliberately not treated as "confirmed" here.

## Folder Structure ↔ Architecture Doc Mapping

| Folder | Architecture Doc Section | Responsibility |
|---|---|---|
| `cmd/pgarchimigrator` | 5 (Interface: CLI + REST API) | Binary entry point, Cobra commands |
| `internal/config` | 5, 6 | Configuration loading (env, yaml), preflight settings |
| `internal/db` | 3.2, 6 | pgx connection pool, TLS, minimal privilege checks |
| `internal/strategy` | 4.0 (Strategy Decision Matrix) | Operation type + table size → strategy decision |
| `internal/typecompat` | 4.0 (extension) | Automatic type-cast compatibility detection for `ALTER_COLUMN_TYPE` |
| `internal/orchestrator` | 3.1 | Orchestration Engine: State Manager + Step Executor |
| `internal/ddlflow` | 4.0 rows 1-4 | Direct DDL / Expand & Backfill flow (6 of the 8 operation types) |
| `internal/shadowflow` | 4.1, 4.3 | Shadow Table + Logical Replication flow (Decoder, SyncEngine, Apply, Swap) |
| `internal/preview` | (extension) | Dry-run: strategy + SQL + read-only pre-flight warnings, no DB writes |
| `internal/monitor` | 3.3 | Performance Monitor, Lock Detector (throttle signals) |
| `internal/reaper` | 3.3 (Orphan Resource Reaper) | Cleanup of orphaned slots/shadow tables/triggers |
| `internal/state` | 3.1, 5.1 | Checkpoint Store (SQLite, single-instance) |
| `internal/auth` | (extension) | Product-agnostic org/user/session/RBAC layer |
| `internal/auditlog` | 3.3, 6 | JSON audit log writer |
| `internal/api` | 5 | REST API + embedded web dashboard |
| `web/` | 5 ("Basit Web UI") | React SPA dashboard — built separately, embedded into the Go binary (see below) |

## Quick Start (Docker)

The fastest way to try this out — no CLI bootstrapping required, the
web UI walks you through creating the first admin account on first
visit:

```bash
docker run -d -p 8080:8080 \
  -e PGARCHIMIGRATOR_DATABASE_URL="postgresql://user:pass@your-db-host:5432/yourdb?sslmode=require" \
  -v pgarchimigrator-data:/data \
  ghcr.io/pgarchihub/pgarchimigrator:latest \
  serve --secure-cookies=true
```

Open `http://localhost:8080/` — you'll land on a one-time "Create your
admin account" screen (see `internal/api`'s `handleSetup`), not a bare
login form. The `-v pgarchimigrator-data:/data` volume is important: it's where
the SQLite state/auth databases live, and without it every container
restart loses all users and migration history.

## Setup (local development)

```bash
# 1. Backend
go mod tidy
docker compose -f deploy/docker-compose.dev.yml up -d   # test PostgreSQL instances
go run ./cmd/pgarchimigrator auth create-admin --email admin@example.com --password yourpassword

# 2. Frontend (only needed if you're changing web/ — a built copy is
#    already committed at internal/api/webapp/ for plain `go run`/`go build`)
cd web && npm install && npm run build && cd ..
cp -r web/dist/* internal/api/webapp/

# 3. Run
go run ./cmd/pgarchimigrator serve
# Open http://localhost:8080/ (redirects to the dashboard at /app)
```

`docker build .` does the frontend build step automatically (see the
Dockerfile's `webbuild` stage) — the manual `npm run build` + `cp` above is
only needed when running the Go backend directly via `go run`/`go build`
without going through Docker.

### Frontend tests

```bash
cd web
npm test        # Vitest + React Testing Library, one-shot run
npm run build   # tsc --noEmit type-check + production bundle
```

### Backend tests

Most packages need the dev PostgreSQL instances running (step 1 above):

```bash
go test ./...                                    # unit tests, no DB needed
go test ./internal/ddlflow/... -tags=integration -v   # and similarly for
                                                        # internal/{reaper,shadowflow,preview,typecompat,catalog,db}
```

### Continuous Integration

Every push and pull request runs the exact same checks above automatically
(see `.github/workflows/ci.yml`):

- **Backend / unit tests** — `gofmt` check, `go vet`, `go test ./...`
- **Backend / integration tests** — brings up the same
  `deploy/docker-compose.dev.yml` Postgres instances used locally, then
  `go test ./... -tags=integration`
- **Frontend / tests + build** — `npm test` (pinned to `TZ=Europe/Istanbul`
  in `web/vite.config.ts` — a real timezone-dependent bug once passed on a
  UTC runner and failed on a real machine; pinning a non-UTC zone makes
  that class of bug reproducible in CI too) and `npm run build`
- **Deployment / Docker build + Helm lint** — the full multi-stage Docker
  build (including the frontend build stage) and `helm lint` /
  `helm template` against the chart

Nothing here needs configuring beyond what's already in the workflow file
— no secrets, no external services beyond the ephemeral Postgres
containers the job itself starts and tears down.

## Load testing

`cmd/loadtest` is a standalone tool (talks to the REST API like any
external client — no `internal/` import) for measuring whether a
migration actually stays low-impact at scale:

```bash
# 1. Generate a large test table (defaults to 10M rows)
go run ./cmd/loadtest generate --dsn "postgresql://user:pass@host:5432/db"

# 2. Run concurrent traffic against it while triggering a real migration,
#    and get a before/during/after latency report
go run ./cmd/loadtest run \
  --dsn "postgresql://user:pass@host:5432/db" \
  --api-url "http://localhost:8080" \
  --admin-email admin@example.com --admin-password <password> \
  --operation ADD_COLUMN --column loadtest_flag --column-type boolean --default false
```

`run` reports p50/p95/p99 query latency before, during, and after the
migration, plus a rough verdict (p99-during vs p99-baseline ratio). Use
`--operation`/`--column-type`/`--default` to test different operation
types and strategies against the same large table. To specifically
exercise `EXPAND_BACKFILL`'s batched-write strategy (rather than
`DIRECT_DDL`'s metadata-only fast path), the default must be both a
genuinely volatile expression **and** explicitly flagged as such — the
server can't infer volatility from the string alone:

```bash
go run ./cmd/loadtest run \
  --dsn "postgresql://user:pass@host:5432/db" \
  --api-url "http://localhost:8080" \
  --admin-email admin@example.com --admin-password <password> \
  --operation ADD_COLUMN --column created_ts --column-type timestamptz \
  --default "now()" --volatile-default
```

`SHADOW_TABLE` (the strategy for the largest/most disruptive changes,
e.g. an incompatible `ALTER_COLUMN_TYPE`) needs `--strategy-override`
to test reliably — the server otherwise decides DIRECT_DDL vs
SHADOW_TABLE automatically based on whether the old/new types are
compatible (see `internal/typecompat`), which isn't always obvious from
the column types alone:

```bash
go run ./cmd/loadtest run \
  --dsn "postgresql://user:pass@host:5432/db" \
  --api-url "http://localhost:8080" \
  --admin-email admin@example.com --admin-password <password> \
  --operation ALTER_COLUMN_TYPE --column customer_id --column-type text \
  --strategy-override SHADOW_TABLE
```

The target table needs a PRIMARY KEY for `SHADOW_TABLE` specifically
(unlike `DIRECT_DDL`/`EXPAND_BACKFILL`) — `loadtest generate`'s table
already has one (`id BIGSERIAL PRIMARY KEY`).

## License

Licensed under the [Apache License, Version 2.0](LICENSE).

