package db

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5/pgxpool"
)

// VersionSupportStatus categorizes a PostgreSQL major version relative to
// this project's supported range — computed once (see ClassifyVersion) so
// the frontend never needs to duplicate the
// [minPostgresVersionNum, maxTestedPostgresVersionNum] thresholds itself,
// avoiding any risk of the two sides drifting apart.
type VersionSupportStatus string

const (
	// VersionStatusUnknown means PostgresVersion hasn't been determined
	// yet (e.g. before the first successful version query) — not an
	// error state, just "no opinion yet".
	VersionStatusUnknown VersionSupportStatus = ""
	// VersionStatusBelowMinimum means the version is older than
	// minPostgresVersionNum (TR-11) — StartMigration's VersionCheck
	// refuses to run any migration in this state; this status exists so
	// the UI can show the SAME fact before the operator even attempts one.
	VersionStatusBelowMinimum VersionSupportStatus = "below_minimum"
	// VersionStatusSupported means the version falls within
	// [minPostgresVersionNum, maxTestedPostgresVersionNum] — the range
	// this project's CI test matrix (.github/workflows/ci.yml) actually
	// runs the full integration suite against.
	VersionStatusSupported VersionSupportStatus = "supported"
	// VersionStatusNewerThanTested means the version is newer than
	// maxTestedPostgresVersionNum — not blocked (very likely to work
	// fine), but not yet confirmed by this project's own CI either. See
	// maxTestedPostgresVersionNum's doc comment for why "very likely" is
	// deliberately not treated as "confirmed".
	VersionStatusNewerThanTested VersionSupportStatus = "newer_than_tested"
)

// ClassifyVersion maps a major version number (e.g. 16) to its
// VersionSupportStatus. Shared by checkPostgresVersion (which populates
// PreflightResult.Warnings from it) and main.go (which populates
// ConnectionInfo.VersionSupportStatus for the API/frontend) — one
// classification function, not two copies of the same two comparisons.
func ClassifyVersion(majorVersion int) VersionSupportStatus {
	if majorVersion == 0 {
		return VersionStatusUnknown
	}
	versionNum := majorVersion * 10000
	switch {
	case versionNum < minPostgresVersionNum:
		return VersionStatusBelowMinimum
	case versionNum > maxTestedPostgresVersionNum:
		return VersionStatusNewerThanTested
	default:
		return VersionStatusSupported
	}
}

// ConnectionInfo is the non-sensitive subset of a PostgreSQL DSN — safe
// to expose over the REST API (see internal/api's handleGetConnectionInfo)
// so the New Migration screen can show "which database am I about to
// change" without ever transmitting or displaying the password. The
// password is deliberately NOT a field on this struct at all, not just
// omitted from JSON — there is no way to accidentally leak it through
// this type.
//
// PostgresVersion/PostgresVersionString are populated separately from
// the other fields (see FetchPostgresVersion) — unlike Host/Port/
// Username/Database, which come straight from parsing the DSN string,
// the server version can only be learned by actually querying the live
// connection, so these two default to zero-value (0, "") until that
// query has run at least once. Both are 0/"" is a legitimate,
// non-error state (e.g. right at startup before the first successful
// connection), not something callers need to treat as a bug.
type ConnectionInfo struct {
	Host     string
	Port     uint16
	Username string
	Database string

	// PostgresVersion is the server's major version number (e.g. 16 for
	// PostgreSQL 16.15) — server_version_num divided by 10000, matching
	// the same derivation internal/db's preflight check already used.
	PostgresVersion int
	// PostgresVersionString is the full human-readable version string
	// PostgreSQL itself reports (e.g. "PostgreSQL 16.15 (Debian
	// 16.15-1.pgdg13+2) on x86_64-pc-linux-gnu, ..."), from `SELECT
	// version()` — shown as-is in the UI rather than reconstructed,
	// since PostgreSQL's own formatting already includes everything an
	// operator would want to see (build info, platform) without this
	// project needing to keep that formatting in sync.
	PostgresVersionString string
	// VersionSupportStatus is ClassifyVersion(PostgresVersion) — computed
	// once at the same time PostgresVersion is populated (see main.go),
	// not left for API consumers to recompute from the raw number.
	VersionSupportStatus VersionSupportStatus
}

