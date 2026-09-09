# pgArchiMigrator — Testing Document

> This document brings together the project's own test strategy,
> current test coverage, and manual end-to-end test scenarios in one
> place — meant as a reference for both ongoing development and human
> contributors.

## 1. Test Philosophy and Pyramid

This project follows a four-layer test strategy:

```
        ┌─────────────────────────────┐
        │  Manual End-to-End GUI      │  ← Section 5
        │  Scenarios                  │
        ├─────────────────────────────┤
        │  Go Integration Tests       │  ← requires real PostgreSQL
        │  (-tags=integration)        │
        ├─────────────────────────────┤
        │  Frontend Tests             │  ← Vitest + Testing Library
        │  (Vitest)                   │
        ├─────────────────────────────┤
        │  Go Unit Tests              │  ← the widest base
        │  (go test)                  │
        └─────────────────────────────┘
```

**Core principle**: real tests are preferred wherever possible (real
`go test`, real `vitest run`, a real SQLite/PostgreSQL connection) —
fakes/mocks are only used when an external dependency (a real
third-party service, e.g. a real OAuth provider) genuinely can't be
exercised for real.

## 2. Sandbox Constraints (Specific to the Development Environment)

Known constraints of the sandbox environment this project was
developed in:

- **`proxy.golang.org` access is blocked** — this means packages with
  external Go module dependencies (`modernc.org/sqlite`, `github.com/
  jackc/pgx/v5`) **cannot be run with real `go test`**. For these
  packages, only **syntax verification** via `gofmt -e` is possible;
  the logic is cross-verified in an independent language where
  possible (e.g. Python's `sqlite3` module — see Section 2.2).
- **Packages with only stdlib dependencies** (`internal/strategy`,
  `engines/postgresql/progress`, `internal/entitlement`, `internal/ecosystem` —
  via `state`/`upgrade`/`entitlement` stubs — the parts of `internal/
  serviceauth` other than `sqlite_store.go`, `internal/
  agentattestation`, `internal/runtimeverify`, `internal/idempotency`
  — excluding the SQLite implementation, `internal/deploylayout`,
  `cmd/pgarchisign`) **can be copied into an isolated module and run
  with real `go test`** — this is the most frequently used
  verification method in this project.
- **No live PostgreSQL instance in the sandbox** — tests marked with
  `-tags=integration` **cannot be run** in this environment, only
  syntax-verified via `gofmt -e`. These tests run in CI (`ci.yml`, with
  a real `docker compose`).
- **The frontend (`web/`) is fully independent** — `npm run build`
  (tsc + vite) and `npx vitest run` run **cleanly and completely** in
  the sandbox, with a real browser DOM simulation (jsdom).

## 3. Backend — Go Test Layers

### 3.1. Unit Tests (Verifiable in an Isolated Module)

The following packages have **no** external dependency (SQLite/pgx)
and can therefore be isolated under `/tmp` with a minimal `go.mod` and
run with **real `go test`**:

| Package | Test Count | Notes |
|---|---|---|
| `internal/strategy` | 39 | Operation → strategy decision logic |
| `engines/postgresql/progress` | 41 | Progress computation, phase display |
| `internal/entitlement` | 5 | Edition (Community/Enterprise) checks |
| `internal/ecosystem` | 25 | Event-publishing decorators (`Store`, `UpgradeStore`), `traceparent` |
| `internal/serviceauth` (excluding SQLite) | 20 | OAuth2 client-credentials logic |
| `internal/agentattestation` | 9 | Nonce-bound Ed25519 attestation verification |
| `internal/runtimeverify` | 9 | Installed package integrity, path traversal protection |
| `internal/idempotency` (excluding SQLite) | 8 | Replaying repeated requests |
| `internal/deploylayout` | 10 | ArchiOrbitLabs directory layout resolution |
| `engines/postgresql/upgrade` (excluding SQLite) | 19 | `ConnectionProvider`, `Introspect` helpers |
| `cmd/pgarchisign` | 4 | Ed25519 artifact signing/verification |

**How to run** (example, for any package):
```bash
mkdir -p /tmp/pkg_test && cp internal/PACKAGE/*.go /tmp/pkg_test/
cd /tmp/pkg_test
cat > go.mod << 'EOF'
module pkg_test
go 1.22
EOF
GOTOOLCHAIN=local go test ./... -v
```

Some packages (`ecosystem`, `serviceauth`, `upgrade`) depend on other
packages in the project (`state`, `entitlement`); before isolating
them, **stub** copies of those packages (type definitions only, no
real logic) are created and their import paths rewritten with `sed`.

### 3.2. Packages with Heavy Dependencies (Syntax Verification Only)

Packages such as `internal/state`, `internal/auth` (the SQLite
implementation), `engines/postgresql/ddlflow`, `engines/postgresql/shadowflow`,
`internal/orchestrator`, `internal/api`, `engines/postgresql/db`,
`cmd/pgarchimigrator` depend directly on `modernc.org/sqlite` or
`github.com/jackc/pgx/v5`, so **real `go test` cannot run** in the
sandbox. For these packages:

1. **`gofmt -e package/file.go > /dev/null && echo OK`** — mandatory
   syntax check after every file change.
2. **Manual symbol verification** — every function/type name used in
   tests is confirmed to actually exist via `grep` (`gofmt -e` only
   checks syntax, it does **not catch** type/symbol mismatches).
3. **Cross-language verification of SQLite logic** — for example,
   `engines/postgresql/upgrade`'s schema-migration logic (`PRAGMA table_info` →
   conditional `ALTER TABLE`) was **actually run and verified** with
   Python's stdlib `sqlite3` module first, then ported to Go (see
   the internal architecture notes on this).
