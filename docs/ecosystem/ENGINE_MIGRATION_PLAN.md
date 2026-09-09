# pgArchiMigrator → Engine Architecture Migration Plan

> Internal planning document. Not pushed to the public GitHub repo (see
> the reasoning in ARCHITECTURE.md's own note on internal-only docs).
> Written before any code was moved — the actual move happens in a
> follow-up session against this plan, not ad hoc.

## Why This Document Exists

The ask: reorganize the codebase so PostgreSQL-specific code lives under
`engines/postgresql/`, in a way that's eventually "plug and play" for a
second engine (MySQL, mentioned as a directional standard — not a
concrete near-term plan, confirmed directly).

Given that, this plan deliberately does **two different things** with
two different risk levels:

1. **A mechanical package move** (low risk, done now) — physically
   relocate packages that are genuinely PostgreSQL-specific into
   `engines/postgresql/`, updating import paths. No new abstractions
   invented.
2. **Explicitly NOT done now**: designing a generic `Engine` Go
   interface/contract that PostgreSQL code is forced through. See
   "Why No Interface Yet" below — this is deferred until a second real
   engine exists to validate the boundary against.

## Investigation Findings (Before Trusting Any Classification)

A naive `grep -rl "jackc/pgx"` classification is **not reliable on its
own** — cross-checked by hand:

- `internal/strategy` and `internal/state` both matched a raw grep for
  Postgres-looking SQL keywords (`ALTER TABLE`, `pg_class`), but on
  inspection: `strategy`'s only references are in **doc comments**
  explaining where a number like `EstimatedRowCount` conceptually comes
  from — the package's own decision logic (operation type + table size
  → strategy) is genuinely engine-agnostic. `state`'s `ALTER TABLE`
  statements are its own SQLite schema migrations for the app's
  internal job-tracking database — nothing to do with the target
  database engine at all. **Neither moves.**
- `internal/api` is genuinely split: 6 of 9 files are engine-agnostic
  (routing, auth, ecosystem event publishing); 3 files
  (`resource_status.go`, `upgrade_handlers.go`, and `server.go`'s own
  `Pool *pgxpool.Pool` field) are PostgreSQL-specific. See "The `api`
  Package Problem" below — this is the one package that doesn't cleanly
  resolve into "move" or "don't move."

## Final Package Classification

### Moves to `engines/postgresql/`

| Package (now under `engines/postgresql/`) | Why it's engine-specific |
|---|---|
| `catalog` | Queries `pg_catalog`/`information_schema` directly via pgx |
| `db` | pgx connection pooling, PostgreSQL version detection |
| `ddlflow` | Executes `DIRECT_DDL`/`EXPAND_BACKFILL` — real PostgreSQL SQL |
| `shadowflow` | `SHADOW_TABLE` — PostgreSQL native logical replication |
| `upgrade` | Database Migration — logical replication between instances |
| `preview` | Generates real PostgreSQL DDL for dry-run previews |
| `progress` | Also generates PostgreSQL DDL text (verified above — this was the one case where the grep signal was real, not just a comment) |
| `reaper` | Cleans up orphaned PostgreSQL replication slots/publications |
| `typecompat` | PostgreSQL's own type system compatibility rules |
| `monitor` | Replication lag monitoring via PostgreSQL system views |

**Status: this move has been executed** (see the commit that applied
this plan). This table is kept as the historical record of what moved
and why.


### Stays in `internal/` (genuinely engine-agnostic)

`agentattestation`, `auditlog`, `auth`, `config`, `deploylayout`,
`ecosystem`, `entitlement`, `idempotency`, `migrationfile`,
`serviceauth`, `state`, `strategy`, `runtimeverify`, `version`.

Also stays: `internal/api` in its entirety, for now — see below.

## The `api` Package Problem

