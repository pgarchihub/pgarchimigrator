package db

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckpointPressure reports whether PostgreSQL's checkpoints are being
// forced more often than scheduled — see FetchCheckpointPressure's doc
// comment for the incident this exists to surface.
type CheckpointPressure struct {
	RequestedCheckpoints int64
	TimedCheckpoints     int64
	// Pressured is true when requested (forced-early) checkpoints
	// substantially outnumber timed (scheduled) ones — see
	// FetchCheckpointPressure for the exact threshold and reasoning.
	Pressured bool
}

// FetchCheckpointPressure queries checkpoint statistics — from
// pg_stat_bgwriter on PostgreSQL < 17, or pg_stat_checkpointer on 17+.
// PostgreSQL 17 split checkpointer statistics into their own view,
// removing checkpoints_timed/checkpoints_req from pg_stat_bgwriter
// entirely (renamed num_timed/num_requested on the new view) — this
// project supports PostgreSQL 12 through 18 (see README.md's "Supported
// PostgreSQL Versions"), so a single query cannot work across the whole
// range; majorVersion selects which one to run.
//
// Why this exists — found the hard way, via a real load test: PostgreSQL
// logged "checkpoints are occurring too frequently" warnings every 6-22
// seconds under heavy sustained write load (the default max_wal_size was
// far too small for the combined traffic), and one checkpoint took 90
// SECONDS to complete — directly causing multi-second query latency
// spikes that were, at first, reasonably mistaken for an application-
// level bug before pg_stat_activity and the PostgreSQL log ruled that
// out (see the launch guide's "B.4b" section for the full incident).
// Nothing in this product surfaced this possibility to a DBA before now
// — diagnosing it needed direct server log access.
//
// A checkpoint is "requested" (forced early) when WAL volume hits
// max_wal_size before the next scheduled ("timed") checkpoint would
// naturally occur — a requested count substantially exceeding the timed
// count is the standard, widely-documented signal that max_wal_size is
// undersized for the current write volume.
func FetchCheckpointPressure(ctx context.Context, pool *pgxpool.Pool, majorVersion int) (CheckpointPressure, error) {
	query := `SELECT checkpoints_req, checkpoints_timed FROM pg_stat_bgwriter`
	if majorVersion >= 17 {
		query = `SELECT num_requested, num_timed FROM pg_stat_checkpointer`
	}

	var requested, timed int64
	if err := pool.QueryRow(ctx, query).Scan(&requested, &timed); err != nil {
		return CheckpointPressure{}, fmt.Errorf("failed to query checkpoint statistics: %w", err)
	}

	return CheckpointPressure{
		RequestedCheckpoints: requested,
		TimedCheckpoints:     timed,
		Pressured:            isCheckpointPressured(requested, timed),
	}, nil
}

// isCheckpointPressured is the pure threshold logic FetchCheckpointPressure
// applies to the two raw counters — split out so it's testable without a
// real PostgreSQL connection. A minimum requested count avoids flagging a
// freshly-started or very quiet server, where a couple of requested
// checkpoints among zero/few timed ones isn't meaningful yet.
func isCheckpointPressured(requested, timed int64) bool {
	return requested >= 5 && requested > timed
}