// ParseConnectionInfo extracts ConnectionInfo from a PostgreSQL DSN (the
// same PGARCHIMIGRATOR_DATABASE_URL used to build the connection pool itself),
// using pgxpool's own parser rather than hand-rolling URL parsing — this
// guarantees it accepts exactly the same DSN forms pgxpool.New does
// (postgres://, key=value, etc.), with no risk of the two disagreeing on
// what's valid. Does NOT populate PostgresVersion/PostgresVersionString —
// see FetchPostgresVersion for that, which needs an actual live
// connection this function never makes.
func ParseConnectionInfo(dsn string) (ConnectionInfo, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return ConnectionInfo{}, fmt.Errorf("failed to parse database DSN: %w", err)
	}
	return ConnectionInfo{
		Host:     cfg.ConnConfig.Host,
		Port:     cfg.ConnConfig.Port,
		Username: cfg.ConnConfig.User,
		Database: cfg.ConnConfig.Database,
	}, nil
}

// ValidateMinimumVersion implements TR-11's minimum-version guarantee as
// a standalone, reusable check — independent of the shadow-table-specific
// PgxPreflighter.checkPostgresVersion (which ALSO enforces this as part
// of a larger precondition list, but only runs for the SHADOW_TABLE
// strategy). This is the one internal/orchestrator calls for EVERY
// migration regardless of strategy — added specifically to close a real
// gap: before this existed, a DIRECT_DDL or EXPAND_BACKFILL migration
// against a PostgreSQL 10/11 server ran with NO version check at all,
// silently exposed to internal/ddlflow's PG11+ "fast path ADD COLUMN"
// assumption not actually holding (see internal/ddlflow's own doc
// comments on why that assumption specifically requires PG11+).
func ValidateMinimumVersion(ctx context.Context, pool *pgxpool.Pool) error {
	versionNum, _, err := FetchPostgresVersion(ctx, pool)
	if err != nil {
		return fmt.Errorf("failed to determine PostgreSQL version: %w", err)
	}
	if versionNum*10000 < minPostgresVersionNum {
		return fmt.Errorf(
			"PostgreSQL version is %d, minimum supported version is %d (TR-11); PostgreSQL 10-11 is marked experimental/unsupported",
			versionNum, minPostgresVersionNum/10000,
		)
	}
	return nil
}

// FetchPostgresVersion queries the live connection for its version — the
// single source of truth this whole project uses for version detection,
// shared by internal/db's own preflight check (checkPostgresVersion),
// internal/orchestrator's general minimum-version gate (added
// specifically because the shadow-table-only preflight previously left
// DIRECT_DDL/EXPAND_BACKFILL migrations completely unchecked against
// TR-11), and the connection-info banner the New Migration screen shows.
// One query, reused everywhere, rather than three independent copies of
// "SHOW server_version_num" that could silently drift apart.
func FetchPostgresVersion(ctx context.Context, pool *pgxpool.Pool) (versionNum int, versionString string, err error) {
	var rawVersionNum string
	// SHOW server_version_num is a SHOW command and is always returned by
	// the pgx driver in TEXT format (OID 25) — it cannot be Scan'd
	// directly into an int, it must be read as a string first (see
	// checkPostgresVersion's identical historical note).
	if err := pool.QueryRow(ctx, "SHOW server_version_num").Scan(&rawVersionNum); err != nil {
		return 0, "", fmt.Errorf("failed to read server_version_num: %w", err)
	}
	rawNum, err := strconv.Atoi(rawVersionNum)
	if err != nil {
		return 0, "", fmt.Errorf("could not convert server_version_num (%q) to a number: %w", rawVersionNum, err)
	}

	var fullVersion string
	if err := pool.QueryRow(ctx, "SELECT version()").Scan(&fullVersion); err != nil {
		return 0, "", fmt.Errorf("failed to read version(): %w", err)
	}

	return rawNum / 10000, fullVersion, nil // e.g. 160003 -> 16
}
