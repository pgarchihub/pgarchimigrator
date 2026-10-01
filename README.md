# pgArchiMigrator

[![CI](https://github.com/pgarchihub/pgarchimigrator/actions/workflows/ci.yml/badge.svg)](https://github.com/pgarchihub/pgarchimigrator/actions/workflows/ci.yml)
![Version](https://img.shields.io/badge/version-v2.1.0-blue)
![Edition](https://img.shields.io/badge/edition-Community-2f7d73)
![PostgreSQL](https://img.shields.io/badge/PostgreSQL-12--18-blue)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](https://github.com/pgarchihub/pgarchimigrator/blob/main/LICENSE)

This is the **Community Edition** — part of the
[pgArchiHub](https://pgarchihub.com) product family, developed by
ArchiOrbit Labs.

## ⚡ The Problem

A plain `ALTER TABLE ... ADD COLUMN ... DEFAULT <volatile expression>`,
or an incompatible `ALTER COLUMN ... TYPE`, on a large production
table takes an `ACCESS EXCLUSIVE` lock for as long as the rewrite
takes — every read and write queues up behind it for the full
duration. Generic ETL/dump-and-restore tools solve a different
problem (moving data between systems) and don't address this at all.

pgArchiMigrator routes every one of its 12 supported operation types
to the cheapest strategy that's actually safe for the table in front
of it — a metadata-only DDL statement when that's genuinely all it
takes, a batched background backfill when it isn't, or a shadow
table synced via PostgreSQL's own native logical replication when
the change is too disruptive for either of those. Which strategy
applies to which operation, and why, is in
"Key Capabilities" below — not a marketing
name, the actual decision table this project's own code implements.

## 🚀 Quick Demo

Try the real thing against a real 5,000,000-row table — one command,
no manual setup, a real zero-downtime `ALTER COLUMN TYPE` you can
watch complete:

```bash
git clone https://github.com/pgarchihub/pgarchimigrator.git
cd pgarchimigrator/playground
docker compose up -d --wait
```

Full walkthrough (login, what to click, real measured timing on a
shared CI runner) in [`playground/README.md`](playground/README.md).

## Screenshots

**New migration** — the screen shows exactly which host, database, and
PostgreSQL version it's connected to before you pick an operation, with
`ADD_FOREIGN_KEY`'s referenced table/column populated from the schema
itself (primary keys marked) instead of typed from memory.

![New migration](docs/images/pgArchiMigrator_01_New_Migration.png)

**Migration detail** — the exact SQL a completed migration ran, plus a
step-by-step progress trace and a rollback window. Here, `RENAME_TABLE`
left a backward-compatible view under the old name.

![Migration detail](docs/images/pgArchiMigrator_02_Migration_Detail.png)

**Database migration detail** — a completed whole-database move: every
in-scope table synced and verified via PostgreSQL's own native logical
replication. Cutover stays a separate, deliberate step you take
yourself.

![Database migration detail](docs/images/pgArchiMigrator_03_Database_Migration_Detail.png)

## 📊 Benchmark

Real measured output from this repository's own `cmd/loadtest` tool
(application-query latency measured through the real REST API, not a
synthetic number) — see this project's own CI
(`.github/workflows/ci.yml`'s `benchmark` job) for how this is
re-measured on every push, not written once and left to go stale.
`ADD_COLUMN` with a volatile default against a real 5,000,000-row
table, specifically chosen to exercise `EXPAND_BACKFILL`'s batched-write
path rather than the metadata-only fast path most schema changes
actually take:

```
Strategy used: EXPAND_BACKFILL

BEFORE/AFTER migration (baseline) (421,636 queries, 0 errors):
  p50=3ms  p95=3ms  p99=4ms  max=242ms
DURING migration (622,214 queries, 0 errors):
  p50=4ms  p95=5ms  p99=6ms  max=471ms

p99 latency during the migration was 1.5x the baseline p99.
=> Looks like a genuinely low-impact migration.
```

Measured on GitHub Actions' own standard 2-vCPU runner — modest,
shared hardware, not a tuned benchmark machine. Run it yourself
against your own table shape and hardware: see ["Load testing"](#load-testing)
below.

## 🛠️ Key Capabilities

- **12 operation types** — `ADD_COLUMN`, `DROP_COLUMN`,
  `ALTER_COLUMN_TYPE`, `ADD_INDEX`, `DROP_INDEX`, `SET_NOT_NULL`,
  `ADD_CONSTRAINT`, `RENAME_COLUMN`, `RENAME_TABLE`,
  `ADD_FOREIGN_KEY`, `ADD_GENERATED_COLUMN`, `PARTITION_TABLE`.
- **Automatic strategy selection** — every operation routes to
  **Direct DDL** (metadata-only, when the change genuinely is that
  cheap), **Expand & Backfill** (a new column/constraint added
  alongside the old, backfilled in batches, then swapped in), or
  **Shadow Table** (a full copy kept in sync via PostgreSQL's own
  logical replication, for the most disruptive changes — e.g. an
  incompatible `ALTER_COLUMN_TYPE`, or `PARTITION_TABLE`, which always
  uses this strategy regardless of table size) — see
  `internal/strategy` for the actual decision logic, not a
  simplification of it.
- **Backward-compatible by default where it matters** —
  `RENAME_TABLE` leaves a real, auto-updatable view under the old
  name; `RENAME_COLUMN` dual-writes both names during its own
  transition window; `DROP_COLUMN` defers the physical drop until
  its own rollback window closes, so a caller still using the
  original name is never broken mid-migration.
- **Dry-run previews** — strategy + the exact SQL that would run +
  read-only pre-flight warnings, no database writes
  (`internal/engines/postgresql/preview`).
- **Role-based auth and a full audit log** — every migration records
  who ran it (`internal/auth`, `internal/auditlog`).
- **A rollback window, not just a rollback button** — `SHADOW_TABLE`
  migrations keep the pre-migration table reachable for a real
  window after cutover, not just a point of no return the instant the
  swap finishes.

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
  tested" note if you connect to one (see `internal/engines/postgresql/db.ClassifyVersion`).
  PostgreSQL's own query planner has, in this project's own testing
  history, occasionally behaved differently than expected even between
  versions well within the previously-tested range — "probably fine" is
  deliberately not treated as "confirmed" here.

## Folder Structure ↔ Architecture Doc Mapping

| Folder | Architecture Doc Section | Responsibility |
|---|---|---|
| `cmd/pgarchimigrator` | 5 (Interface: CLI + REST API) | Binary entry point, Cobra commands |
| `internal/config` | 5, 6 | Configuration loading (env, yaml), preflight settings |
| `internal/engines/postgresql/db` | 3.2, 6 | pgx connection pool, TLS, minimal privilege checks |
| `internal/strategy` | 4.0 (Strategy Decision Matrix) | Operation type + table size → strategy decision |
| `internal/engines/postgresql/typecompat` | 4.0 (extension) | Automatic type-cast compatibility detection for `ALTER_COLUMN_TYPE` |
| `internal/orchestrator` | 3.1 | Orchestration Engine: State Manager + Step Executor |
| `internal/engines/postgresql/ddlflow` | 4.0 rows 1-4 | Direct DDL / Expand & Backfill flow (10 of the 12 operation types; `ALTER_COLUMN_TYPE` and `PARTITION_TABLE` route here only when the table/change is small enough not to need Shadow Table) |
| `internal/engines/postgresql/shadowflow` | 4.1, 4.3 | Shadow Table + Logical Replication flow — `ALTER_COLUMN_TYPE` (incompatible casts) and `PARTITION_TABLE` (always, at any table size — see `strategy.OpPartitionTable`'s own doc comment for why) |
| `internal/engines/postgresql/preview` | (extension) | Dry-run: strategy + SQL + read-only pre-flight warnings, no DB writes |
| `internal/engines/postgresql/monitor` | 3.3 | Performance Monitor, Lock Detector (throttle signals) |
| `internal/engines/postgresql/reaper` | 3.3 (Orphan Resource Reaper) | Cleanup of orphaned slots/shadow tables/triggers |
| `internal/state` | 3.1, 5.1 | Checkpoint Store (SQLite, single-instance) |
| `internal/auth` | (extension) | Product-agnostic org/user/session/RBAC layer |
| `internal/auditlog` | 3.3, 6 | JSON audit log writer |
| `internal/api` | 5 | REST API + embedded web dashboard |
| `web/` | 5 ("Basit Web UI") | React SPA dashboard — built separately, embedded into the Go binary (see below) |

## 📦 Installation

### Install Script

Prefer a native binary over Docker? One line downloads the right
portable package for your OS/architecture, verifies its checksum, and
installs it to this project's own standard location
(`internal/deploylayout`):

```bash
# Linux / macOS
curl -fsSL https://raw.githubusercontent.com/pgarchihub/pgarchimigrator/main/scripts/install.sh | sh
```

```powershell
# Windows (PowerShell)
irm https://raw.githubusercontent.com/pgarchihub/pgarchimigrator/main/scripts/install.ps1 | iex
```

This verifies the download's SHA-256 checksum, but NOT the Ed25519
artifact-descriptor signature (see `cmd/pgarchisign`) — that's a
stronger, separate guarantee the script prints instructions for at the
end, worth doing if you want assurance the artifact was genuinely built
by this project rather than just "whatever bytes were at this URL."
See `scripts/install.sh`'s own header comment for why that step isn't
automated inside the installer itself.

Supported platforms today: Linux (x64/arm64), macOS (Apple Silicon
only — Intel isn't published yet), Windows (x64). See
`deploy/platforms/` for the exact matrix.

### Docker

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

(Want to see it working against real data first, with zero
configuration? See "Quick Demo" above instead.)

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
go test ./internal/engines/postgresql/ddlflow/... -tags=integration -v   # and similarly for
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
- **Playground** — the real `docker compose up` demo in
  [`playground/`](playground/README.md), run end to end against the
  real published Docker image
- **Benchmark** — the real `cmd/loadtest` measurement quoted above,
  re-run on every push

Nothing here needs configuring beyond what's already in the workflow file
— no secrets, no external services beyond the ephemeral Postgres
containers the job itself starts and tears down.

## Load testing

`cmd/loadtest` is a standalone tool (talks to the REST API like any
external client — no `internal/` import) for measuring whether a
migration actually stays low-impact at scale — the same tool behind
the "Benchmark" numbers above, which you can reproduce
or re-run against your own table shape:

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
compatible (see `internal/engines/postgresql/typecompat`), which isn't always obvious from
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

## 🤝 Community & Support

- **Docs & architecture**: [pgarchihub.com](https://pgarchihub.com)
- **Contributing**: see [`CONTRIBUTING.md`](CONTRIBUTING.md) for how
  to run this locally and what to know before opening a pull request
- **Found a bug or have a question?** Open a
  [GitHub Issue](https://github.com/pgarchihub/pgarchimigrator/issues)
- **Found a security issue?** See [`SECURITY.md`](SECURITY.md)
  instead of opening a public issue

## License

Licensed under the [Apache License, Version 2.0](LICENSE).
