// Package shadowflow implements Architecture Doc Section 4.1 "Zero-Downtime
// Migration Flow (Shadow Table Path)" and Section 4.3 "Dependent Object
// Migration Plan". This is the flow that delivers the tool's core value
// proposition (e.g. an incompatible ALTER COLUMN TYPE).
package shadowflow

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/jackc/pglogrepl"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/monitor"
	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
)

// defaultRollbackWindow matches Requirements Doc FR-08a's default.
const defaultRollbackWindow = 10 * time.Minute

// settleWindow is a deliberate, honestly-documented simplification: instead
// of measuring true replication lag (comparing the slot's confirmed_flush
// position against the source's current WAL position), Execute waits this
// long after Initial Sync completes to let Delta Sync drain any remaining
// backlog before proceeding to Swap. A more robust implementation would
// poll actual lag and proceed only once it drops below a threshold.
const settleWindow = 2 * time.Second

// ShadowFlow implements the orchestrator.Flow interface for the
// shadow-table strategy (Architecture Doc Section 4.0, row 5).
//
// KNOWN LIMITATION: Execute does not yet call a dependent-object migration
// step (dependents.go, still unimplemented). `CREATE TABLE ... LIKE ...
// INCLUDING ALL` already copies indexes, defaults, NOT NULL, and CHECK
// constraints, but foreign keys referencing this table, triggers, RLS
// policies, and sequence ownership are NOT yet migrated (Architecture Doc
// Section 4.3). This is safe for the simple case (a table with only a
// primary key and no incoming dependents) but is a real gap otherwise.
type ShadowFlow struct {
	Pool           *pgxpool.Pool
	ReplicationDSN string
	Store          state.Store
	Preflighter    db.Preflighter
	Watcher        monitor.Watcher // optional; FR-05/FR-06 awareness
	SwapExecutor   *SwapExecutor
	BatchSize      int
	RollbackWindow time.Duration
}

var _ orchestrator.Flow = (*ShadowFlow)(nil)

// New creates a ShadowFlow with the given dependencies and default settings.
func New(pool *pgxpool.Pool, replicationDSN string, store state.Store, preflighter db.Preflighter) *ShadowFlow {
	return &ShadowFlow{
		Pool:           pool,
		ReplicationDSN: replicationDSN,
		Store:          store,
		Preflighter:    preflighter,
		SwapExecutor:   NewSwapExecutor(pool),
		RollbackWindow: defaultRollbackWindow,
	}
}

// resourceNames groups every identifier ShadowFlow generates for a job.
type resourceNames struct {
	shadowTable string
	tempTable   string
	slotName    string
	pubName     string
}

// ResourceNames returns the deterministic shadow table, temp table,
// replication slot, and publication names ShadowFlow generates for a given
// job. Exported so other packages — specifically internal/reaper, which
// needs to clean up a job's temp table/slot/publication once its
// ROLLBACK_WINDOW has expired — can recompute the exact same names without
// requiring them to be persisted on the Job record (only slotName and
// shadowTable are persisted, via state.Store.UpdateResources, because that
// mirrors internal/reaper's older, already-established orphan-cleanup
// contract; tempTable and pubName were never persisted, so recomputing
// them deterministically here is the only way another package can find
// them).
func ResourceNames(jobID, tableName string) (shadowTable, tempTable, slotName, pubName string) {
	n := resourceNamesFor(jobID, tableName)
	return n.shadowTable, n.tempTable, n.slotName, n.pubName
}

// resourceNamesFor deterministically derives every resource name from the
// job ID and table name, so they can be recomputed identically by Execute,
// Rollback, or a future resume path — without needing to persist all four
// names (only slotName/shadowTable are persisted, via Store.UpdateResources,
// because internal/reaper's already-established contract reads those two
// fields directly from the Job row).
//
// PostgreSQL identifiers are limited to 63 bytes; both the job ID and table
// name are truncated defensively to stay well under that limit.
func resourceNamesFor(jobID, tableName string) resourceNames {
	safeID := strings.ReplaceAll(jobID, "-", "_")
	if len(safeID) > 16 {
		safeID = safeID[:16]
	}
	safeTable := tableName
	if len(safeTable) > 20 {
		safeTable = safeTable[:20]
	}
	return resourceNames{
		shadowTable: fmt.Sprintf("__pgam_shadow_%s_%s", safeTable, safeID),
		tempTable:   fmt.Sprintf("__pgam_temp_%s_%s", safeTable, safeID),
		slotName:    fmt.Sprintf("pgam_slot_%s_%s", safeTable, safeID),
		pubName:     fmt.Sprintf("pgam_pub_%s_%s", safeTable, safeID),
	}
}

