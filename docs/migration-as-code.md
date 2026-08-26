# Migration as Code

pgArchiMigrator supports defining migrations as version-controlled JSON
files — committed alongside your application code, code-reviewed in a
pull request, and applied idempotently via the CLI or a CI pipeline.

## Why

Without this, a migration only exists as one-off CLI flags or a web form
submission — nothing about *what* migrations a team has run, in what
order, or *why*, is captured anywhere reviewable alongside the
application code that needs them. A JSON file per migration closes that
gap: it can be diffed in a PR, reviewed like any other code change, and
its intent (via the `description` field) documented right next to the
change itself.

## File format

Each migration is a single JSON file. Field names mirror the REST API's
own request body (see `internal/api`'s `startMigrationRequest`) closely,
so anything familiar from the web UI or API carries over directly.

```json
{
  "id": "20260826_add_status_column",
  "description": "Add a status column to orders ahead of the fulfillment dashboard launch",
  "schema": "public",
  "table": "orders",
  "operation": "ADD_COLUMN",
  "column": "status",
  "type": "text",
  "default": "'active'"
}
```

| Field | Required | Notes |
|---|---|---|
| `id` | Yes | Unique across all migrations you'll ever apply — this is what makes re-running `apply-file` idempotent (see below). Prefix with a sortable date or number, e.g. `20260826_...` or `001_...`, so filenames and IDs agree on ordering. |
| `description` | No | Purely documentation — shown in CLI output and the resulting job's Description field. |
| `schema` | No | Defaults to `public`. |
| `table` | Yes | |
| `operation` | Yes | One of `ADD_COLUMN`, `DROP_COLUMN`, `ALTER_COLUMN_TYPE`, `ADD_INDEX`, `DROP_INDEX`, `SET_NOT_NULL`, `ADD_CONSTRAINT`, `RENAME_COLUMN`. |
| `column` | Operation-dependent | See `pgarchimigrator migrate --help` for exactly which operations require it — the same rules apply here. |
| `type` | For `ADD_COLUMN`/`ALTER_COLUMN_TYPE` | |
| `default` | No | Default value expression for `ADD_COLUMN`, e.g. `"'active'"` or `"now()"`. |
| `volatile_default` | No | Set `true` alongside a volatile `default` (e.g. `now()`) — triggers the Expand & Backfill strategy instead of a metadata-only `DIRECT_DDL`. |
| `index_name` | For `DROP_INDEX` (required), optional for `ADD_INDEX` | |
| `constraint_name` | For `ADD_CONSTRAINT` (required) | |
| `check_expression` | For `ADD_CONSTRAINT` (required) | e.g. `"price > 0"`. |
| `new_column_name` | For `RENAME_COLUMN` (required) | |
| `strategy_override` | No | Force `DIRECT_DDL`, `EXPAND_BACKFILL`, or `SHADOW_TABLE` — only combinations `internal/strategy`'s whitelist actually supports for the given operation are accepted; see that package's own validation for the incident that made this a hard requirement, not just a suggestion. |

## Directory convention

Keep migration files in a flat directory (a nested structure isn't
supported — this matches Flyway/golang-migrate's own convention), one
file per migration:

```
migrations/
  20260820_add_status_column.json
  20260822_add_orders_email_index.json
  20260826_backfill_created_at.json
```

`apply-file --dir migrations/` and `preview-file --dir migrations/` both
process every `*.json` file in the directory, sorted by **filename** —
not file content, not modification time — so the naming convention you
pick is what actually determines apply order.

## CLI usage

**Preview every migration in a directory** (read-only, makes no changes
— this is what the companion GitHub Action runs):

```bash
pgarchimigrator preview-file --dir migrations/
```

**Apply every migration in a directory**, skipping any already applied:

```bash
pgarchimigrator apply-file --dir migrations/
```

**A single file**, for either command:

```bash
pgarchimigrator apply-file --file migrations/20260826_add_status_column.json
```

## Idempotency

`apply-file` checks each migration's `id` against every existing job's
`Name` field before applying it. If a `COMPLETED` job with that exact
name already exists, the migration is skipped:

```
SKIP  20260820_add_status_column (already applied)
SKIP  20260822_add_orders_email_index (already applied)
APPLY 20260826_backfill_created_at — Backfill created_at for existing rows
...
```

This makes it safe to run `apply-file --dir migrations/` on every
deploy, the same way `flyway migrate` or `migrate up` are typically
invoked in a CI/CD pipeline — only genuinely new migrations ever apply.

A migration that previously **failed** or was **aborted** is not
considered applied, and will be attempted again on the next run —
matching the intuitive expectation that a failed migration needs to
either succeed or be explicitly dealt with, not silently skipped
forever.

## Continuous integration

See `.github/workflows/migration-preview.yml` — it runs `preview-file`
automatically against a temporary PostgreSQL instance whenever a pull
request touches files under `migrations/`, and posts the result as a PR
comment, so a reviewer sees the exact SQL and chosen strategy for every
migration in the diff without needing to run anything locally.
