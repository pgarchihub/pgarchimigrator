// Package db manages PostgreSQL connections and implements Architecture Doc
// Section 4.1 "Preflight Check". Preflight reports missing preconditions with
// a clear error before the operation starts — no silent fallback.
package db

import (
	"context"
	"fmt"
)

// PreflightResult carries the outcome of the preflight check and any
// missing preconditions.
type PreflightResult struct {
	WALLevelLogical     bool
	HasReplicationRole  bool
	TargetHasPrimaryKey bool
	ReplicaIdentity     string
	PostgresVersion     int // TR-11: minimum supported version is 12
	Errors              []string
	// Warnings are informational, non-blocking notes — unlike Errors,
	// their presence never prevents a migration from proceeding. See
	// checkPostgresVersion's maxTestedPostgresVersionNum note for the
	// motivating case: a newer-than-tested PostgreSQL version is worth
	// flagging, but not worth refusing to run against.
	Warnings []string
}

// Preflighter defines the mandatory checks required before the shadow-table
// flow can run. Concrete implementation (pgx-based `SHOW wal_level`,
// `pg_roles`, `pg_constraint` queries) lives in
// internal/db/pgx_preflighter.go.
type Preflighter interface {
	// CheckShadowTablePreconditions applies the "Preconditions" list from
	// Architecture Doc Section 3.2:
	//  - wal_level = logical
	//  - REPLICA IDENTITY DEFAULT or FULL (i.e. a PRIMARY KEY)
	//  - the user has the REPLICATION role or pg_read_all_data + slot privileges
	//  - PostgreSQL >= 12 (TR-11)
	CheckShadowTablePreconditions(ctx context.Context, schema, table string) (*PreflightResult, error)
}

// Validate turns the missing preconditions in PreflightResult into a single
// meaningful error.
// US-05: "Any missing precondition is reported to me at setup time with a
// clear error and guidance."
func (r *PreflightResult) Validate() error {
	if len(r.Errors) == 0 {
		return nil
	}
	msg := "preflight check failed, the shadow-table strategy cannot be used:\n"
	for _, e := range r.Errors {
		msg += "  - " + e + "\n"
	}
	return fmt.Errorf("%s", msg)
}

// TODO: pgx.Pool-based concrete Preflighter implementation.
// Example check queries (implementation notes):
//   SHOW wal_level;
//   SELECT rolreplication OR rolsuper FROM pg_roles WHERE rolname = current_user;
//
//   -- PRIMARY KEY check: relreplident is NOT ENOUGH, because every table is
//   -- created with relreplident='d' (default) whether or not it has a PK.
//   -- The actual PK must be verified via pg_constraint:
//   SELECT conname FROM pg_constraint
//     WHERE conrelid = '<schema.table>'::regclass AND contype = 'p';
//   -- (0 rows returned means no PK -> the shadow-table strategy should be rejected, see Architecture Doc 3.2)
//
//   -- relreplident can still be used as supplementary information about the
//   -- REPLICA IDENTITY mode:
//   SELECT relreplident FROM pg_class WHERE oid = '<schema.table>'::regclass;
//   SHOW server_version_num;