// Execute runs the shadow-table flow up through a successful swap, per
// Architecture Doc Section 4.1:
//
//  0. Preflight Check
//  1. Preparation: create the shadow table, publication, and replication slot
//  2. Initial Sync + Delta Sync, run CONCURRENTLY (Delta Sync starts
//     immediately after the slot exists; Initial Sync's batch copy safely
//     overlaps with it because ApplyEngine.Apply is idempotent)
//  3. Validation (row-count comparison — full chunked checksum per TR-10
//     is a TODO, see the comment on validate below)
//  4. Swap
//
// After a successful swap, Execute stops Delta Sync (no further legitimate
// writes should target the renamed-away original table) and transitions
// the job to ROLLBACK_WINDOW with a deadline, per FR-08a — it does NOT
// proceed to Cleanup/Completed itself. Cleanup only happens once the
// rollback window has passed without a Rollback call; driving that
// transition is a natural extension of internal/reaper's sweep (TODO).
func (f *ShadowFlow) Execute(ctx context.Context, job *state.Job) error {
	names := resourceNamesFor(job.ID, job.TableName)

	if err := f.setPhase(ctx, job, state.PhasePreflight); err != nil {
		return err
	}
	preflight, err := f.Preflighter.CheckShadowTablePreconditions(ctx, job.SchemaName, job.TableName)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("preflight check failed: %w", err))
	}
	if err := preflight.Validate(); err != nil {
		return f.fail(ctx, job, err)
	}

	// Collected before any DDL runs, per Architecture Doc Section 4.1 step 0
	// / Section 4.3 — nothing has been created yet, so a failure here is a
	// plain fail(), not failAndCleanup().
	deps, err := Inventory(ctx, f.Pool, job.SchemaName, job.TableName)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to inventory dependent objects: %w", err))
	}

	castColumn, castType := "", ""
	if job.Operation == "ALTER_COLUMN_TYPE" {
		castColumn, castType = job.ColumnName, job.ColumnType
	}

	if err := f.setPhase(ctx, job, state.PhasePreparation); err != nil {
		return err
	}
	startLSN, err := f.prepare(ctx, job, names, castColumn, castType, deps)
	if err != nil {
		return f.failAndCleanup(ctx, job, names, deps, fmt.Errorf("preparation failed: %w", err))
	}
	if err := f.Store.UpdateResources(ctx, job.ID, names.slotName, names.shadowTable); err != nil {
		return f.failAndCleanup(ctx, job, names, deps, fmt.Errorf("failed to persist resource names: %w", err))
	}
	// Keep the in-memory job in sync with what was just persisted — the
	// Store call above only updates the DATABASE row. Nothing in this
	// package currently reads job.ReplicationSlotName/job.ShadowTableName
	// back off the in-memory job afterward (everything downstream uses
	// `names` directly instead), so this had no observable effect today —
	// found via an audit prompted by the identical, but user-visible, bug
	// in internal/ddlflow's Job.RowsProcessed (see that package's
	// backfillLoop). Fixed here anyway to match this codebase's
	// established convention (every other Store.Update* call is
	// immediately followed by updating the corresponding in-memory
	// field) and to prevent a future feature that DOES read these fields
	// from silently getting stale/empty values.
	job.ReplicationSlotName = names.slotName
	job.ShadowTableName = names.shadowTable

	pkCols, err := PrimaryKeyColumns(ctx, f.Pool, job.SchemaName, job.TableName)
	if err != nil {
		return f.failAndCleanup(ctx, job, names, deps, fmt.Errorf("failed to determine primary key columns: %w", err))
	}

	if err := f.setPhase(ctx, job, state.PhaseSyncing); err != nil {
		return err
	}
	syncCtx, stopDeltaSync := context.WithCancel(ctx)
	defer stopDeltaSync() // safety net: always stop the goroutine, even on an early return
	syncErrCh := f.startDeltaSync(syncCtx, job.SchemaName, names, pkCols, castColumn, castType, startLSN)

	if err := f.runInitialSync(ctx, job, names, pkCols, castColumn, castType); err != nil {
		stopDeltaSync()
		<-syncErrCh // wait for the goroutine to actually exit before returning
		return f.failAndCleanup(ctx, job, names, deps, fmt.Errorf("initial sync failed: %w", err))
	}

	if err := f.setPhase(ctx, job, state.PhaseDeltaSync); err != nil {
		stopDeltaSync()
		<-syncErrCh
		return err
	}

	select {
	case <-ctx.Done():
		stopDeltaSync()
		<-syncErrCh
		return ctx.Err()
	case <-time.After(settleWindow):
	}

	if err := f.setPhase(ctx, job, state.PhaseValidating); err != nil {
		stopDeltaSync()
		<-syncErrCh
		return err
	}
	if err := f.validate(ctx, job, names, pkCols, castColumn, castType); err != nil {
		stopDeltaSync()
		<-syncErrCh
		return f.failAndCleanup(ctx, job, names, deps, fmt.Errorf("validation failed: %w", err))
	}

	if err := f.setPhase(ctx, job, state.PhaseSwapping); err != nil {
		stopDeltaSync()
		<-syncErrCh
		return err
	}
	swapErr := f.SwapExecutor.Swap(ctx, job.SchemaName, job.TableName, names.shadowTable, names.tempTable, DefaultSwapConfig())

	// Whether the swap succeeded or failed, Delta Sync's job is done: on
	// success, the renamed-away original table (tempTable) should receive
	// no further legitimate application writes; on failure, nothing changed
	// and we're about to fail the job anyway.
	stopDeltaSync()
	<-syncErrCh

	if swapErr != nil {
		return f.failAndCleanup(ctx, job, names, deps, fmt.Errorf("swap failed: %w", swapErr))
	}

	// The swap has already succeeded at this point — job.TableName now
	// holds the post-swap data. We deliberately do NOT route any failure
	// from here through failAndCleanup: that would drop the replication
	// slot/publication/shadow-table-name that Rollback() may still need,
	// and the original table has already been cut over, so "cleaning up"
	// by dropping things would be actively wrong, not just unnecessary.
	var dependentErr error
	if err := EnableUserTriggers(ctx, f.Pool, job.SchemaName, job.TableName, deps); err != nil {
		dependentErr = fmt.Errorf("failed to re-enable triggers after swap: %w", err)
	}
	if err := ReattachAfterSwap(ctx, f.Pool, deps); err != nil {
		dependentErr = errors.Join(dependentErr, fmt.Errorf("failed to reattach dependent objects after swap: %w", err))
	}

	if err := f.setPhase(ctx, job, state.PhaseRollbackWindow); err != nil {
		return fmt.Errorf("swap succeeded but failed to update phase to ROLLBACK_WINDOW: %w", err)
	}
	deadline := time.Now().Add(f.rollbackWindow())
	job.RollbackDeadline = &deadline
	if err := f.Store.UpdateRollbackDeadline(ctx, job.ID, deadline); err != nil {
		// The job is correctly in ROLLBACK_WINDOW phase; only the persisted
		// deadline failed to save. This is a real but narrow issue for
		// crash-recovery (a restart wouldn't know the exact deadline), but
		// the job itself is not FAILED — the swap succeeded, and marking it
		// FAILED here would be misleading and would point Rollback() at the
		// wrong cleanup path.
		return fmt.Errorf("swap succeeded and the job is in ROLLBACK_WINDOW, but failed to persist the rollback deadline: %w", err)
	}

	if dependentErr != nil {
		// The core data migration succeeded and the job is correctly in
		// ROLLBACK_WINDOW — but a dependent object (trigger/FK/view)
		// wasn't fully reattached and needs manual attention. Returned as
		// an error so the caller is alerted, without undoing the
		// successful migration itself.
		return fmt.Errorf("swap succeeded, but dependent-object reattachment had issues: %w", dependentErr)
	}
	return nil
}

