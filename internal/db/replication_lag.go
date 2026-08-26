package db

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ReplicationLag is a point-in-time snapshot of a logical replication
// slot's lag.
type ReplicationLag struct {
	// LagBytes is the byte distance between the server's current WAL
	// position and the last position this slot's consumer has confirmed
	// flushing (`pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)`).
	// 0 both for "genuinely caught up" and for "slot exists but hasn't
	// confirmed any position yet" (confirmed_flush_lsn is NULL right
	// after slot creation) — these aren't distinguished here; both are
	// "nothing concerning to report yet".
	LagBytes int64
	Active   bool
}

// FetchReplicationLag queries pg_replication_slots for a single named
// slot's current lag — used to show a live "is this SHADOW_TABLE
// migration's delta sync actually catching up, or falling further
// behind?" indicator (see pgArchiMigrator_Guven_Katmani_Tasarimi.md's
// Faz 2.1, "Canlı Replication Lag / Yakınsama Göstergesi").
//
// Why this exists — found the hard way, via a real load test: a
// SHADOW_TABLE migration's delta-sync phase can, under heavy sustained
// write load, never converge — the ApplyEngine's decode+apply throughput
// simply can't keep pace with incoming WAL. Without this signal, that
// looked IDENTICAL, from the outside, to a migration that's healthy but
// just slow: nothing in the product surfaced it, and diagnosing a real
// multi-hour SYNCING phase needed direct pg_stat_activity/
// pg_replication_slots inspection with a psql client, not anything this
// project's own UI showed.
//
// Returns (ReplicationLag{}, false, nil) — not an error — if no slot with
// this name currently exists (the migration hasn't reached the phase
// that creates one yet, or has already finished and cleaned it up); this
// is a normal, expected state, not a failure, and callers should treat
// it as "nothing to show" rather than surfacing an error.
func FetchReplicationLag(ctx context.Context, pool *pgxpool.Pool, slotName string) (ReplicationLag, bool, error) {
	var active bool
	var lagBytes *int64 // nullable: confirmed_flush_lsn can be NULL right after slot creation

	err := pool.QueryRow(ctx, `
		SELECT active, pg_wal_lsn_diff(pg_current_wal_lsn(), confirmed_flush_lsn)
		FROM pg_replication_slots
		WHERE slot_name = $1
	`, slotName).Scan(&active, &lagBytes)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ReplicationLag{}, false, nil
		}
		return ReplicationLag{}, false, fmt.Errorf("failed to query replication lag for slot %q: %w", slotName, err)
	}

	lag := ReplicationLag{Active: active}
	if lagBytes != nil {
		lag.LagBytes = *lagBytes
	}
	return lag, true, nil
}
