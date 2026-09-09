// Package orchestrator implements the "Orchestration Engine" described in
// Architecture Doc Section 3.1. It runs engines/postgresql/ddlflow or
// engines/postgresql/shadowflow based on the decision from internal/strategy, and
// checkpoints progress via internal/state.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/auditlog"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
)

// Flow is the common interface for a concrete migration strategy executor
// (Direct DDL, Expand&Backfill, Shadow Table). engines/postgresql/ddlflow and
// engines/postgresql/shadowflow implement this interface — the orchestrator can call
// either the same way without knowing which strategy it is (Step Executor,
// Section 3.1).
type Flow interface {
	// Execute runs the flow from the beginning (or resumes from the last
	// checkpoint). Progress is persisted to state.Store by updating
	// job.Phase at every step (FR-04).
	Execute(ctx context.Context, job *state.Job) error

	// Rollback reverts the operation per FR-07/FR-08/FR-08a.
	// The rollback-window check is applied here in the shadow-table flow.
	Rollback(ctx context.Context, job *state.Job) error
}

// TableStatsFetcher supplies the raw table statistics strategy.Decide
// needs. Kept as an injectable function (rather than importing engines/postgresql/db
// directly) so this package stays decoupled from any specific database
// driver — the same pattern already used by FlowFor below. In production,
// this is backed by db.FetchTableStats; tests can supply a fake.
type TableStatsFetcher func(ctx context.Context, schema, table string) (strategy.TableStats, error)

// FlowBuilder builds the concrete Flow for a given strategy decision. In
// production this constructs an engines/postgresql/ddlflow.DDLFlow or
// engines/postgresql/shadowflow.ShadowFlow (both already implement Flow); tests can
// supply a fake that never touches a real database.
type FlowBuilder func(strat strategy.Strategy) (Flow, error)

// Orchestrator is the entry point for all migration requests.
type Orchestrator struct {
	Store       state.Store
	FlowFor     FlowBuilder
	TableStats  TableStatsFetcher
	IDGenerator func() string // optional; defaults to generateJobID
	// AuditWriter is optional (nil disables audit logging entirely — no
	// error, no panic). See internal/auditlog and TR-07 "Who, when, what".
	AuditWriter auditlog.Writer
	// VersionCheck is optional (nil skips it entirely, matching
	// AuditWriter's identical nil-is-fine pattern) — a general,
	// strategy-independent TR-11 minimum-version gate, checked once at
	// the very start of every StartMigration call regardless of which
	// strategy ends up being chosen. Added specifically because the
	// shadow-table-specific preflight check (engines/postgresql/db's
	// PgxPreflighter) previously left DIRECT_DDL/EXPAND_BACKFILL
	// migrations completely unchecked against this same minimum. In
	// production this is backed by db.ValidateMinimumVersion; tests that
	// don't care about version enforcement simply leave it nil.
	VersionCheck func(ctx context.Context) error
}

// New creates an Orchestrator with the given dependencies.
func New(store state.Store, flowFor FlowBuilder, tableStats TableStatsFetcher) *Orchestrator {
	return &Orchestrator{Store: store, FlowFor: flowFor, TableStats: tableStats}
}

// MigrationRequest carries the user request coming from the CLI/API layer.
type MigrationRequest struct {
	SchemaName       string
	TableName        string
	Change           strategy.ColumnChange
	StrategyOverride strategy.Strategy // FR-02, empty means automatic selection
	Actor            string            // "who" for the audit log (TR-07); empty becomes "unknown"
	// Name/Description are purely human-facing, optional labels for the
	// job — see state.Job.Name's doc comment. Deliberately live on
	// MigrationRequest itself, not inside Change (strategy.ColumnChange):
	// they describe the MIGRATION, not the schema-change parameters, and
	// apply identically regardless of Operation.
	Name        string
	Description string
	// The three fields below carry Archi ecosystem correlation context
	// (AC-PF-003 Section 12.1) for a migration started BY the ecosystem
	// — see state.Job.CorrelationID's own doc comment for the full
	// reasoning (same fields, same meaning; this is just where they
	// enter the system before StartMigration copies them onto the new
	// Job). All three are empty for a migration started directly
	// through this product's own CLI/API/dashboard.
	CorrelationID string
	CausationID   string
	LifecycleID   string
}

