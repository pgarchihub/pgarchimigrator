# pgArchiMigrator v2.0.0

**This version onward, pgArchiMigrator is distributed as the Community
Edition** — part of the [pgArchiHub](https://pgarchihub.com) product
family, developed by ArchiOrbit Labs.

## Overview

v2.0.0 is a major release built around two things: four new schema
migration operations, and an entirely new capability — moving a whole
database between PostgreSQL instances, not just changing a schema
in place. Alongside those, the dashboard was substantially reworked
based on real usage: a redesigned navigation shell, a smarter
foreign-key picker, one-click retry with a review step, and one-line
install scripts so trying this out doesn't require Docker.

## New: Database Migration

A second migration mode, distinct from schema migration — moves an
entire database (schema *and* data) to a different PostgreSQL
instance: a different host, a different cloud provider, or a newer
PostgreSQL version. Built on PostgreSQL's own native logical
replication (no third-party replication tooling), with row-for-row
validation before a job reports itself ready. This tool does not
perform cutover — repointing application traffic at the new instance
remains a separate, deliberate step.

- **Structured connection fields** — Host/Port/Username/Password/
  Database instead of a raw connection string.
- **Schema/table picker** — connects to the source and lists its real
  schemas and tables with checkboxes, instead of typing names from
  memory; supports both whole-schema and specific-table scoping. A
  matching read-only check against the target lets you confirm what
  already exists there before starting.
- **Retry with a review step** — retrying a failed job shows exactly
  what will be reused (host/port/username/database on both sides, the
  replication address if one is set, and the schema/table scope) before
  it starts, with an editable override for the one field most likely
  to need correcting between attempts (the replication address) —
  never re-exposing a password to do it.
- Available via the CLI, the cookie-authenticated dashboard API, and a
  service-to-service OAuth2 API for CI/CD automation.

## New Schema Migration Operations

Four operations added since v1.0.0, each routed through the existing
strategy engine (`DIRECT_DDL` / `EXPAND_BACKFILL` / `SHADOW_TABLE`)
rather than a bespoke mechanism of its own:

- **`RENAME_TABLE`** — renames the table and leaves a backward-compatible
  `VIEW` under the old name (PostgreSQL's own auto-updatable view rules
  mean both reads and writes pass through transparently), so live
  callers using the old name aren't broken instantly. This view isn't
  auto-dropped — cleanup is a separate, later step.
- **`ADD_FOREIGN_KEY`** — reuses `ADD_CONSTRAINT`'s own `NOT VALID` +
  `VALIDATE CONSTRAINT` pattern, which PostgreSQL also supports for
  foreign keys. Supports `ON DELETE` behavior (`CASCADE`, `SET NULL`,
  `SET DEFAULT`, `RESTRICT`, `NO ACTION`) against a closed allow-list.
- **`ADD_GENERATED_COLUMN`** — a genuine, native
  `GENERATED ALWAYS AS (...) STORED` column on a small table; on a
  large one, a plain column plus a `BEFORE INSERT/UPDATE` trigger
  simulating the same behavior (not native — PostgreSQL requires a
  full table rewrite to add a real generated column, which this
  avoids).
- **`PARTITION_TABLE`** — the largest addition. PostgreSQL has no way
  to convert an existing table to partitioned in place, so this always
  routes through `SHADOW_TABLE` (new table + logical replication sync
  + atomic swap), regardless of table size. Supports `RANGE` and
  `LIST` strategies, with bounds specified either as an explicit list
  or a rule-based shortcut (`monthly`, `yearly`, `daily`), plus an
  optional `DEFAULT` partition.

## Dashboard

- **Redesigned navigation** — a collapsible left sidebar (icon-only
  when collapsed, its own toggle at the top so it's never scrolled out
  of view) and a fixed full-width header with the logo, a Help
  shortcut, and a clickable account menu (Switch user / Sign out). The
  header and sidebar stay in place; only the content area scrolls.
- **Foreign-key picker** — `ADD_FOREIGN_KEY`'s "Referenced table" and
  "Referenced column" are now dropdowns populated from your own
  database (excluding the source table itself), with primary-key
  columns clearly marked, instead of free-text fields.
- **Operation dropdown grouped by what it acts on** — Table, Column,
  and Index operations are now visually separated via `<optgroup>`.
- **Connection info banner** — the New Migration screen shows which
  database server, port, user, and PostgreSQL version this instance is
  actually connected to, front and center, before you start filling in
  a migration.
- Naming clarity: the schema-migration screen is now labeled
  **"Zero-Downtime Migration"** and the whole-database move screen is
  labeled **"Database Migration"** (previously "Migrations" and
  "Upgrades") — the underlying routes and API paths are unchanged,
  this is a display-only rename.

## Installation

One-line install scripts for Linux, macOS (Apple Silicon), and
Windows — downloads the right portable package, verifies its SHA-256
checksum, and installs it to this project's own standard location. See
the README for usage. Docker remains fully supported and unchanged.

## CI/CD

- **`migration-apply.yml`** — a manually-triggered-only GitHub Action
  (no push/merge ever starts it) that applies Migration as Code files
  against a real database, requiring explicit confirmation (typing
  `APPLY`). Runs a `preview-file` health check first, archives the
  output as a build artifact, optionally notifies Slack, and on
  failure summarizes completed job IDs for manual rollback.
- **`publish-image.yml`** — publishes the Docker image to GHCR, making
  good on something the README had promised but never actually
  produced before. Triggered on a git tag push, builds multi-platform
  (amd64/arm64).

## Removed

- The `/legacy` route and `dashboard.html` — the vanilla-JS dashboard
  that predated the move to the React SPA has been removed entirely.
  It only ever supported `ADD_COLUMN`/`ALTER_COLUMN_TYPE` and didn't
  even know about v1.0's later operations, making it a misleading
  "fallback" to keep around.

## Security

`react-router-dom` (a production dependency) had moderate-severity
CVEs (`GHSA-wrjc-x8rr-h8h6`, `GHSA-337j-9hxr-rhxg`), resolved by
upgrading to `react-router-dom@7.18.3`. The only real breaking change
in the v6→v7 migration was the removal of the `future` prop (v6's
"future flags" are now v7's default behavior) — removed from every
file that used it, with the full frontend test suite passing
throughout.

Remaining advisories in the `esbuild`/`vite`/`vitest` toolchain (still
visible in `npm audit`) were deliberately left as-is — they only pose
a risk to the local development server ("enables any website to send
any requests to the development server") and never reach the
production output `vite build` actually produces. Fixing them requires
a much larger, breaking major Vite upgrade, deferred since there's no
real production risk today.

## Test Coverage

- **Backend**: real decision-logic tests in the `strategy` package for
  every new operation, plus integration tests against real PostgreSQL
  (including end-to-end sync+swap verification for `PARTITION_TABLE`
  and the new Database Migration flow's own two-instance scenarios).
- **Frontend**: 365/365 tests passing.
