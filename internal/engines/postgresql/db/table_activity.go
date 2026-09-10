package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TableActivity is a point-in-time snapshot of query activity currently
// touching a specific table — see FetchTableActivity's doc comment for
// how "touching" is determined and why.
type TableActivity struct {
	// ActiveQueries is how many sessions are currently holding or
	// waiting for a lock on this table.
	ActiveQueries int
	// MaxDurationSeconds is the longest-running of those queries, in
	// seconds — the single most useful number for "is this migration
	// making something slow right now", since a handful of fast queries
	// alongside one stuck one is a very different situation than several
	// moderately slow ones.
	MaxDurationSeconds float64
}

// FetchTableActivity reports how many sessions are CURRENTLY holding or
// waiting for a lock on the given table, and how long the longest-running
// of them has been active — used for an opt-in "what is this migration
// doing to real query latency right now" indicator (see
// pgArchiMigrator_Guven_Katmani_Tasarimi.md's Faz 2.4).
//
// Deliberately joins through pg_locks rather than text-matching
// pg_stat_activity.query against the table name: query text matching is
// fragile (a query mentioning the table name in a comment, a string
// literal, or a completely unrelated table with a similar name would all
// false-positive), while a session actually holding or waiting for a
// lock on this table's relation OID is a precise, unambiguous signal
// that it is, in fact, interacting with this specific table right now.
//
// Only counts sessions in the 'active' state (currently executing) —
// idle-in-transaction sessions holding a lock but not actively running
// anything wouldn't reflect real query latency.
func FetchTableActivity(ctx context.Context, pool *pgxpool.Pool, schema, table string) (TableActivity, error) {
	var activity TableActivity
	var maxDuration *float64 // NULL when ActiveQueries is 0 (no rows to aggregate over)

	err := pool.QueryRow(ctx, `
		SELECT
			count(*),
			max(EXTRACT(EPOCH FROM (now() - a.query_start)))
		FROM pg_locks l
		JOIN pg_class c ON l.relation = c.oid
		JOIN pg_namespace n ON c.relnamespace = n.oid
		JOIN pg_stat_activity a ON l.pid = a.pid
		WHERE n.nspname = $1 AND c.relname = $2
		  AND a.state = 'active'
		  AND a.pid != pg_backend_pid()
	`, schema, table).Scan(&activity.ActiveQueries, &maxDuration)
	if err != nil {
		return TableActivity{}, fmt.Errorf("failed to query table activity for %s.%s: %w", schema, table, err)
	}

	if maxDuration != nil {
		activity.MaxDurationSeconds = *maxDuration
	}
	return activity, nil
}
