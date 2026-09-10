package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// minPostgresVersionNum is the minimum supported PostgreSQL version per
// TR-11 (in server_version_num format, e.g. 12 -> 120000).
const minPostgresVersionNum = 120000

// PgxPreflighter is the pgx-based concrete implementation of the
// Preflighter interface.
type PgxPreflighter struct {
	Pool *pgxpool.Pool
}

// NewPgxPreflighter creates a PgxPreflighter with the given connection pool.
func NewPgxPreflighter(pool *pgxpool.Pool) *PgxPreflighter {
	return &PgxPreflighter{Pool: pool}
}

var _ Preflighter = (*PgxPreflighter)(nil)

// CheckShadowTablePreconditions collects the "Preconditions" list from
// Architecture Doc Section 3.2 into a single result. Each check runs
// independently, and failures are appended to PreflightResult.Errors — so
// the user sees every missing precondition at once instead of discovering
// them one retry at a time.
func (p *PgxPreflighter) CheckShadowTablePreconditions(ctx context.Context, schema, table string) (*PreflightResult, error) {
	result := &PreflightResult{}
	qualifiedName := fmt.Sprintf("%s.%s", schema, table)

	if err := p.checkWALLevel(ctx, result); err != nil {
		return nil, fmt.Errorf("error while checking wal_level: %w", err)
	}

	if err := p.checkReplicationRole(ctx, result); err != nil {
		return nil, fmt.Errorf("error while checking replication privileges: %w", err)
	}

	if err := p.checkPrimaryKey(ctx, qualifiedName, result); err != nil {
		return nil, fmt.Errorf("error while checking primary key: %w", err)
	}

	if err := p.checkPostgresVersion(ctx, result); err != nil {
		return nil, fmt.Errorf("error while checking PostgreSQL version: %w", err)
	}

	return result, nil
}

// checkWALLevel checks via "SHOW wal_level" (Architecture Doc 3.2, precondition 1).
func (p *PgxPreflighter) checkWALLevel(ctx context.Context, result *PreflightResult) error {
	var walLevel string
	if err := p.Pool.QueryRow(ctx, "SHOW wal_level").Scan(&walLevel); err != nil {
		return err
	}

	result.WALLevelLogical = walLevel == "logical"
	if !result.WALLevelLogical {
		result.Errors = append(result.Errors, fmt.Sprintf(
			"wal_level='%s' but must be 'logical'. This setting requires a "+
				"PostgreSQL restart; on managed services (RDS/Cloud SQL/Azure "+
				"Database) it may require a parameter group change plus a "+
				"maintenance window (see Architecture Doc Section 3.2).",
			walLevel,
		))
	}
	return nil
}

// checkReplicationRole checks whether the current user has the REPLICATION
// role or superuser privileges (Architecture Doc 3.2, precondition 3;
// superuser is only detected here, not "required" — see TR-06 "Least
// Privilege").
func (p *PgxPreflighter) checkReplicationRole(ctx context.Context, result *PreflightResult) error {
	var hasRole bool
	query := `SELECT rolreplication OR rolsuper FROM pg_roles WHERE rolname = current_user`
	if err := p.Pool.QueryRow(ctx, query).Scan(&hasRole); err != nil {
		return err
	}

	result.HasReplicationRole = hasRole
	if !hasRole {
		result.Errors = append(result.Errors, fmt.Sprintf(
			"user '%s' does not have the REPLICATION role. "+
				"Grant it with 'ALTER ROLE %s WITH REPLICATION;'.",
			currentUserPlaceholder, currentUserPlaceholder,
		))
	}
	return nil
}

// currentUserPlaceholder is a simple placeholder used in the error message
// to avoid an extra query just to fetch current_user. TODO: fetch the real
// current_user value in a separate query and embed it in the message.
const currentUserPlaceholder = "<current_user>"

