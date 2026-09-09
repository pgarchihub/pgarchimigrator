package upgrade

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// syncPollInterval is how often Flow.Run checks AllTablesReady while
// waiting for PostgreSQL's own subscription machinery to catch up —
// see Flow.Run's own polling loop. Not configurable (yet): a fixed,
// modest interval that adds negligible load to the target instance
// regardless of how long the underlying copy actually takes.
const syncPollInterval = 5 * time.Second

// Flow runs one upgrade Job through every phase from PhaseIntrospecting
// to PhaseReady — the whole-database analog of engines/postgresql/ddlflow.DDLFlow/
// engines/postgresql/shadowflow.ShadowFlow's own Execute method, deliberately its
// own type rather than implementing orchestrator.Flow's interface: that
// interface's Execute(ctx, *state.Job) signature is built around
// state.Job specifically, which this package's Job is deliberately NOT
// (see the package doc comment) — internal/orchestrator has no
// awareness this package exists at all, matching how internal/ecosystem
// integrates with the rest of this codebase without any of ddlflow/
// shadowflow/orchestrator needing to change for it.
type Flow struct {
	Store              Store
	ConnectionProvider ConnectionProvider
}

// Run executes job's entire lifecycle synchronously, start to finish —
// like orchestrator.StartMigration's own synchronous default (see that
// function's doc comment), a caller wanting this backgrounded runs Run
// in its own goroutine with its own long-lived context, exactly as
// orchestrator.StartMigrationAsync does for a single migration. This
// package doesn't provide its own "Async" variant yet — see
// docs/ecosystem/ARCHITECTURE.md's own upgrade section for what's still
// ahead; the CLI/API wiring that will actually call this is one of
// those remaining pieces.
//
// On any failure, job's Phase is left at PhaseFailed (via
// Store.UpdateJobPhaseWithError) before the error is returned — a
// caller inspecting the Job afterward (whether Run failed synchronously
// or was backgrounded) always finds a coherent terminal state, never a
// job stuck silently mid-phase because Run's own goroutine died without
// recording why.
func (f *Flow) Run(ctx context.Context, job *Job) error {
	sourceDSN, err := f.ConnectionProvider.SourceDSN(ctx, job)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to resolve source connection: %w", err))
	}
	targetDSN, err := f.ConnectionProvider.TargetDSN(ctx, job)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to resolve target connection: %w", err))
	}
	// See Job.SourceReplicationRef's own doc comment for why this is
	// resolved separately from sourceDSN above — it's what the TARGET's
	// own PostgreSQL server uses in CREATE SUBSCRIPTION, not what THIS
	// process connects to source with.
	replicationDSN, err := f.ConnectionProvider.ReplicationDSN(ctx, job)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to resolve replication connection: %w", err))
	}

	sourcePool, err := pgxpool.New(ctx, sourceDSN)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to connect to source: %w", err))
	}
	defer sourcePool.Close()

	targetPool, err := pgxpool.New(ctx, targetDSN)
	if err != nil {
		return f.fail(ctx, job, fmt.Errorf("failed to connect to target: %w", err))
	}
	defer targetPool.Close()

	refs, err := f.introspect(ctx, job, sourcePool)
	if err != nil {
		return f.fail(ctx, job, err)
	}

	if err := f.createSchema(ctx, job, sourcePool, targetPool, refs); err != nil {
		return f.fail(ctx, job, err)
	}

	if err := f.sync(ctx, job, sourcePool, targetPool, refs, replicationDSN); err != nil {
		return f.fail(ctx, job, err)
	}

	if err := f.validate(ctx, job, sourcePool, targetPool, refs); err != nil {
		return f.fail(ctx, job, err)
	}

	if err := f.Store.UpdateJobPhase(ctx, job.ID, PhaseReady); err != nil {
		return fmt.Errorf("failed to record final job phase: %w", err)
	}
	job.Phase = PhaseReady
	return nil
}

func (f *Flow) introspect(ctx context.Context, job *Job, sourcePool *pgxpool.Pool) ([]TableRef, error) {
	refs, err := Introspect(ctx, sourcePool, job)
	if err != nil {
		return nil, fmt.Errorf("introspection failed: %w", err)
	}
	if len(refs) == 0 {
		return nil, fmt.Errorf("no tables found in scope on the source instance")
	}
	for _, ref := range refs {
		if err := f.Store.CreateTable(ctx, &Table{JobID: job.ID, SchemaName: ref.SchemaName, TableName: ref.TableName, Phase: PhaseIntrospecting}); err != nil {
			return nil, fmt.Errorf("failed to record table %s: %w", ref, err)
		}
	}
	if err := f.Store.UpdateJobProgress(ctx, job.ID, len(refs), 0, 0); err != nil {
		return nil, fmt.Errorf("failed to record table count: %w", err)
	}
	job.TablesTotal = len(refs)
	return refs, nil
}