4. **These packages' REAL tests run in CI** (`ci.yml`'s `backend-unit`
   job) — this is a sandbox constraint, not a CI constraint.

| Package | Test Count (runs in CI) |
|---|---|
| `internal/auth` | 26 |
| `engines/postgresql/ddlflow` | 51 |
| `internal/orchestrator` | 17 |
| `internal/state` | 12 |
| `engines/postgresql/reaper` | 12 |
| `internal/api` | 120 |

### 3.3. Integration Tests (`-tags=integration`)

Tests that need a real PostgreSQL pair are marked with the `//go:build
integration` build tag and can **only** be run in CI or on a local
machine with `deploy/docker-compose.dev.yml` running:

```bash
docker compose -f deploy/docker-compose.dev.yml up -d
go test ./... -tags=integration -timeout 5m -v
docker compose -f deploy/docker-compose.dev.yml down -v
```

| Package | Test Count | Coverage |
|---|---|---|
| `engines/postgresql/shadowflow` | 30 | Shadow-table sync within a single instance |
| `engines/postgresql/upgrade` | 9 | Full flow **across two separate** PostgreSQL instances (`pg-logical` ↔ `pg-upgrade-target`) |

`engines/postgresql/upgrade`'s integration tests are especially important, since
this package uses **real** native PostgreSQL logical replication —
`TestFlow_Run_EndToEnd_SmallTable` (a real 500-row table, real
`CREATE SUBSCRIPTION`, real validation) is the **most comprehensive
single integration test** in this project.

## 4. Frontend — Vitest + Testing Library

```bash
cd web
npm run build       # tsc -b && vite build — TypeScript type-checking + production build
npx vitest run       # the full test suite
```

**365 tests** across 24 files — covering every route (`Dashboard`,
`NewMigration`, `MigrationDetail`, `UpgradesList`, `NewUpgrade`,
`UpgradeDetail`, `Users`, `Login`, `Setup`, `Help`, `Shell`), every UI
component (`TextField`, `VersionBadge`, `ErrorBoundary`,
`SchemaTablePicker`, `ConnectionFields`), and helper libraries
(`api.ts`, `format.ts`, `dsn.ts`, `auth.tsx`).

**Critical**: `npm run build` (tsc) and `npx vitest run` use
**different TypeScript targets** — a feature that passes under Vitest
(like `Array.prototype.at()`) can fail under `tsc -b`. **Both must be
run separately**; checking only one is not enough.

**A known test pattern**: `TextField`s with `required` add an
`sr-only "(required)"` string — tests should use
`getByLabelText(/^field name/i)` (a prefix regex) instead of
`getByLabelText("Field Name")` (an exact match).