// StartMigration implements FR-01/FR-02/FR-03:
//  1. Table statistics are fetched and strategy.Decide picks a strategy (or
//     the user's StrategyOverride is used, still subject to strategy.Decide's
//     hard constraints — see internal/strategy).
//  2. A Job checkpoint is created via state.Store.
//  3. The corresponding Flow (ddlflow or shadowflow) is built and Execute'd.
//
// StartMigration always returns the Job (even on failure) alongside any
// error, so the caller can inspect job.Phase/job.LastError — Flow.Execute
// implementations are responsible for marking the job FAILED and cleaning
// up their own partial resources before returning an error (see
// engines/postgresql/ddlflow.DDLFlow.fail and
// engines/postgresql/shadowflow.ShadowFlow.failAndCleanup); StartMigration itself
// does not attempt any additional cleanup.
// preparedJob is prepareJob's return value — everything StartMigration
// and StartMigrationAsync share, up to (and including) the point the
// job is durably persisted, leaving only the actual flow.Execute call
// (synchronous vs. backgrounded) to the two callers.
type preparedJob struct {
	job   *state.Job
	flow  Flow
	actor string
}

// prepareJob does everything StartMigration and StartMigrationAsync
// have in common: validate the request, decide a strategy, create the
// job checkpoint, and resolve which Flow will execute it. Deliberately
// factored out so BOTH callers share the exact same validation logic
// and job-creation shape — the only difference between "start a
// migration and wait for it" and "start a migration and return
// immediately" (see StartMigrationAsync's own doc comment for why that
// second mode exists) should ever be how flow.Execute gets called, never
// a second, independently-maintained copy of everything above it.
//
// Returns job == nil only when no job record was ever created at all
// (a validation failure, or the table-stats/strategy-decision step
// itself failing) — mirrors StartMigration's own pre-refactor "job ==
// nil means nothing happened" contract exactly, so neither caller's
// nil-handling needed to change.
func (o *Orchestrator) prepareJob(ctx context.Context, req MigrationRequest) (*preparedJob, error) {
	if o.VersionCheck != nil {
		if err := o.VersionCheck(ctx); err != nil {
			return nil, fmt.Errorf("preflight failed: %w", err)
		}
	}

	actor := req.Actor
	if actor == "" {
		actor = "unknown"
	}

	rawStats, err := o.TableStats(ctx, req.SchemaName, req.TableName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch table statistics: %w", err)
	}
	rawStats.SchemaName = req.SchemaName
	rawStats.TableName = req.TableName

	strat, err := strategy.Decide(rawStats, req.Change, req.StrategyOverride)
	if err != nil {
		return nil, fmt.Errorf("failed to decide migration strategy: %w", err)
	}

	// Reject a malformed/malicious request BEFORE a job record even
	// exists — see internal/strategy's ValidateColumnType/
	// ValidateSQLExpression doc comments for the real vulnerability this
	// closes: NewType/DefaultValue/CheckExpression all get inlined
	// directly into DDL text later (PostgreSQL doesn't support parameter
	// binding inside ALTER TABLE/ADD COLUMN/ADD CONSTRAINT), so an
	// unvalidated caller-supplied value was a genuine SQL injection
	// vector. Enforced again at the point engines/postgresql/ddlflow and
	// engines/postgresql/shadowflow actually build DDL from these values, so any
	// direct caller of those flows (not just ones that went through
	// StartMigration) gets the same protection — this early check is an
	// additional, better-UX layer (a clear 4xx before any job exists),
	// not the only one.
	if req.Change.NewType != "" {
		if err := strategy.ValidateColumnType(req.Change.NewType); err != nil {
			return nil, fmt.Errorf("invalid column type: %w", err)
		}
	}
	if req.Change.DefaultValue != "" {
		if err := strategy.ValidateSQLExpression(req.Change.DefaultValue, "default value"); err != nil {
			return nil, err
		}
	}
	if req.Change.CheckExpression != "" {
		if err := strategy.ValidateSQLExpression(req.Change.CheckExpression, "check expression"); err != nil {
			return nil, err
		}
	}
	if err := strategy.ValidateOnDeleteAction(req.Change.OnDelete); err != nil {
		return nil, err
	}
	if req.Change.GeneratedExpression != "" {
		if err := strategy.ValidateSQLExpression(req.Change.GeneratedExpression, "generated expression"); err != nil {
			return nil, err
		}
	}
	if req.Change.Operation == strategy.OpPartitionTable {
		if err := strategy.ValidatePartitionStrategy(req.Change.PartitionStrategy); err != nil {
			return nil, err
		}
		var bounds []strategy.PartitionBound
		if err := json.Unmarshal([]byte(req.Change.PartitionBoundsJSON), &bounds); err != nil {
			return nil, fmt.Errorf("invalid partition bounds: %w", err)
		}
		if len(bounds) == 0 {
			return nil, fmt.Errorf("partition bounds cannot be empty")
		}
		for _, b := range bounds {
			if b.Name == "" {
				return nil, fmt.Errorf("every partition bound needs a name")
			}
			for _, v := range append([]string{b.From, b.To}, b.Values...) {
				if v == "" {
					continue
				}
				if err := strategy.ValidateSQLExpression(v, "partition bound"); err != nil {
					return nil, err
				}
			}
		}
	}

	job := &state.Job{
		ID:                      o.newJobID(),
		SchemaName:              req.SchemaName,
		TableName:               req.TableName,
		Strategy:                string(strat),
		Phase:                   state.PhasePreflight,
		Operation:               string(req.Change.Operation),
		ColumnName:              req.Change.ColumnName,
		ColumnType:              req.Change.NewType,
		DefaultValue:            req.Change.DefaultValue,
		IsVolatileDefault:       req.Change.IsVolatileDefault,
		IndexName:               req.Change.IndexName,
		ConstraintName:          req.Change.ConstraintName,
		CheckExpression:         req.Change.CheckExpression,
		NewColumnName:           req.Change.NewColumnName,
		NewTableName:            req.Change.NewTableName,
		ReferencedTable:         req.Change.ReferencedTable,
		ReferencedColumn:        req.Change.ReferencedColumn,
		OnDelete:                req.Change.OnDelete,
		GeneratedExpression:     req.Change.GeneratedExpression,
		PartitionColumn:         req.Change.PartitionColumn,
		PartitionStrategy:       req.Change.PartitionStrategy,
		PartitionBoundsJSON:     req.Change.PartitionBoundsJSON,
		PartitionIncludeDefault: req.Change.PartitionIncludeDefault,
		EstimatedRowCount:       rawStats.EstimatedRowCount,
		Name:                    req.Name,
		Description:             req.Description,
		CorrelationID:           req.CorrelationID,
		CausationID:             req.CausationID,
		LifecycleID:             req.LifecycleID,
		// NOTE: strategy.ColumnChange.TypeConversionCompatible is
		// deliberately NOT copied here — it's only an input to
		// strategy.Decide's strategy CHOICE, not state any flow needs
		// later, so state.Job has no corresponding field for it.
	}

	if err := o.Store.Create(ctx, job); err != nil {
		return nil, fmt.Errorf("failed to create job checkpoint: %w", err)
	}

	o.logAudit(ctx, job.ID, actor, "MIGRATION_STARTED", "SUCCESS", map[string]any{
		"schema": req.SchemaName, "table": req.TableName, "strategy": string(strat), "operation": string(req.Change.Operation),
	})

	flow, err := o.FlowFor(strat)
	if err != nil {
		o.logAudit(ctx, job.ID, actor, "MIGRATION_STARTED", "FAILURE", map[string]any{"error": err.Error()})
		// Unlike every branch above, a job DOES exist by this point —
		// preserves StartMigration's original "return job, err" (not
		// "nil, err") contract for this specific failure, so a caller
		// can still inspect what WAS persisted even though nothing will
		// ever execute against it.
		return &preparedJob{job: job, actor: actor}, fmt.Errorf("failed to obtain a flow for strategy %s: %w", strat, err)
	}

	return &preparedJob{job: job, flow: flow, actor: actor}, nil
}