// prepare creates the shadow table (with the target column's type already
// applied — cheap since the table is still empty), applies the Category A
// dependent objects (triggers, RLS, grants, sequence ownership — see
// dependents.go), creates the publication, and creates the replication
// slot, returning the LSN Delta Sync must start from.
func (f *ShadowFlow) prepare(ctx context.Context, job *state.Job, names resourceNames, castColumn, castType string, deps *DependentObjects) (pglogrepl.LSN, error) {
	sourceQualified := quoteIdent(job.SchemaName) + "." + quoteIdent(job.TableName)
	shadowQualified := quoteIdent(job.SchemaName) + "." + quoteIdent(names.shadowTable)

	createSQL := fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING ALL)", shadowQualified, sourceQualified)
	if _, err := f.Pool.Exec(ctx, createSQL); err != nil {
		return 0, fmt.Errorf("failed to create shadow table: %w", err)
	}

	if castColumn != "" {
		// Defense in depth — see strategy.ValidateColumnType's own doc
		// comment for the real injection vector this closes.
		// internal/orchestrator.StartMigration already validates the
		// requested type before a job even exists; this second check
		// protects any direct caller of this flow too, matching
		// internal/ddlflow's identical checks for its own DDL-building
		// call sites.
		if err := strategy.ValidateColumnType(castType); err != nil {
			return 0, fmt.Errorf("invalid target column type: %w", err)
		}

		// USING is required whenever there is no implicit/assignment cast
		// from the old type to the new one (e.g. text -> integer) — this is
		// true regardless of whether the table currently has any rows.
		// Without USING, PostgreSQL only allows casts it considers
		// "automatic", which text->integer is not (SQLSTATE 42804).
		alterSQL := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s USING %s::%s",
			shadowQualified, quoteIdent(castColumn), castType, quoteIdent(castColumn), castType)
		if _, err := f.Pool.Exec(ctx, alterSQL); err != nil {
			return 0, fmt.Errorf("failed to apply the target column type to the (still-empty) shadow table: %w", err)
		}
	}

	if err := ApplyToShadowTable(ctx, f.Pool, job.SchemaName, names.shadowTable, deps); err != nil {
		return 0, fmt.Errorf("failed to apply dependent objects to the shadow table: %w", err)
	}

	if err := CreatePublication(ctx, f.Pool, job.SchemaName, job.TableName, names.pubName); err != nil {
		return 0, err
	}

	startLSN, err := CreateReplicationSlotAndGetStartLSN(ctx, f.ReplicationDSN, names.slotName)
	if err != nil {
		return 0, err
	}
	return startLSN, nil
}