**A known pitfall**: functions mocked with `vi.mock()` keep their own
`mock.calls` history **across tests by default** — it is not cleared
automatically. Any file where a test reads an index like
`mock.calls[0]` **must** have `beforeEach(() => vi.clearAllMocks())` —
otherwise a test **passes when the file is run alone, but can fail
when the whole suite runs together**, picking up a prior test's own
call (this genuinely happened once in this project — see
`NewUpgrade.test.tsx`).

## 5. CI/CD Test Pipelines

| Workflow | What It Tests |
|---|---|
| `ci.yml` — `backend-unit` | Real tests for the heavy-dependency Go packages |
| `ci.yml` — `backend-integration` | `-tags=integration` tests, with a real `docker compose` (including `pg-logical` + `pg-upgrade-target` health checks) |
| `ci.yml` — `frontend` | `npm run build` + `npx vitest run` |
| `ci.yml` — `deployment-checks` | Syntax checks for the deployment/packaging scripts |
| `deployment-pipeline.yml` | The Build → Checksum → Ed25519 signing → Verification chain running end to end (with a CI-only test key) |
| `migration-preview.yml` | Verifying migration-as-code files with `preview-file` |
| `migration-apply.yml` | Manually-approved production migration application (manually triggered) |

## 6. Manual End-to-End GUI Test Scenarios

This section contains step-by-step scenarios for testing the
dashboard **as a real user would**. It answers the "does this actually
work" question that automated tests (Sections 3–4) don't cover —
indeed, **4 real bugs** were found in this project during exactly this
kind of manual testing (listed in Section 6.4).

### 6.1. Environment Setup

```bash
# Source (PG 16, port 55432) and target (PG 17, port 55434) instances
docker compose -f deploy/docker-compose.dev.yml up -d pg-logical pg-upgrade-target

# Admin user
go run ./cmd/pgarchimigrator auth create-admin --email admin@example.com --password yourpassword

# The server's OWN primary connection (for migration/catalog features —
# separate from the source/target DSNs in the Database Migration form)
export PGARCHIMIGRATOR_DATABASE_URL="postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55432/pgarchimigrator_test?sslmode=disable"
go run ./cmd/pgarchimigrator serve
```

The `docker exec $(...)` subshell syntax does **not** work in Windows
PowerShell — use `docker compose exec <service> <command>` instead
(used in this format in the examples below).

### 6.2. Scenario A — Zero-Downtime Migration (Schema Migration)

1. Log in to the dashboard, click **"New Migration"**
2. Pick a table, select the `ADD_COLUMN` operation, enter a column
   name + type
3. Review the preview, start the migration
4. Watch progress on the detail page (phases: `PREFLIGHT` →
   `PREPARATION` → ... → `COMPLETED`)
5. **Retry test**: try a scenario that will **deliberately fail** (e.g.
   `RENAME_COLUMN` against a nonexistent table), confirm the **"Retry
   this migration"** button appears once it's `FAILED`, click it —
   confirm a new job starts with a **new job ID**, **without
   re-entering any information**

#### 6.2.1. The 4 New Operation Types Added in v2

These 4 operations were added after v1.0.0 (see `RELEASE_NOTES_v2.0.0.md`)
and had **never been manually tested through the GUI** — only verified
via backend unit/integration tests. The scenarios below each include a
real test table + form-filling steps + expected result.

**Common setup** — before each scenario, create test tables in the
database the app's own `PGARCHIMIGRATOR_DATABASE_URL` is connected to:
```sql
DROP TABLE IF EXISTS test_orders, test_customers CASCADE;
CREATE TABLE test_customers (id BIGINT PRIMARY KEY, name TEXT NOT NULL);
INSERT INTO test_customers (id, name) SELECT g, 'customer-' || g FROM generate_series(1, 20) g;
CREATE TABLE test_orders (id BIGINT PRIMARY KEY, customer_id BIGINT, amount NUMERIC NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now());
INSERT INTO test_orders (id, customer_id, amount) SELECT g, (g % 20) + 1, (g * 1.5)::numeric FROM generate_series(1, 100) g;
```

##### A. `RENAME_TABLE`

1. **New Migration** → table: `test_orders` → Operation: `RENAME_TABLE`
2. **New table name**: `test_orders_v2`
3. Review the preview — expected: `DIRECT_DDL` strategy, SQL showing
   `ALTER TABLE ... RENAME TO ...` plus a backward-compatible `VIEW`
   under the old name