// StartMigration validates req, creates a job checkpoint, and runs it
// to completion SYNCHRONOUSLY — the caller doesn't get control back
// until the migration has finished (successfully or not). This is what
// every existing caller (the CLI, the cookie-authenticated human
// dashboard API) has always done and continues to do unchanged; see
// StartMigrationAsync for the alternative this refactor introduces
// alongside it, not instead of it.
func (o *Orchestrator) StartMigration(ctx context.Context, req MigrationRequest) (*state.Job, error) {
	prepared, err := o.prepareJob(ctx, req)
	if prepared == nil {
		return nil, err
	}
	if err != nil {
		return prepared.job, err
	}

	if err := prepared.flow.Execute(ctx, prepared.job); err != nil {
		o.logAudit(ctx, prepared.job.ID, prepared.actor, "MIGRATION_EXECUTE", "FAILURE", map[string]any{"error": err.Error(), "phase": string(prepared.job.Phase)})
		return prepared.job, fmt.Errorf("migration failed: %w", err)
	}

	o.logAudit(ctx, prepared.job.ID, prepared.actor, "MIGRATION_EXECUTE", "SUCCESS", map[string]any{"phase": string(prepared.job.Phase)})
	return prepared.job, nil
}

// StartMigrationAsync mirrors StartMigration's exact validation and
// job-creation logic (via the shared prepareJob) but runs flow.Execute
// in a background goroutine rather than waiting for it, returning the
// job as soon as it's durably created — its Phase will still be
// PhasePreflight (or whatever prepareJob's error path left it at) at
// the moment this function returns, not a terminal phase.
//
// This exists for internal/api's ecosystem-facing POST
// /api/v1/migrations handler (see docs/ecosystem/ARCHITECTURE.md):
// AC-PF-003 Section 13.2's "202 Accepted + operation reference" pattern
// for command responses requires returning as soon as the job is
// durably created, not waiting for it to finish — which can be minutes
// to hours for a SHADOW_TABLE migration, far longer than any HTTP
// client should be expected to block on.
//
// Deliberately runs Execute with context.Background(), not ctx — ctx is
// the INCOMING HTTP request's context, which the http package cancels
// the moment the handler returns (immediately after this function
// hands back the job) — using it for the background Execute call would
// abort the migration within moments of starting it. This mirrors how
// engines/postgresql/reaper's own background sweep loop is deliberately given its
// own long-lived context rather than reusing whatever request triggered
// it.
func (o *Orchestrator) StartMigrationAsync(ctx context.Context, req MigrationRequest) (*state.Job, error) {
	prepared, err := o.prepareJob(ctx, req)
	if prepared == nil {
		return nil, err
	}
	if err != nil {
		return prepared.job, err
	}

	go func() {
		bgCtx := context.Background()
		if err := prepared.flow.Execute(bgCtx, prepared.job); err != nil {
			o.logAudit(bgCtx, prepared.job.ID, prepared.actor, "MIGRATION_EXECUTE", "FAILURE", map[string]any{"error": err.Error(), "phase": string(prepared.job.Phase)})
			return
		}
		o.logAudit(bgCtx, prepared.job.ID, prepared.actor, "MIGRATION_EXECUTE", "SUCCESS", map[string]any{"phase": string(prepared.job.Phase)})
	}()

	return prepared.job, nil
}