// checkPrimaryKey implements Architecture Doc Section 3.2 precondition 2.
//
// IMPORTANT: the relreplident column is NOT ENOUGH by itself — every table,
// whether or not it has a PK, is created with relreplident='d' (default).
// The actual PRIMARY KEY must be verified via pg_constraint (this
// distinction was confirmed against a test table with relreplident='d' but
// no PK during development).
func (p *PgxPreflighter) checkPrimaryKey(ctx context.Context, qualifiedTable string, result *PreflightResult) error {
	var pkCount int
	query := `
		SELECT count(*)
		FROM pg_constraint
		WHERE conrelid = $1::regclass AND contype = 'p'
	`
	if err := p.Pool.QueryRow(ctx, query, qualifiedTable).Scan(&pkCount); err != nil {
		return err
	}

	result.TargetHasPrimaryKey = pkCount > 0

	// Informational only: also read the REPLICA IDENTITY mode (not used in
	// the error decision, but useful for the audit log and for the message
	// shown to the user).
	var replicaIdentity string
	identityQuery := `
		SELECT CASE relreplident
			WHEN 'd' THEN 'DEFAULT'
			WHEN 'n' THEN 'NOTHING'
			WHEN 'f' THEN 'FULL'
			WHEN 'i' THEN 'INDEX'
			ELSE 'UNKNOWN'
		END
		FROM pg_class WHERE oid = $1::regclass
	`
	if err := p.Pool.QueryRow(ctx, identityQuery, qualifiedTable).Scan(&replicaIdentity); err != nil {
		return err
	}
	result.ReplicaIdentity = replicaIdentity

	if !result.TargetHasPrimaryKey {
		result.Errors = append(result.Errors, fmt.Sprintf(
			"table '%s' has no PRIMARY KEY (checked via pg_constraint, contype='p'). "+
				"The shadow-table strategy cannot work without a PRIMARY KEY "+
				"(Architecture Doc Section 3.2). Current REPLICA IDENTITY mode: %s "+
				"(this alone is not sufficient).",
			qualifiedTable, replicaIdentity,
		))
	}
	return nil
}

// maxTestedPostgresVersionNum is the newest PostgreSQL major version this
// project has actually been validated against (in server_version_num
// format, matching minPostgresVersionNum's convention) — NOT a hard
// ceiling. A version newer than this doesn't fail the preflight check
// the way an older-than-minimum version does; it's surfaced as an
// informational note instead (see PreflightResult.Warnings), since a
// newer PostgreSQL is far more likely to work fine than to break
// anything — but "far more likely" isn't "confirmed", and this project's
// own investigation history (a load test found the query planner didn't
// behave as documentation/experience from older versions suggested,
// even on a version well within the previously-tested range) is a
// concrete reason not to silently assume every future version needs no
// verification. Bump this only after actually testing against the new
// version (see the CI matrix in .github/workflows/ci.yml, which runs the
// full integration suite against every version from
// minPostgresVersionNum through this one).
const maxTestedPostgresVersionNum = 180000

// checkPostgresVersion implements TR-11 (minimum PostgreSQL 12) and adds
// an informational (non-blocking) note when the connected server is
// newer than maxTestedPostgresVersionNum.
func (p *PgxPreflighter) checkPostgresVersion(ctx context.Context, result *PreflightResult) error {
	versionNum, _, err := FetchPostgresVersion(ctx, p.Pool)
	if err != nil {
		return err
	}
	result.PostgresVersion = versionNum

	switch ClassifyVersion(versionNum) {
	case VersionStatusBelowMinimum:
		result.Errors = append(result.Errors, fmt.Sprintf(
			"PostgreSQL version is %d, minimum supported version is 12 (TR-11). "+
				"PostgreSQL 10-11 is marked experimental/unsupported.",
			result.PostgresVersion,
		))
	case VersionStatusNewerThanTested:
		result.Warnings = append(result.Warnings, fmt.Sprintf(
			"PostgreSQL version is %d — newer than %d, the newest version this project has been "+
				"explicitly tested against. This will very likely still work fine, but hasn't been confirmed yet.",
			result.PostgresVersion, maxTestedPostgresVersionNum/10000,
		))
	}
	return nil
}