4. Start the migration, confirm it reaches `COMPLETED`
5. **Verify** (via psql/DBeaver):
   ```sql
   SELECT * FROM test_orders_v2 LIMIT 1;  -- new name, the real table
   SELECT * FROM test_orders LIMIT 1;     -- old name, now a VIEW (should return the same data)
   \d test_orders                          -- should show "View", not "Table"
   ```

##### B. `ADD_FOREIGN_KEY`

1. **New Migration** → table: `test_orders` → Operation: `ADD_FOREIGN_KEY`
2. **Local column**: `customer_id` (pick from the dropdown)
3. **Constraint name**: `fk_orders_customer`
4. **Referenced table**: `test_customers`
5. **Referenced column**: `id`
6. **On delete**: `CASCADE`
7. Review the preview — expected: `DIRECT_DDL` strategy, SQL containing
   `ADD CONSTRAINT ... NOT VALID` + `VALIDATE CONSTRAINT`
8. Start the migration, confirm it reaches `COMPLETED`
9. **Verify**:
   ```sql
   \d test_orders  -- should show fk_orders_customer as FOREIGN KEY (customer_id) REFERENCES test_customers(id) ON DELETE CASCADE
   DELETE FROM test_customers WHERE id = 1;  -- confirm orders with customer_id=1 are also deleted (CASCADE)
   ```

##### C. `ADD_GENERATED_COLUMN`

1. **New Migration** → table: `test_orders` → Operation: `ADD_GENERATED_COLUMN`
2. **Column**: `amount_with_tax`
3. **Type**: `numeric`
4. **Generated expression**: `amount * 1.20`
5. Review the preview — expected (since the table is small):
   `DIRECT_DDL` strategy, SQL containing a native
   `GENERATED ALWAYS AS (amount * 1.20) STORED`
6. Start the migration, confirm it reaches `COMPLETED`
7. **Verify**:
   ```sql
   SELECT id, amount, amount_with_tax FROM test_orders LIMIT 5;  -- amount_with_tax should equal amount * 1.20
   \d test_orders  -- the amount_with_tax column should be marked "generated"
   ```
8. **Large-table variant** (optional, repeat with a table above the
   size threshold): the expected strategy should be `EXPAND_BACKFILL`,
   and `\d test_orders` should **not** mark the column as generated
   (a plain column + `BEFORE INSERT/UPDATE` trigger simulation instead)
   — this is a documented, deliberate behavioral difference.

##### D. `PARTITION_TABLE`

1. **New Migration** → table: `test_orders` → Operation: `PARTITION_TABLE`
2. **Partition column**: `created_at`
3. **Partition strategy**: `RANGE`
4. **Interval** (shortcut): `monthly` (the JSON bounds field can be
   left empty)
5. Review the preview — expected: `SHADOW_TABLE` strategy
   (`PARTITION_TABLE` always uses this strategy, regardless of table
   size — see the internal architecture notes on this "exception")
6. Start the migration, watch progress (shadow table creation → sync →
   atomic swap)
7. Confirm it reaches `COMPLETED`
8. **Verify**:
   ```sql
   \d test_orders  -- should show "Partitioned table"
   SELECT count(*) FROM test_orders;  -- 100 rows (no data loss)
   SELECT tablename FROM pg_tables WHERE tablename LIKE 'test_orders%';  -- monthly partitions should appear
   ```

### 6.3. Scenario B — Database Migration (formerly "Upgrades")

1. Add test data to the source instance:
   ```bash
   docker compose -f deploy/docker-compose.dev.yml exec pg-logical psql -U pgarchimigrator -d pgarchimigrator_test -c "CREATE TABLE customers (id BIGINT PRIMARY KEY, name TEXT NOT NULL); INSERT INTO customers (id, name) SELECT g, 'customer-' || g FROM generate_series(1, 200) g;"
   ```
2. Go to **Database Migration → Start database migration**
3. Fill in the **Source** section: Host `localhost`, Port `55432`,
   Username `pgarchimigrator`, Password `pgarchimigrator_dev_only`,
   Database `pgarchimigrator_test`, SSL mode `Disable`
