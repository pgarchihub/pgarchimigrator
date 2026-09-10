# Privilege Requirements on Managed PostgreSQL Providers

> This document honestly documents **what privileges** the native
> PostgreSQL logical replication (`CREATE PUBLICATION`/
> `CREATE SUBSCRIPTION`) that both the `SHADOW_TABLE` strategy
> (Zero-Downtime Migration) and `internal/engines/postgresql/upgrade` (Database Migration)
> depend on **actually require**, and what that means on **common
> managed PostgreSQL providers**. The goal: give a user a clear answer
> up front to "why doesn't this work?" instead of trial and error.

## 1. What Exactly Does This Tool Run?

Both mechanisms rely on **the same underlying PostgreSQL commands**:

- **`SHADOW_TABLE`** (`internal/engines/postgresql/shadowflow`) — for incompatible type
  changes on large tables and for `PARTITION_TABLE`, creates a shadow
  table **within the same instance** and keeps it in sync via logical
  replication.
- **`internal/engines/postgresql/upgrade`** (Database Migration) — moves a whole database
  via logical replication **between two separate instances**.

`internal/engines/postgresql/upgrade`'s `CreatePublication` function **always** uses
`CREATE PUBLICATION ... FOR TABLE <explicit list>` — **never**
`FOR ALL TABLES` (see `sync.go`'s own doc comment). This is a
**deliberate design decision** and directly affects the privilege
requirements below.

## 2. PostgreSQL's Own Privilege Requirements (by Version)

The information in this section is confirmed against PostgreSQL's own
official documentation (see the sources at the end).

### 2.1. `CREATE PUBLICATION` (Source Side)

| Usage | Privilege Required |
|---|---|
| `FOR TABLE <list>` — **what this tool uses** | `CREATE` privilege on the database + **ownership** of the listed tables |
| `FOR ALL TABLES` / `FOR TABLES IN SCHEMA` | **Superuser required** |

**Result**: since this tool uses `FOR TABLE`, **no superuser is needed
on the source side** — just a user who owns the tables being moved (or
has `ALTER`/ownership privilege on them). This is good news — most
application users already own their own tables.

### 2.2. `CREATE SUBSCRIPTION` (Target Side)

| PostgreSQL Version | Privilege Required |
|---|---|
| **16 and later** | The `pg_create_subscription` predefined role + `CREATE` privilege on the database |
| **15 and earlier** | **Superuser required** — no workaround |

The `pg_create_subscription` role was **added in PostgreSQL 16** (see
the sources). This has a real practical consequence:

> **If the target instance is on PostgreSQL 15 or earlier, this tool
> (both `SHADOW_TABLE` and `internal/engines/postgresql/upgrade`) needs a superuser
> account on the target (or a role that's nearly equivalent to
> superuser — see Section 3).** There is no way to run
> `CREATE SUBSCRIPTION` with a low-privilege application user on PG 15
> or earlier.

### 2.3. The Replication Connection (the Role Inside `CONNECTION`)

The role specified inside `CREATE SUBSCRIPTION ... CONNECTION '...'`
(the user the target uses to connect to the source — in this project,
`SourceConnectionRef`/`SourceReplicationRef`'s own username) also
needs, on the source side:

- **The `REPLICATION` attribute** (or superuser)
- **`SELECT` privilege** on the published tables (for the initial data
  copy — a detail worth noting explicitly, since the `REPLICATION`
  attribute alone does not guarantee this)

### 2.4. Server-Level Setting: `wal_level = logical`

This is a server configuration parameter that **no application user
can change via SQL** — it must be set by the **database
administrator/infrastructure owner**, and usually requires a restart.

## 3. Status on Common Managed Providers

### 3.1. AWS RDS for PostgreSQL

- **Server setting**: set the `rds.logical_replication` parameter
  group option to `1` — this also affects `wal_level`/
  `max_wal_senders`/`max_replication_slots`/`max_connections`, and
  **requires an instance restart**.
- **User roles**: the account needs **both `rds_superuser` and
  `rds_replication`** roles. These are not real PostgreSQL
  `SUPERUSER` (RDS doesn't grant real superuser access), but they are
  **still broad, privileged roles** — **typically not granted** to a
  low-privilege application user with access to only its own schema.
- **Practical consequence**: to use this tool on RDS, you'll need
  either the master user account (which already has these roles) or a
  separate account these roles have been **explicitly granted** to.

### 3.2. Azure Database for PostgreSQL (Flexible Server)

- **Server setting**: set the `wal_level` server parameter to
  `logical` (via the portal/CLI, requires a restart).
- **User roles**: an admin must run
  `ALTER ROLE <user> WITH REPLICATION;` for the role used in the
  replication connection. On PG 16+, `pg_create_subscription`
  membership on the target is enough; on PG 15 and earlier,
  `azure_pg_admin` membership (Azure's own closest equivalent to
  superuser) is needed.

### 3.3. Google Cloud SQL for PostgreSQL

- **Server setting**: turn on the `cloudsql.logical_decoding` instance
  flag.
- **User roles**: the user must be created with the `REPLICATION`
  attribute; in some scenarios (particularly compared to tools using
  the `pglogical` extension) the `cloudsqlsuperuser` role may also be
  needed — this tool does **not** use `pglogical` (it uses native
  PostgreSQL logical replication), so on PG 16+ targets `REPLICATION` +
  `pg_create_subscription` alone will likely be enough, but this is an
  **unverified assumption** that should be tested against a real Cloud
  SQL instance.

## 4. Practical Recommendations for This Tool

1. **Check the target instance's PostgreSQL version** — below 16, this
   tool **cannot work** without superuser (or the provider's own
   closest equivalent: `rds_superuser`+`rds_replication` on RDS,
   `azure_pg_admin` on Azure, `cloudsqlsuperuser` on GCP).
2. **No superuser needed on the source side** (thanks to this tool's
   own use of `FOR TABLE`) — just a user who owns the tables being
   moved, which is a **real advantage**: you don't need to request
   broad privileges on the source at all.
3. **`wal_level = logical` is always a prerequisite** — this isn't
   something the tool itself can configure; it's a step the user needs
   to handle **beforehand**. There is currently **no** preflight check
   in the application that verifies or explains this (see Section 5).
4. **When you get a connection error**, distinguishing whether it's a
   "network reachability" issue (see `SourceReplicationRef`) or a
   "missing privilege" issue currently falls to **the user themselves**
   — while PostgreSQL's own error messages are often clear (e.g.
   "permission denied to create subscription"), this tool doesn't
   **yet** make that distinction itself.

## 5. Out of Scope / Future Improvement Ideas

This document **only documents the current state** — the following
have not been implemented yet; this document itself can serve as the
rationale/starting point for them:

- **Preflight privilege checks**: before starting a database
  migration/upgrade, check **in advance** whether the required
  privileges (role memberships, `wal_level`) exist on both source and
  target, and give a **specific, actionable** error if not (similar to
  today's `isConnectionFailureErr`/`isDuplicateTableErr` pattern, but
  BEFORE attempting the connection).
- **Version-specific error enrichment**: when a `CREATE SUBSCRIPTION`
  "permission denied" error occurs, check the target's PostgreSQL
  version and add a hint like "superuser is required before PG 16."
- **Provider detection**: guess which managed provider is in use from
  the hostname pattern in the connection info (e.g.
  `*.rds.amazonaws.com`, `*.postgres.database.azure.com`,
  `*.sql.cloud.google.com`), and link to that provider's own specific
  setup instructions.

## Sources

- PostgreSQL 18 Docs — [Predefined Roles](https://www.postgresql.org/docs/current/predefined-roles.html)
- PostgreSQL 18 Docs — [CREATE SUBSCRIPTION](https://www.postgresql.org/docs/current/sql-createsubscription.html)
- PostgreSQL 19 Docs — [Logical Replication Security](https://www.postgresql.org/docs/19/logical-replication-security.html)
- AWS RDS Docs — [Performing logical replication for Amazon RDS for PostgreSQL](https://docs.aws.amazon.com/AmazonRDS/latest/UserGuide/PostgreSQL.Concepts.General.FeatureSupport.LogicalReplication.html)
- Microsoft Learn — [Logical replication and logical decoding in Azure Database for PostgreSQL](https://learn.microsoft.com/en-us/azure/postgresql/flexible-server/concepts-logical)
- AWS DMS Docs — [Using Google Cloud for PostgreSQL as a source](https://docs.aws.amazon.com/dms/latest/userguide/CHAP_Source.GCPostgres.html) (a third-party source touching on GCP Cloud SQL privilege requirements)