// startDeltaSync starts the SyncEngine in a background goroutine and
// returns a channel that receives its terminal error (nil on a clean
// context-cancellation shutdown).
func (f *ShadowFlow) startDeltaSync(ctx context.Context, schema string, names resourceNames, pkCols []string, castColumn, castType string, startLSN pglogrepl.LSN) <-chan error {
	engine := &SyncEngine{
		Decoder: &Decoder{ReplicationDSN: f.ReplicationDSN, SlotName: names.slotName, PublicationName: names.pubName},
		Apply: &ApplyEngine{
			Pool: f.Pool, Schema: schema, ShadowTable: names.shadowTable,
			PrimaryKeyColumns: pkCols, CastColumn: castColumn, CastType: castType,
		},
	}
	errCh := make(chan error, 1)
	go func() {
		err := engine.Run(ctx, startLSN)
		if errors.Is(err, context.Canceled) {
			err = nil
		}
		errCh <- err
	}()
	return errCh
}

// runInitialSync copies existing rows into the shadow table (Architecture
// Doc Section 4.1 step 2), respecting throttle signals via f.Watcher.
func (f *ShadowFlow) runInitialSync(ctx context.Context, job *state.Job, names resourceNames, pkCols []string, castColumn, castType string) error {
	cfg := InitialSyncConfig{
		Pool: f.Pool, Watcher: f.Watcher, BatchSize: f.BatchSize,
		SourceSchema: job.SchemaName, SourceTable: job.TableName, ShadowTable: names.shadowTable,
		CastColumn: castColumn, CastType: castType, PKColumns: pkCols,
		// See InitialSyncConfig.OnBatchComplete's own doc comment for why
		// this exists at all — mirrors internal/ddlflow's backfillLoop's
		// identical IncrementRowsProcessed call: best-effort (a failure
		// to persist the counter shouldn't abort an otherwise-successful
		// batch, it's a display metric, not correctness-critical state),
		// and keeps the in-memory job in sync with what was just
		// persisted (the Store call alone only updates the DATABASE row).
		OnBatchComplete: func(rowsCopiedThisBatch int64) {
			if err := f.Store.IncrementRowsProcessed(ctx, job.ID, rowsCopiedThisBatch); err != nil {
				log.Printf("shadowflow: failed to persist rows-processed counter for job %s: %v", job.ID, err)
			}
			job.RowsProcessed += rowsCopiedThisBatch
		},
	}
	return cfg.Run(ctx)
}

