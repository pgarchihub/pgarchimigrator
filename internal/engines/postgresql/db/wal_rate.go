package db

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// SampleWALGenerationRate measures how fast the server is currently
// generating WAL, by taking two pg_current_wal_lsn() readings separated
// by sampleDuration and computing the byte rate between them.
//
// Why this exists — found the hard way, via a real load test: a
// SHADOW_TABLE migration's delta sync can, under heavy sustained write
// load, never converge (see internal/api's attachReplicationLag doc
// comment for the full incident). Knowing the CURRENT write rate before
// starting such a migration lets an operator make an informed choice
// about timing, rather than discovering the problem 30+ minutes into a
// migration that will never finish.
//
// This blocks the caller for the full sampleDuration — deliberately not
// something that runs automatically as part of every migration; see
// internal/api's handleEstimateWriteLoad for why this is opt-in.
func SampleWALGenerationRate(ctx context.Context, pool *pgxpool.Pool, sampleDuration time.Duration) (float64, error) {
	var startLSN string
	if err := pool.QueryRow(ctx, `SELECT pg_current_wal_lsn()::text`).Scan(&startLSN); err != nil {
		return 0, fmt.Errorf("failed to read starting WAL position: %w", err)
	}

	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-time.After(sampleDuration):
	}

	var bytesGenerated int64
	if err := pool.QueryRow(ctx, `SELECT pg_wal_lsn_diff(pg_current_wal_lsn(), $1::pg_lsn)`, startLSN).Scan(&bytesGenerated); err != nil {
		return 0, fmt.Errorf("failed to read ending WAL position: %w", err)
	}

	return float64(bytesGenerated) / sampleDuration.Seconds(), nil
}
