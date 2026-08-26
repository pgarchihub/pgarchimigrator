// replication.go manages the PostgreSQL PUBLICATION and replication slot
// that back the Decoder/SyncEngine described in Architecture Doc Section
// 3.2 and 4.1. A PUBLICATION is required for the pgoutput plugin to know
// which table's changes to stream; the replication slot retains WAL from
// its creation point onward so no committed change is ever lost between
// slot creation and the moment Delta Sync starts consuming it.
package shadowflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CreatePublication creates a PUBLICATION that streams changes for a single
// table. Idempotent: safe to call if a publication with the same name
// already exists from a previous, aborted attempt (the caller/orchestrator
// is responsible for choosing a unique name per job, e.g. via the job ID,
// so collisions across unrelated jobs are avoided).
func CreatePublication(ctx context.Context, pool *pgxpool.Pool, schema, table, publicationName string) error {
	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM pg_publication WHERE pubname = $1)`, publicationName,
	).Scan(&exists); err != nil {
		return fmt.Errorf("failed to check for existing publication: %w", err)
	}
	if exists {
		return nil
	}

	ddl := fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s.%s",
		quoteIdent(publicationName), quoteIdent(schema), quoteIdent(table))
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("failed to create publication %q: %w", publicationName, err)
	}
	return nil
}

// DropPublicationIfExists is used during Cleanup (Architecture Doc Section
// 4.1 step 7) and by the Reaper for orphaned publications left behind by a
// crashed job.
func DropPublicationIfExists(ctx context.Context, pool *pgxpool.Pool, publicationName string) error {
	ddl := fmt.Sprintf("DROP PUBLICATION IF EXISTS %s", quoteIdent(publicationName))
	if _, err := pool.Exec(ctx, ddl); err != nil {
		return fmt.Errorf("failed to drop publication %q: %w", publicationName, err)
	}
	return nil
}

// CreateReplicationSlotAndGetStartLSN opens a dedicated connection in
// replication mode (required by the wire protocol — a normal pooled
// connection cannot issue CREATE_REPLICATION_SLOT), creates a permanent
// logical slot using the pgoutput plugin, and returns the LSN at which
// Delta Sync must start consuming.
//
// Design note (deliberate simplification vs. exported-snapshot isolation):
// the slot is created BEFORE Initial Sync begins, so the WAL from this LSN
// onward is guaranteed to be retained — nothing committed after this point
// can be lost. Initial Sync's row-by-row COPY may therefore overlap with
// some early WAL records (i.e. a row copied by Initial Sync might also
// arrive again via Delta Sync). This is safe ONLY because ApplyEngine.Apply
// is idempotent (UPSERT/DELETE keyed by primary key) — see apply.go.
func CreateReplicationSlotAndGetStartLSN(ctx context.Context, replicationDSN, slotName string) (pglogrepl.LSN, error) {
	conn, err := pgconn.Connect(ctx, replicationDSN)
	if err != nil {
		return 0, fmt.Errorf("failed to open a replication-mode connection: %w", err)
	}
	defer conn.Close(ctx)

	result, err := pglogrepl.CreateReplicationSlot(ctx, conn, slotName, "pgoutput",
		pglogrepl.CreateReplicationSlotOptions{Temporary: false, SnapshotAction: "NOEXPORT_SNAPSHOT"})
	if err != nil {
		return 0, fmt.Errorf("failed to create replication slot %q: %w", slotName, err)
	}

	startLSN, err := pglogrepl.ParseLSN(result.ConsistentPoint)
	if err != nil {
		return 0, fmt.Errorf("failed to parse the slot's consistent point LSN (%q): %w", result.ConsistentPoint, err)
	}
	return startLSN, nil
}

// ReplicationDSN appends the `replication=database` query parameter
// required to open a connection in logical-replication mode, as opposed to
// the normal pgxpool.Pool used everywhere else in this codebase.
func ReplicationDSN(baseDSN string) string {
	if strings.Contains(baseDSN, "?") {
		return baseDSN + "&replication=database"
	}
	return baseDSN + "?replication=database"
}

// DropReplicationSlot drops a replication slot unconditionally (it errors
// if the slot doesn't exist) — used during a successful Cleanup step
// (Architecture Doc Section 4.1 step 7). For crash-recovery cleanup of an
// orphaned slot, see internal/reaper instead, which checks for existence
// first before dropping.
func DropReplicationSlot(ctx context.Context, pool *pgxpool.Pool, slotName string) error {
	if _, err := pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slotName); err != nil {
		return fmt.Errorf("failed to drop replication slot %q: %w", slotName, err)
	}
	return nil
}