// validate performs the Section 4.1 step 4 "Validation" check. Currently
// this compares total row counts between the source and shadow tables.
//
// TODO (TR-10): implement full chunked checksum validation (comparing
// per-chunk checksums rather than just row counts) so that row-level data
// mismatches — not just missing/extra rows — are caught before swapping.
// Row-count parity is a meaningful but partial signal: it would not, for
// example, catch a row whose non-key column was corrupted during Apply.
// validate performs the Section 4.1 step 4 "Validation" check:
//  1. A row-count comparison (always).
//  2. A chunked checksum comparison per TR-10, covering single-column and
//     composite primary keys of any orderable type (integer, text, uuid,
//     ...) — see validateChunkedChecksum's doc comment for how. Only a
//     table with NO primary key at all falls back to row-count-only.
func (f *ShadowFlow) validate(ctx context.Context, job *state.Job, names resourceNames, pkCols []string, castColumn, castType string) error {
	sourceQualified := quoteIdent(job.SchemaName) + "." + quoteIdent(job.TableName)
	shadowQualified := quoteIdent(job.SchemaName) + "." + quoteIdent(names.shadowTable)

	var sourceCount, shadowCount int64
	if err := f.Pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", sourceQualified)).Scan(&sourceCount); err != nil {
		return fmt.Errorf("failed to count source rows: %w", err)
	}
	if err := f.Pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s", shadowQualified)).Scan(&shadowCount); err != nil {
		return fmt.Errorf("failed to count shadow rows: %w", err)
	}
	if sourceCount != shadowCount {
		return fmt.Errorf("row count mismatch: source has %d row(s), shadow has %d row(s)", sourceCount, shadowCount)
	}

	if err := validateChunkedChecksum(ctx, f.Pool, job.SchemaName, job.TableName, names.shadowTable, pkCols, castColumn, castType, f.checksumBatchSize()); err != nil {
		return err
	}
	return nil
}

func (f *ShadowFlow) checksumBatchSize() int {
	if f.BatchSize <= 0 {
		return defaultChecksumBatchSize
	}
	return f.BatchSize
}