func (f *Flow) createSchema(ctx context.Context, job *Job, sourcePool, targetPool *pgxpool.Pool, refs []TableRef) error {
	for _, ref := range refs {
		if err := CreateTableOnTarget(ctx, sourcePool, targetPool, ref); err != nil {
			_ = f.Store.UpdateTablePhase(ctx, job.ID, ref.SchemaName, ref.TableName, PhaseFailed, err.Error())
			// Stops at the first failure rather than continuing to the
			// next table — a partially-created target schema (some
			// tables present, others not) is a confusing state to leave
			// behind, and there's no reason to believe the NEXT table
			// would succeed if this one didn't (a bad DSN, a permissions
			// problem, and similar root causes affect every table
			// equally). Matches this project's own established
			// "apply-file stops at the first failure" precedent (see
			// docs/migration-as-code.md) for the identical reasoning.
			return fmt.Errorf("failed to create %s on target: %w", ref, err)
		}
		if err := f.Store.UpdateTablePhase(ctx, job.ID, ref.SchemaName, ref.TableName, PhaseSchemaCreated, ""); err != nil {
			return fmt.Errorf("failed to record table %s progress: %w", ref, err)
		}
	}
	if err := f.Store.UpdateJobPhase(ctx, job.ID, PhaseSchemaCreated); err != nil {
		return fmt.Errorf("failed to record job phase: %w", err)
	}
	job.Phase = PhaseSchemaCreated
	return nil
}

func (f *Flow) sync(ctx context.Context, job *Job, sourcePool, targetPool *pgxpool.Pool, refs []TableRef, replicationDSN string) error {
	if err := CreatePublication(ctx, sourcePool, job.ID, refs); err != nil {
		return fmt.Errorf("sync setup failed: %w", err)
	}
	if err := CreateSubscription(ctx, targetPool, job.ID, replicationDSN); err != nil {
		// A specific, actionable hint for a specific, predictable cause
		// — see this function's own explanation below, rather than
		// surfacing PostgreSQL's own connection-failure text unexplained
		// a second time. CREATE SUBSCRIPTION is executed by the TARGET's
		// own PostgreSQL server (see Job.SourceReplicationRef's own doc
		// comment), so a connection failure here specifically — as
		// opposed to anywhere else in this flow — usually means the
		// TARGET can't reach the source using the SAME address this
		// process itself used (job.SourceReplicationRef was never set,
		// so ReplicationDSN fell back to the ordinary SourceDSN). A real
		// bug report from manual testing hit exactly this, repeatedly,
		// with no explanation of why.
		if job.SourceReplicationRef == "" && isConnectionFailureErr(err) {
			return fmt.Errorf(
				"sync setup failed: failed to create subscription: %w — this usually means the TARGET instance's own PostgreSQL server can't reach the source using the same address this process uses (no replication address override is set on this job). Retry with a replication host/port override, or start a new database migration with the \"Advanced: different replication address\" field filled in",
				err,
			)
		}
		return fmt.Errorf("sync setup failed: %w", err)
	}
	if err := f.Store.UpdateJobPhase(ctx, job.ID, PhaseSyncing); err != nil {
		return fmt.Errorf("failed to record job phase: %w", err)
	}
	job.Phase = PhaseSyncing

	// Polls PostgreSQL's own subscription-progress catalog rather than
	// this package driving the copy itself — see AllTablesReady's own
	// doc comment. No timeout here beyond ctx's own: an initial full
	// copy of a genuinely large database can legitimately take hours,
	// and this package has no principled basis for guessing a "this has
	// taken too long" threshold on the caller's behalf — a caller
	// wanting a bound sets one on ctx (context.WithTimeout) before
	// calling Run.
	ticker := time.NewTicker(syncPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("sync did not complete before the context was canceled: %w", ctx.Err())
		case <-ticker.C:
			// Same cadence as the AllTablesReady check just below —
			// this is exactly the "batched, not once per row" interval
			// UpdateTableRowsSynced's own doc comment calls for, so no
			// separate throttling is needed here.
			f.updateRowCounts(ctx, job, targetPool, refs)

			ready, err := AllTablesReady(ctx, targetPool, job.ID, refs)
			if err != nil {
				return fmt.Errorf("failed to check sync progress: %w", err)
			}
			if ready {
				// One final count now that sync is confirmed complete —
				// the LAST tick's count (just above) can undercount if
				// a handful of rows landed in the gap between that read
				// and AllTablesReady's own "ready" observation.
				f.updateRowCounts(ctx, job, targetPool, refs)
				return nil
			}
		}
	}
}

