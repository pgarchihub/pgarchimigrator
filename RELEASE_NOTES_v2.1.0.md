# pgArchiMigrator v2.1.0

## Overview

A minor release: real, backward-compatible additions since v2.0.0,
plus the fix that makes the published Docker image actually exist
(see "Fixed" below — v2.0.0's own image was never successfully
published).

## Added

- **DROP_COLUMN is now backward-compatible during its rollback
  window** — the column stays queryable under its original name for
  the full rollback window instead of being renamed immediately on
  entering that phase. A caller still using the original column name
  is never broken mid-migration; the physical drop is deferred to
  finalization. Brings DROP_COLUMN in line with RENAME_TABLE and
  RENAME_COLUMN, which already worked this way.
- **`GET /app/manifest.json`** — a static, build-time-generated
  manifest describing this application's own navigation (product ID,
  display name, nav items with their required roles, and the full set
  of routes) at the path a future shell/console integration would read
  it from. This is deliberately just the manifest itself — no
  mount()/unmount() contract, no runtime shell integration — see this
  repository's own internal design notes for why a fuller integration
  is intentionally deferred until there's a second real party to
  validate that contract against.
- **`playground/`** — a self-contained `docker compose up` demo: a
  pre-seeded 5,000,000-row table and a real zero-downtime `ALTER
  COLUMN TYPE` (`id`: `integer` → `bigint`, the single most common
  real-world reason teams reach for this) you can watch complete in
  well under a minute, with no manual setup (an admin login is created
  automatically). See `playground/README.md`.

## Fixed

- **The published Docker image (`ghcr.io/pgarchihub/pgarchimigrator`)
  now actually exists.** `v2.0.0`'s own tag push triggered
  `publish-image.yml`, but that run failed (a `go build` error in the
  code as it stood at that exact tag) — silently, with no user-facing
  signal, leaving the README's own "Quick Start (Docker)" instructions
  referring to an image that had never been successfully built. Found
  while verifying `playground/`'s own use of that same image against
  the real, running CI for this release — not by chance.

## Upgrading from v2.0.0

No schema or configuration changes. A plain restart on the new image/
binary is sufficient — DROP_COLUMN's own behavior change only affects
migrations *started* after upgrading; anything already mid-flight
keeps the behavior it started with.