// Rollback implements FR-07/FR-08/FR-08a. It handles two cases:
//
// Dependent objects (foreign keys, views) are re-inventoried and reattached
// symmetrically with Execute's post-swap step — see the ROLLBACK_WINDOW
// case below. Triggers/RLS/grants need no special handling on rollback:
// the restored table is the untouched original object, which never lost
// its own copies of those in the first place.
//
//   - job.Phase == ROLLBACK_WINDOW: the swap already succeeded. If the
//     rollback deadline has not passed, a reverse swap restores the
//     original table, and the post-swap (new) data is preserved under the
//     shadow-table name rather than being dropped outright. If the
//     deadline HAS passed, this returns an error per FR-08a — the caller
//     must treat this as a new migration, not a rollback.
//   - job.Phase is any pre-swap phase (PREFLIGHT..SWAPPING but the swap
//     itself never committed): the shadow table, slot, and publication are
//     dropped; the original table is left untouched (Architecture Doc
//     Section 4.2, "if the swap never happened").
func (f *ShadowFlow) Rollback(ctx context.Context, job *state.Job) error {
	names := resourceNamesFor(job.ID, job.TableName)

	switch job.Phase {
	case state.PhaseRollbackWindow:
		if job.RollbackDeadline != nil && time.Now().After(*job.RollbackDeadline) {
			return fmt.Errorf("shadowflow: the rollback window expired at %s; this must now be handled as a new migration, not a rollback (FR-08a)",
				job.RollbackDeadline.Format(time.RFC3339))
		}

		// Capture the dependent objects (foreign keys, views) CURRENTLY
		// referencing the live, about-to-be-reverted table — this is the
		// same Inventory() Execute uses, called fresh here rather than
		// relying on Execute's original snapshot, since Rollback may run
		// in an entirely separate process invocation. Because foreign key
		// and view definitions reference tables BY NAME (not OID), this
		// capture remains valid and reusable after the reverse swap
		// changes which physical object owns that name — see
		// ReattachAfterSwap's doc comment for why re-running the same
		// name-based DDL after any swap correctly binds to the new owner.
		deps, err := Inventory(ctx, f.Pool, job.SchemaName, job.TableName)
		if err != nil {
			return fmt.Errorf("shadowflow: failed to inventory dependent objects before rollback: %w", err)
		}

		// Reverse the swap. Recall Swap(schema, oldTable, newTable, tempName)
		// renames oldTable->tempName, then newTable->oldTable. The forward
		// swap in Execute called Swap(schema, sourceTable, shadowTable, tempTable),
		// leaving: sourceTable name = NEW data, tempTable name = ORIGINAL data.
		// To reverse, we swap sourceTable (currently NEW data) and tempTable
		// (currently ORIGINAL data), reusing the now-free shadowTable name as
		// scratch space for whichever table gets renamed out of the way:
		//   Swap(schema, sourceTable, tempTable, shadowTable)
		//   -> sourceTable(NEW data) renamed to shadowTable (preserved, not dropped)
		//   -> tempTable(ORIGINAL data) renamed to sourceTable (rollback achieved)
		if err := f.SwapExecutor.Swap(ctx, job.SchemaName, job.TableName, names.tempTable, names.shadowTable, DefaultSwapConfig()); err != nil {
			return fmt.Errorf("shadowflow: reverse swap failed: %w", err)
		}

		// Symmetric with Execute's post-swap step: redirect the foreign
		// keys/views captured above at whatever now owns the table's name
		// — which, after the reverse swap, is the restored original table.
		// (Triggers/RLS/grants need no action here: the restored table is
		// the untouched original object, which never lost its own copies
		// of those — only the shadow table's FRESH copies were involved in
		// the forward swap's EnableUserTriggers call.)
		if err := ReattachAfterSwap(ctx, f.Pool, deps); err != nil {
			return fmt.Errorf("shadowflow: reverse swap succeeded but failed to reattach dependent objects: %w", err)
		}

		// The migration is aborted either way now — the slot and
		// publication are no longer needed. This is best-effort: the
		// reverse swap (the part that actually restores correct data) has
		// already succeeded, so a cleanup failure here is reported but does
		// not undo the successful rollback.
		if err := dropSlotIfExistsSoft(ctx, f.Pool, names.slotName); err != nil {
			return fmt.Errorf("shadowflow: reverse swap succeeded but slot cleanup failed: %w", err)
		}
		if err := DropPublicationIfExists(ctx, f.Pool, names.pubName); err != nil {
			return fmt.Errorf("shadowflow: reverse swap succeeded but publication cleanup failed: %w", err)
		}

	case state.PhasePreflight, state.PhasePreparation, state.PhaseSyncing, state.PhaseDeltaSync, state.PhaseValidating, state.PhaseSwapping:
		// The swap never committed (or Execute never got that far); the
		// original table is untouched. Best-effort cleanup of whatever
		// partial state exists.
		if err := dropSlotIfExistsSoft(ctx, f.Pool, names.slotName); err != nil {
			return fmt.Errorf("shadowflow: failed to clean up replication slot during rollback: %w", err)
		}
		if err := DropPublicationIfExists(ctx, f.Pool, names.pubName); err != nil {
			return fmt.Errorf("shadowflow: failed to clean up publication during rollback: %w", err)
		}
		dropSQL := fmt.Sprintf("DROP TABLE IF EXISTS %s.%s", quoteIdent(job.SchemaName), quoteIdent(names.shadowTable))
		if _, err := f.Pool.Exec(ctx, dropSQL); err != nil {
			return fmt.Errorf("shadowflow: failed to drop shadow table during rollback: %w", err)
		}

	default:
		return fmt.Errorf("shadowflow: rollback is not supported from phase %s", job.Phase)
	}

	return f.setPhase(ctx, job, state.PhaseAborted)
}