// RollbackMigration looks up a job and delegates to its Flow's Rollback,
// per FR-07/FR-08/FR-08a. This requires re-deciding which Flow to use
// (ddlflow vs shadowflow) purely from the persisted job.Strategy string,
// since Rollback can be called in a separate process invocation (e.g. a
// `pgarchimigrator rollback <job-id>` CLI call) long after StartMigration returned.
func (o *Orchestrator) RollbackMigration(ctx context.Context, jobID, actor string) (*state.Job, error) {
	if actor == "" {
		actor = "unknown"
	}

	job, err := o.Store.Get(ctx, jobID)
	if err != nil {
		return nil, fmt.Errorf("failed to load job %q: %w", jobID, err)
	}

	flow, err := o.FlowFor(strategy.Strategy(job.Strategy))
	if err != nil {
		o.logAudit(ctx, jobID, actor, "ROLLBACK", "FAILURE", map[string]any{"error": err.Error()})
		return job, fmt.Errorf("failed to obtain a flow for strategy %s: %w", job.Strategy, err)
	}

	if err := flow.Rollback(ctx, job); err != nil {
		o.logAudit(ctx, jobID, actor, "ROLLBACK", "FAILURE", map[string]any{"error": err.Error(), "phase": string(job.Phase)})
		return job, fmt.Errorf("rollback failed: %w", err)
	}
	o.logAudit(ctx, jobID, actor, "ROLLBACK", "SUCCESS", map[string]any{"phase": string(job.Phase)})
	return job, nil
}

// logAudit is a best-effort helper: a nil AuditWriter (the default) or a
// write failure never blocks or fails the actual migration operation —
// the audit trail is important but must not become a new source of
// migration failures.
func (o *Orchestrator) logAudit(ctx context.Context, jobID, actor, action, result string, detail map[string]any) {
	if o.AuditWriter == nil {
		return
	}
	_ = o.AuditWriter.Write(ctx, auditlog.Entry{
		JobID: jobID, Actor: actor, Action: action, Result: result, Detail: detail,
	})
}

// newJobID uses the injected IDGenerator if set (mainly for deterministic
// tests), otherwise generateJobID.
func (o *Orchestrator) newJobID() string {
	if o.IDGenerator != nil {
		return o.IDGenerator()
	}
	return generateJobID()
}

// generateJobID produces a reasonably unique, sortable-by-time job ID
// without pulling in an external UUID dependency: a nanosecond timestamp
// plus 8 random bytes is more than sufficient collision resistance for a
// single-instance deployment (TR-13).
func generateJobID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf) // crypto/rand.Read never returns a partial read on success; error is vanishingly rare and non-actionable here
	return fmt.Sprintf("job-%d-%s", time.Now().UnixNano(), hex.EncodeToString(buf))
}