// updateRowCounts reads each in-scope table's current row count
// directly from the TARGET and records it via
// Store.UpdateTableRowsSynced — a simple, honest proxy for "how much
// has arrived so far" while PostgreSQL's own logical replication is
// doing the actual copying. Best-effort: a failure to read or record
// one table's count is logged-and-skipped rather than failing the
// whole sync over what is, after all, a progress DISPLAY concern, not
// the sync's own correctness (ValidateTable's own row-count/checksum
// comparison, not this, is what actually verifies correctness).
func (f *Flow) updateRowCounts(ctx context.Context, job *Job, targetPool *pgxpool.Pool, refs []TableRef) {
	for _, ref := range refs {
		var count int64
		row := targetPool.QueryRow(ctx, fmt.Sprintf("SELECT count(*) FROM %s.%s", quoteIdent(ref.SchemaName), quoteIdent(ref.TableName)))
		if err := row.Scan(&count); err != nil {
			continue
		}
		_ = f.Store.UpdateTableRowsSynced(ctx, job.ID, ref.SchemaName, ref.TableName, count)
	}
}

func (f *Flow) validate(ctx context.Context, job *Job, sourcePool, targetPool *pgxpool.Pool, refs []TableRef) error {
	if err := f.Store.UpdateJobPhase(ctx, job.ID, PhaseValidating); err != nil {
		return fmt.Errorf("failed to record job phase: %w", err)
	}
	job.Phase = PhaseValidating

	verified := 0
	for _, ref := range refs {
		result, err := ValidateTable(ctx, sourcePool, targetPool, ref)
		if err != nil {
			return fmt.Errorf("validation failed for %s: %w", ref, err)
		}
		if !result.Matched {
			_ = f.Store.UpdateTablePhase(ctx, job.ID, ref.SchemaName, ref.TableName, PhaseFailed,
				fmt.Sprintf("row count or checksum mismatch: source=(%d rows, checksum %d) target=(%d rows, checksum %d)",
					result.SourceRowCount, result.SourceChecksum, result.TargetRowCount, result.TargetChecksum))
			// Same "stop at the first failure" reasoning as
			// createSchema — a mismatch is a genuine data-integrity
			// problem worth stopping for immediately, not a soft
			// warning to note and continue past.
			return fmt.Errorf("%s failed validation (source and target data do not match)", ref)
		}
		if err := f.Store.UpdateTablePhase(ctx, job.ID, ref.SchemaName, ref.TableName, PhaseReady, ""); err != nil {
			return fmt.Errorf("failed to record table %s progress: %w", ref, err)
		}
		verified++
		if err := f.Store.UpdateJobProgress(ctx, job.ID, job.TablesTotal, job.TablesTotal, verified); err != nil {
			return fmt.Errorf("failed to record validation progress: %w", err)
		}
	}
	job.TablesSynced = job.TablesTotal
	job.TablesVerified = verified
	return nil
}

// fail records job as PhaseFailed with err's own message before
// returning err unchanged — the one place every failure path in Run
// funnels through, so "record the failure" can never be forgotten on a
// new error path added later.
func (f *Flow) fail(ctx context.Context, job *Job, err error) error {
	_ = f.Store.UpdateJobPhaseWithError(ctx, job.ID, PhaseFailed, err.Error())
	job.Phase = PhaseFailed
	job.LastError = err.Error()
	return err
}

// isConnectionFailureErr reports whether err is PostgreSQL's own
// connection_failure condition (SQLSTATE 08006) — checked via pgconn's
// own structured PgError (errors.As), the same pattern
// isDuplicateTableErr (introspect.go) already uses, not a string match
// against the error text, so this stays correct regardless of
// PostgreSQL's own message wording across versions or locales. 08006 is
// a genuinely broad SQLSTATE (many underlying causes can raise it), but
// checked ONLY at CreateSubscription's own call site in Flow.sync,
// where "the target's own server couldn't reach the source" is by far
// the most likely cause in practice.
func isConnectionFailureErr(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "08006"
	}
	return false
}