// dropSlotIfExistsSoft drops a replication slot only if it currently
// exists, silently succeeding otherwise — used by Rollback for early-phase
// cleanup, where the slot may or may not have been created yet depending
// on exactly which step Execute reached before it was interrupted.
func dropSlotIfExistsSoft(ctx context.Context, pool *pgxpool.Pool, slotName string) error {
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM pg_replication_slots WHERE slot_name = $1)`, slotName).Scan(&exists); err != nil {
		return fmt.Errorf("failed to check slot existence: %w", err)
	}
	if !exists {
		return nil
	}
	if _, err := pool.Exec(ctx, `SELECT pg_drop_replication_slot($1)`, slotName); err != nil {
		return fmt.Errorf("failed to drop slot: %w", err)
	}
	return nil
}

func (f *ShadowFlow) rollbackWindow() time.Duration {
	if f.RollbackWindow <= 0 {
		return defaultRollbackWindow
	}
	return f.RollbackWindow
}

func (f *ShadowFlow) setPhase(ctx context.Context, job *state.Job, phase state.Phase) error {
	if err := f.Store.UpdatePhase(ctx, job.ID, phase); err != nil {
		return fmt.Errorf("failed to update phase (%s): %w", phase, err)
	}
	job.Phase = phase
	return nil
}

// fail marks the job as FAILED, writes the original error to LastError, and
// returns the original error unchanged to the caller (mirrors
// internal/ddlflow.DDLFlow.fail for consistency).
func (f *ShadowFlow) fail(ctx context.Context, job *state.Job, cause error) error {
	if err := f.Store.UpdatePhaseWithError(ctx, job.ID, state.PhaseFailed, cause.Error()); err != nil {
		return fmt.Errorf("%w (also failed to update checkpoint: %v)", cause, err)
	}
	job.Phase = state.PhaseFailed
	job.LastError = cause.Error()
	return cause
}

// failAndCleanup is used for every failure that can occur AFTER prepare()
// has started (i.e., once a shadow table, publication, or replication slot
// may already exist). It is important that this runs BEFORE marking the
// job FAILED: internal/reaper's ListStale query deliberately excludes
// terminal phases (COMPLETED/FAILED/ABORTED) — see internal/state's Store
// interface doc comment — so a job left in FAILED with orphaned resources
// would never be picked up by the reaper's safety net. Cleanup here is
// therefore Execute's own responsibility, not something we can defer to
// the reaper. All three drops are best-effort/idempotent (safe no-ops if
// the resource was never created, e.g. because prepare() failed on its
// first step); a cleanup failure is appended to, but does not replace, the
// original cause.
func (f *ShadowFlow) failAndCleanup(ctx context.Context, job *state.Job, names resourceNames, deps *DependentObjects, cause error) error {
	if err := dropSlotIfExistsSoft(ctx, f.Pool, names.slotName); err != nil {
		cause = fmt.Errorf("%w (additionally, slot cleanup failed: %v)", cause, err)
	}
	if err := DropPublicationIfExists(ctx, f.Pool, names.pubName); err != nil {
		cause = fmt.Errorf("%w (additionally, publication cleanup failed: %v)", cause, err)
	}
	// Must run BEFORE the DROP TABLE below, not after — see
	// RevertSequenceOwnership's own doc comment for the real failure mode
	// this prevents: without it, ApplyToShadowTable's earlier (successful)
	// sequence-ownership transfer to the shadow table permanently blocks
	// this exact DROP TABLE with a real PostgreSQL dependency error,
	// leaving BOTH the shadow table AND the mis-owned sequence orphaned —
	// found via a shadow table internal/reaper could never clean up,
	// requiring manual `ALTER SEQUENCE ... OWNED BY` intervention.
	if deps != nil {
		if err := RevertSequenceOwnership(ctx, f.Pool, job.SchemaName, job.TableName, deps); err != nil {
			cause = fmt.Errorf("%w (additionally, sequence ownership revert failed: %v)", cause, err)
		}
	}
	dropSQL := fmt.Sprintf("DROP TABLE IF EXISTS %s.%s", quoteIdent(job.SchemaName), quoteIdent(names.shadowTable))
	if _, err := f.Pool.Exec(ctx, dropSQL); err != nil {
		cause = fmt.Errorf("%w (additionally, shadow table cleanup failed: %v)", cause, err)
	}
	return f.fail(ctx, job, cause)
}