`internal/api`'s `Server` struct holds `Pool *pgxpool.Pool` directly as
a field, and Go's own method-receiver rule means `func (s *Server)
handleX(...)` can **only** be defined in the same package `Server`
itself lives in — it cannot be split into a separate package while
still being a method on `*Server`.

Splitting this properly would require either:
(a) moving the whole `Server` struct (meaning the entire `api`
    package, including all the genuinely engine-agnostic handlers,
    which defeats the purpose), or
(b) converting the PostgreSQL-specific methods into free functions
    taking `*Server` as a parameter, which requires first deciding
    **what `Server` itself should expose as an engine-agnostic
    interface** — e.g. should `Server` hold an abstract `Engine`
    value instead of a concrete `*pgxpool.Pool`? That question **is**
    the interface-design question this plan is deliberately deferring.

**Decision: `internal/api` does not move or split in this pass.** It
keeps importing what's now `engines/postgresql/...` by its new import
path. `resource_status.go` and `upgrade_handlers.go` stay exactly
where they are, engine coupling and all. This is an honest, visible
seam — the codebase will say "the API layer still assumes PostgreSQL"
rather than pretending otherwise with a half-abstraction that isn't
validated against a second engine.

## Why No `Engine` Interface Yet

PostgreSQL's own logical replication (`CREATE PUBLICATION`/`CREATE
SUBSCRIPTION`, `pgoutput`) — which both `SHADOW_TABLE` and the whole
Database Migration feature depend on — has no structural equivalent in
MySQL's binlog-based replication (GTID or file-position based,
different consistency model, different setup). An interface boundary
guessed from a single engine's shape is very likely wrong at exactly
this point — the part of the system doing the most PostgreSQL-specific
work. Building the abstraction now means paying for a rewrite later
when MySQL work starts for real and the guessed boundary doesn't fit.

**What this plan does instead**: the package move itself creates a
clean physical seam (`engines/postgresql/` as a sibling directory any
future `engines/mysql/` would sit next to) without committing to any
Go interface shape. When MySQL work is actually greenlit, the interface
gets designed then, informed by two real implementations instead of
one guess — using this document's own package list as the starting
inventory of "things a second engine would need to also provide."

## Execution Plan (Mechanical Move)

1. **Create `engines/postgresql/` and move the 10 packages listed
   above** via `git mv`, preserving history.
2. **Update every import path** referencing
   `github.com/pgarchihub/pgarchimigrator/internal/{catalog,db,ddlflow,
   shadowflow,upgrade,preview,progress,reaper,typecompat,monitor}` to
   `.../engines/postgresql/...` — across `internal/api`,
   `cmd/pgarchimigrator`, and each moved package's own internal
   cross-references.
3. **Update package doc comments** in each moved package to reflect
   the new path in any self-referential comments (e.g. `catalog.go`'s
   own header comment mentions `internal/api`'s handler names — verify
   those references still resolve).
4. **`gofmt -l .` and `gofmt -e` on every touched file** — mandatory
   syntax check per this project's own established practice.
5. **Rebuild and run the full test suite** — both the packages that
   can run in isolation (see `docs/TESTING.md`'s own list) and a CI
   run for the pgx/sqlite-dependent ones, since this touches import
   paths across nearly the whole backend.
6. **Update non-Go references to old paths** — `Dockerfile`,
   `docs/TESTING.md`'s own package table, `.github/workflows/*.yml`,
   the frontend's own doc comments (`web/src/**`), `go.mod`'s own
   comments, and every other non-Go file in the repo referencing an
   old `internal/<moved-package>` path.
   **(Done — repo-wide grep across every file type confirms zero
   remaining references, including inside code comments.)**
7. **Update `docs/ecosystem/ARCHITECTURE.md`** (internal-only) with
   this decision and its rationale, so a future session — Claude or
   human — doesn't have to reconstruct this reasoning from scratch.

## What This Plan Deliberately Does NOT Include

- No `Engine` interface or contract type.
- No change to `internal/api`'s structure.
- No MySQL scaffolding, stub package, or placeholder directory —
  `engines/mysql/` does not get created until MySQL work is real.
- No change to the frontend, CLI command surface, or any public API
  contract — this is an internal reorganization only.
