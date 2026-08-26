// table_stats.go fetches the raw table statistics internal/strategy.Decide
// needs to choose a migration strategy (Architecture Doc Section 4.0).
// Unlike Preflighter, which specifically checks shadow-table-only
// preconditions (wal_level, replication role), TableStats is needed
// regardless of which strategy ends up being chosen.
package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TableStats holds the raw statistics used by strategy.Decide.
type TableStats struct {
	EstimatedRowCount int64
	IsPartitioned     bool
	HasPrimaryKey     bool
	ReplicaIdentity   string
}

// FetchTableStats queries pg_class/pg_constraint for the statistics needed
// to decide a migration strategy. EstimatedRowCount comes from
// pg_class.reltuples (a planner estimate, not an exact count — sufficient
// for FR-01's < 1M-row threshold decision and far cheaper than an exact
// COUNT(*) on a potentially huge table).
func FetchTableStats(ctx context.Context, pool *pgxpool.Pool, schema, table string) (*TableStats, error) {
	qualifiedTable := fmt.Sprintf("%s.%s", schema, table)

	query := `
		SELECT
			GREATEST(c.reltuples, 0)::bigint AS estimated_row_count,
			c.relkind = 'p' AS is_partitioned,
			EXISTS(
				SELECT 1 FROM pg_constraint pc
				WHERE pc.conrelid = c.oid AND pc.contype = 'p'
			) AS has_primary_key,
			CASE c.relreplident
				WHEN 'd' THEN 'DEFAULT'
				WHEN 'n' THEN 'NOTHING'
				WHEN 'f' THEN 'FULL'
				WHEN 'i' THEN 'INDEX'
				ELSE 'UNKNOWN'
			END AS replica_identity
		FROM pg_class c
		WHERE c.oid = $1::regclass
	`

	var stats TableStats
	err := pool.QueryRow(ctx, query, qualifiedTable).Scan(
		&stats.EstimatedRowCount, &stats.IsPartitioned, &stats.HasPrimaryKey, &stats.ReplicaIdentity,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch table stats for %s: %w", qualifiedTable, err)
	}
	return &stats, nil
}