4. Fill in the **Target** section: same details, Port `55434`
5. Open **"+ Advanced: different replication address?"**, enter
   `pg-logical` as the **Replication host** (the port can be left
   blank — Source's own port is used)
6. Click **Start database migration**, confirm it navigates to the
   detail page
7. Watch the phases: `INTROSPECTING` → `SCHEMA_CREATED` → `SYNCING` →
   `VALIDATING` → `READY`
8. Once `READY`: confirm `Tables verified: 1/1`, and `Rows synced: 200`
   on the table's own row
9. Verify the data on the target:
   ```bash
   docker compose -f deploy/docker-compose.dev.yml exec pg-upgrade-target psql -U pgarchimigrator -d pgarchimigrator_test -c "SELECT count(*) FROM customers;"
   ```
10. **Retry test (review panel)**: start a database migration
    **without** filling in the `Advanced` field (Source Host
    `localhost`, Port `55432`) — confirm it reaches `FAILED` during the
    `SYNCING` phase with a `CREATE SUBSCRIPTION` connection error, and
    that the error message now includes this hint: *"this usually
    means the TARGET instance's own PostgreSQL server can't reach the
    source..."*
11. Click **"Retry this database migration"** — confirm a "Review
    before retrying" panel opens **before the API is called** (showing
    Source/Target host/username/database, with **the password never
    shown anywhere**), and confirm you see the *"No replication address
    override on file"* warning
12. Enter `pg-logical` in the panel's **"Replication host"** field,
    click **"Confirm retry"** — confirm it starts with a new job ID and
    reaches `READY` this time

### 6.4. Known Real Bug Classes (Regression Checklist)

These items are bugs **found during real manual testing** while this
project was being developed — each is now covered by an automated
test, but they're worth paying special attention to during manual
testing too (a new platform/PostgreSQL version/network topology could
surface a similar-class issue):

| # | Bug | Root Cause | Automated Test |
|---|---|---|---|
| 1 | `CREATE SUBSCRIPTION` "connection refused" | A single DSN isn't enough when source/target are reached from **different network locations** (e.g. Docker port-forwarding) | `TestStaticConnectionProvider_ReplicationDSN_*` |
| 2 | "no such column" (500 error) after a restart | `CREATE TABLE IF NOT EXISTS` doesn't add a new column to an **existing** table | `TestNewSQLiteStore_MigratesExistingDatabaseMissingNewerColumns` |
| 3 | `srsubstate` scan error | PostgreSQL's own internal `"char"` type (OID 18) can't be decoded directly to a `string` by `pgx` | `TestSyncProgress_ReadsSrsubstateWithoutScanError` |
| 4 | "Rows synced: 0" (not wrong, just missing data) | A storage method was defined but **never actually called** | `TestFlow_Run_EndToEnd_SmallTable`'s own `RowsSynced` check |
| 5 | Retry fails with the same error again **without a way to fix** a missing replication setting | Retry copies the original job's parameters **exactly as-is** — if they were wrong, they stay wrong | `TestRebuildReplicationRef_*`, `TestHandleRetryUpgrade_WithReplicationOverride_*` |
| 6 | The same connection error (`SQLSTATE 08006`) shown **without any explanation** to the user | The raw PostgreSQL error gave no hint that a replication override might be missing | `isConnectionFailureErr` + `Flow.sync`'s own enriched error message |

**General lesson**: **none** of these bugs could have been caught by
isolated unit tests alone — every one of them required either a real
two-instance network topology or real PostgreSQL system-catalog
behavior. This is concrete proof of why the integration tests in
Section 3.3 (and the manual scenarios in this section) are
indispensable.

## 7. Known Limitations / Out of Scope

- **Dev/test data provisioning** — deliberately removed from the
  roadmap (see the internal architecture notes), so there is no test
  coverage for this area at all.
- **Real PostgreSQL version-upgrade compatibility checks** — not built
  yet (planned as a separate future initiative).
- **Schema/table checkbox picker (Priority 3)** — was not yet
  implemented at the time this note was originally written; it has
  since been built (see Section 6.3's own scenario) — kept here as a
  historical note about how this document evolved alongside the
  feature.
- **`engines/postgresql/upgrade`'s ecosystem OAuth2 endpoint** (`POST
  /api/v1/upgrades`) has been tested in isolation, but never
  **end-to-end** with a real ArchiConsole integration (the ecosystem
  doesn't yet have a consumer actually wired up to this project).


