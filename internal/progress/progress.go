// Package progress computes a human-readable stage list and completion
// percentage for a migration job, for FR-04 ("progress report at every
// step") and US-03 ("I want to see the migration's progress on a
// dashboard"). It builds on top of internal/state.Job.Phase and does not
// require any additional persistence.
package progress

import (
	"fmt"
	"strings"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// StageStatus represents where a single pipeline stage stands relative to
// the job's current phase.
type StageStatus string

const (
	StageDone    StageStatus = "DONE"
	StageCurrent StageStatus = "CURRENT"
	StagePending StageStatus = "PENDING"
)

// StageView is a single pipeline stage annotated with its status, ready for display.
type StageView struct {
	Phase  state.Phase
	Status StageStatus
}

// Report is the full progress report for a single job.
type Report struct {
	JobID           string
	SchemaName      string
	TableName       string
	Strategy        string
	CurrentPhase    state.Phase
	Stages          []StageView
	PercentComplete float64
	Terminal        bool // true once the job has reached COMPLETED, FAILED, or ABORTED
	Failed          bool
	LastError       string
	// CreatedAt/UpdatedAt are copied directly from state.Job — UpdatedAt
	// doubles as "finished at" once Terminal is true, since nothing
	// updates a job's row after that point.
	CreatedAt time.Time
	UpdatedAt time.Time
	// EstimatedRowCount/RowsProcessed — see Job.EstimatedRowCount and
	// Job.RowsProcessed's doc comments for what these mean and their
	// limitations (a point-in-time snapshot, and a counter that only
	// batched backfill operations populate, respectively).
	EstimatedRowCount int64
	RowsProcessed     int64
	// Name/Description mirror state.Job.Name/Description — see that
	// field's doc comment. Both are always present (possibly empty
	// strings) in the JSON response; the frontend falls back to
	// "schema.table" when Name is empty.
	Name        string
	Description string
	Operation   string
	// OperationSummary is a short, human-readable description of what
	// this job's operation actually does (e.g. `Added column "status"
	// (text) with default 'active'`) — always populated, regardless of
	// phase, straight from the job's own persisted parameters.
	OperationSummary string
	// Statements is a best-effort reconstruction of the DDL this job
	// executed, built directly from the job's persisted parameters
	// (not captured verbatim at execution time — there was no need to
	// store it separately when it's fully derivable from fields already
	// on the job). Deliberately empty for ALTER_COLUMN_TYPE under the
	// SHADOW_TABLE strategy: that flow is a genuine multi-step pipeline
	// with no single short statement list that honestly represents it —
	// see internal/preview's buildAlterTypePreview for the identical
	// reasoning applied to the forward-looking dry-run case.
	// OperationSummary still describes it in prose even when this is empty.
	Statements []string

	// ReplicationLagBytes/ReplicationLagTrend are populated ONLY by the
	// API layer (see internal/api's attachReplicationLag), never by
	// Compute itself — Compute has no database access, and querying
	// pg_replication_slots is only worth the extra round-trip for the
	// single-job "get migration status" endpoint a human or the web
	// dashboard is actively polling, not the CLI's Render() output or
	// the bulk "list all migrations" endpoint (which would multiply the
	// query by however many jobs are listed, mostly wasted since most
	// of them aren't being actively watched).
	//
	// nil/"" means "not applicable" — not a SHADOW_TABLE job, no active
	// replication slot right now, or this Report was never enriched —
	// not an error state. ReplicationLagTrend is one of "growing",
	// "shrinking", "stable", or "unknown" (no prior reading yet to
	// compare against — e.g. the very first poll after the slot was
	// created).
	ReplicationLagBytes *int64 `json:",omitempty"`
	ReplicationLagTrend string `json:",omitempty"`

	// ReplicationLagGrowingForSeconds is set ONLY once lag has been
	// continuously growing (no intervening stable/shrinking reading) for
	// at least a few minutes — see internal/api's
	// sustainedGrowthEscalationThreshold for the exact bar and the full
	// reasoning for why this is a distinct, escalated signal from
	// ReplicationLagTrend=="growing" alone, and deliberately NOT an
	// automatic-abort trigger (a load test's synthetic write pressure
	// genuinely never converges, but real production traffic easily
	// could — the decision to actually stop a migration is left to
	// the person watching, this only makes the signal impossible to
	// miss).
	ReplicationLagGrowingForSeconds *int64 `json:",omitempty"`

	// ResourceStatus is populated ONLY by the API layer (see internal/api's
	// attachResourceStatus), never by Compute itself, and only for
	// TERMINAL jobs — see that function's own doc comment for the full
	// "why" (a real incident: an orphaned shadow table + a permanently
	// mis-owned sequence that sat invisible until found via manual psql
	// investigation). Every relevant transient resource for the job's
	// strategy is reported, whether it's confirmed gone (Exists: false —
	// the healthy, common case) or still lingering (Exists: true — the
	// exact situation the real incident above looked like from the
	// outside) — always showing the clean checks too, not just problems,
	// is deliberate: "we checked and it's clean" is a stronger, more
	// verifiable form of confidence than silence implying health. nil
	// means this Report hasn't been enriched yet (e.g. Compute's own
	// direct output, before the API layer's enrichment step runs).
	ResourceStatus []ResourceStatus `json:",omitempty"`

	// CheckpointPressureDetected is populated ONLY by the API layer (see
	// internal/api's attachCheckpointPressure), never by Compute itself,
	// and only for a NON-terminal (still running) job — see that
	// function's own doc comment for the full "why" (a real incident:
	// PostgreSQL checkpoints forced every 6-22 seconds under heavy write
	// load, one taking 90 seconds, causing latency spikes that looked
	// identical to an application bug from the outside). Deliberately a
	// plain bool, only ever present (omitempty) when true — unlike
	// ResourceStatus, this is an external, environmental signal rather
	// than a verification of the tool's own actions, so it's shown as an
	// occasional, situational note rather than a persistent status
	// indicator.
	CheckpointPressureDetected bool `json:",omitempty"`

	// ImpactActiveQueries is populated ONLY while the migration is still
	// running AND explicitly requested (GET /api/migrations/{id}?measureImpact=true)
	// — see internal/api's attachImpactMeasurement doc comment for why
	// this is the one trust-layer indicator that's opt-in while running,
	// unlike the others: unlike a cheap system-view read, the underlying
	// query (a three-way join across pg_locks/pg_class/pg_stat_activity)
	// has genuinely non-trivial cost run on every poll.
	//
	// ImpactPeakQueryDurationSeconds behaves differently depending on
	// whether the job is terminal: while RUNNING, it's a live running
	// maximum across the migration so far (opt-in, same as
	// ImpactActiveQueries above — see impactTracker for why a single
	// instantaneous snapshot could easily miss a spike between polls).
	// Once TERMINAL, it's read from the durable, already-persisted
	// state.Job.ImpactPeakQueryDurationSeconds field instead — shown
	// UNCONDITIONALLY (no measureImpact needed once finished, since
	// reading an already-fetched field costs nothing extra) as the
	// automatic post-migration impact report. nil means impact
	// measurement was simply never turned on for this migration, not
	// "measured and found to be zero".
	ImpactActiveQueries            *int     `json:",omitempty"`
	ImpactPeakQueryDurationSeconds *float64 `json:",omitempty"`
}

// ResourceStatus is a single transient resource's LIVE, directly-verified
// state — not a log entry claiming what happened, an actual query
// confirming what currently exists right now. See attachResourceStatus's
// doc comment (internal/api) for the incident this exists to make
// visible.
type ResourceStatus struct {
	// Name is a short, human-readable label — "Shadow table",
	// "Replication slot", "Publication", "Temporary backfill index".
	Name string `json:"name"`
	// Detail is the actual PostgreSQL object name, e.g.
	// "__pgam_shadow_orders_job123" — shown so an operator with direct
	// database access can independently verify this themselves via psql
	// if they want to, not asked to just trust the badge.
	Detail string `json:"detail"`
	// Exists reflects a live query, not a log entry — true means this
	// resource was just confirmed to still be present (potentially
	// concerning for a terminal job), false means it was just confirmed
	// gone (the healthy, expected state once a migration has finished).
	Exists bool `json:"exists"`
}

// pipelineFor returns the ordered, strategy-specific sequence of phases a
// job is expected to pass through. This mirrors exactly what each flow
// actually transitions through — see internal/ddlflow.executeDirectAddColumn
// / executeExpandBackfill / executeDropColumn, and the 8-step flow
// described in Architecture Doc Section 4.1 for the shadow-table flow.
//
// operation matters here, not just strategy: DIRECT_DDL covers BOTH a
// plain ADD_COLUMN (2 stages, no rollback window) AND DROP_COLUMN's
// two-phase soft-drop (3 stages, WITH a rollback window) — the same
// strategy name maps to two different actual phase sequences depending on
// which operation is running.
func pipelineFor(strategyName, operation string) []state.Phase {
	switch strategyName {
	case "DIRECT_DDL":
		switch operation {
		case "DROP_COLUMN":
			return []state.Phase{state.PhasePreparation, state.PhaseRollbackWindow, state.PhaseCompleted}
		case "SET_NOT_NULL", "ADD_CONSTRAINT":
			return []state.Phase{state.PhasePreparation, state.PhaseValidating, state.PhaseCompleted}
		case "ADD_INDEX":
			// CREATE INDEX CONCURRENTLY has a known PostgreSQL failure
			// mode: if interrupted, it can complete "successfully" while
			// leaving an INVALID index behind (see executeAddIndex's own
			// isIndexValid check, which already fails the job rather
			// than accepting that outcome silently). Giving this its own
			// explicit VALIDATING stage — rather than folding it into
			// the plain 2-stage default below — makes that already-
			// enforced guarantee VISIBLE (picked up automatically by
			// the Migration Detail page's Health Card, see
			// computeHealthSummary), not just internally enforced.
			return []state.Phase{state.PhasePreparation, state.PhaseValidating, state.PhaseCompleted}
		default: // ADD_COLUMN, DROP_INDEX
			return []state.Phase{state.PhasePreparation, state.PhaseCompleted}
		}
	case "EXPAND_BACKFILL":
		// Same 4-stage pipeline for both users of this strategy: ADD_COLUMN's
		// volatile-default backfill and RENAME_COLUMN's new-column backfill
		// (see internal/ddlflow.executeExpandBackfill / executeRenameColumn).
		// PhaseSyncing means "backfilling the new column from the old one"
		// for RENAME_COLUMN specifically, but the stage SHAPE is identical.
		return []state.Phase{state.PhasePreparation, state.PhaseSyncing, state.PhaseValidating, state.PhaseCompleted}
	case "SHADOW_TABLE":
		return []state.Phase{
			state.PhasePreflight, state.PhasePreparation, state.PhaseSyncing,
			state.PhaseDeltaSync, state.PhaseValidating, state.PhaseSwapping,
			state.PhaseRollbackWindow, state.PhaseCleanup, state.PhaseCompleted,
		}
	default:
		// Unknown strategy: fall back to a minimal two-stage pipeline so the
		// caller still gets a usable report instead of an empty one.
		return []state.Phase{state.PhasePreparation, state.PhaseCompleted}
	}
}

// Compute builds a Report for the given job's current state.
func Compute(job *state.Job) *Report {
	pipeline := pipelineFor(job.Strategy, job.Operation)

	summary, statements := describeOperation(job)

	report := &Report{
		JobID:             job.ID,
		SchemaName:        job.SchemaName,
		TableName:         job.TableName,
		Strategy:          job.Strategy,
		CurrentPhase:      job.Phase,
		LastError:         job.LastError,
		CreatedAt:         job.CreatedAt,
		UpdatedAt:         job.UpdatedAt,
		EstimatedRowCount: job.EstimatedRowCount,
		RowsProcessed:     job.RowsProcessed,
		Name:              job.Name,
		Description:       job.Description,
		Operation:         job.Operation,
		OperationSummary:  summary,
		Statements:        statements,
	}

	// KNOWN LIMITATION: the current data model overwrites Phase directly to
	// FAILED/ABORTED (see internal/ddlflow.fail and internal/reaper.ScanOnce)
	// and does not retain which stage the job had reached beforehand. An
	// exact percentage or stage breakdown cannot be computed here, so we
	// report this honestly as an interruption rather than guessing.
	if job.Phase == state.PhaseFailed || job.Phase == state.PhaseAborted {
		report.Terminal = true
		report.Failed = job.Phase == state.PhaseFailed
		report.Stages = []StageView{{Phase: job.Phase, Status: StageCurrent}}
		report.PercentComplete = 0
		return report
	}

	idx := indexOfPhase(pipeline, job.Phase)

	stages := make([]StageView, len(pipeline))
	for i, p := range pipeline {
		status := StagePending
		if idx >= 0 {
			switch {
			case i < idx:
				status = StageDone
			case i == idx:
				status = StageCurrent
			}
		}
		stages[i] = StageView{Phase: p, Status: status}
	}

	if job.Phase == state.PhaseCompleted {
		report.Terminal = true
		report.PercentComplete = 100
		// The final stage is fully done, not merely "in progress".
		if len(stages) > 0 {
			stages[len(stages)-1].Status = StageDone
		}
		report.Stages = stages
		return report
	}

	report.Stages = stages

	if idx < 0 {
		// The job's current phase isn't part of this strategy's known
		// pipeline (e.g. a phase from a different flow, or a bug) — report
		// 0% rather than a misleading guess.
		report.PercentComplete = 0
		return report
	}

	// The current stage is actively in progress, so it gets partial (50%)
	// credit within its own slot rather than counting as fully 0% or 100%.
	report.PercentComplete = (float64(idx) + 0.5) / float64(len(pipeline)-1) * 100
	if report.PercentComplete > 99 {
		report.PercentComplete = 99 // never show 100% until the job is truly COMPLETED
	}
	return report
}

func indexOfPhase(pipeline []state.Phase, phase state.Phase) int {
	for i, p := range pipeline {
		if p == phase {
			return i
		}
	}
	return -1
}

// Render produces a plain-text, CLI-friendly rendering of the report —
// used by `pgarchimigrator status` (see cmd/pgarchimigrator/main.go).
func (r *Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Job %s (%s) — %s.%s\n", r.JobID, r.Strategy, r.SchemaName, r.TableName)

	for _, s := range r.Stages {
		symbol := "[ ]"
		switch s.Status {
		case StageDone:
			symbol = "[x]"
		case StageCurrent:
			symbol = "[>]"
		}
		fmt.Fprintf(&b, "  %s %s\n", symbol, s.Phase)
	}

	switch {
	case r.Terminal && r.Failed:
		fmt.Fprintf(&b, "Status: FAILED — %s\n", r.LastError)
	case r.Terminal && r.CurrentPhase == state.PhaseAborted:
		// The current data model doesn't retain WHY a job was aborted —
		// internal/reaper.ScanOnce (orphan cleanup) and a user-triggered
		// Rollback() both just set Phase=ABORTED with no distinguishing
		// signal (neither sets LastError). Previously this unconditionally
		// said "cleaned up as an orphaned job", which is actively
		// misleading for the (very common) case of a deliberate, successful
		// user rollback — see the real-world report that caught this.
		// Acknowledging both possibilities is honest; claiming one
		// specific cause without evidence is not.
		fmt.Fprintf(&b, "Status: ABORTED (rolled back, or cleaned up as an orphaned job)\n")
	case r.Terminal:
		fmt.Fprintf(&b, "Status: COMPLETED (100%%)\n")
	default:
		fmt.Fprintf(&b, "Progress: %.0f%%\n", r.PercentComplete)
	}

	if !r.CreatedAt.IsZero() {
		fmt.Fprintf(&b, "Started: %s\n", r.CreatedAt.Format(time.RFC3339))
		if r.Terminal {
			fmt.Fprintf(&b, "Finished: %s (duration: %s)\n", r.UpdatedAt.Format(time.RFC3339), r.UpdatedAt.Sub(r.CreatedAt).Round(time.Second))
		} else {
			fmt.Fprintf(&b, "Elapsed: %s\n", time.Since(r.CreatedAt).Round(time.Second))
		}
	}
	if r.RowsProcessed > 0 {
		if r.EstimatedRowCount > 0 {
			fmt.Fprintf(&b, "Rows processed: %d (~%d estimated)\n", r.RowsProcessed, r.EstimatedRowCount)
		} else {
			fmt.Fprintf(&b, "Rows processed: %d\n", r.RowsProcessed)
		}
	}

	return b.String()
}

// quoteIdent double-quotes a PostgreSQL identifier for display purposes —
// mirrors the same technique used at actual execution time in
// internal/ddlflow (see its own quoteIdent), so the statements shown here
// look exactly like what really ran, not an approximation.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// describeOperation builds a short, human-readable summary of what a
// job's operation does, plus (where the flow that ran it used a
// deterministic, reproducible sequence of statements) a best-effort
// reconstruction of the actual DDL — built entirely from the job's own
// persisted parameters, since nothing captures the executed SQL text
// verbatim at execution time. See Report.Statements' doc comment for why
// SHADOW_TABLE ALTER_COLUMN_TYPE deliberately returns no statements.
func describeOperation(job *state.Job) (summary string, statements []string) {
	qualified := quoteIdent(job.SchemaName) + "." + quoteIdent(job.TableName)

	switch job.Operation {
	case "ADD_COLUMN":
		summary = fmt.Sprintf("Added column %q (%s) to %s.%s", job.ColumnName, job.ColumnType, job.SchemaName, job.TableName)
		stmt := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", qualified, quoteIdent(job.ColumnName), job.ColumnType)
		if job.DefaultValue != "" {
			stmt += " DEFAULT " + job.DefaultValue
			summary += fmt.Sprintf(" with default %s", job.DefaultValue)
		}
		if job.IsVolatileDefault {
			summary += " — backfilled in batches (volatile default)"
		}
		return summary, []string{stmt}

	case "DROP_COLUMN":
		summary = fmt.Sprintf("Dropped column %q from %s.%s", job.ColumnName, job.SchemaName, job.TableName)
		if job.DeprecatedColumnName == "" {
			// Explicitly []string{}, not nil: a nil Go slice marshals to
			// JSON `null`, and the frontend does `job.Statements.length`
			// unconditionally — this exact bug (a real, user-reported
			// crash) was already found and fixed once in
			// internal/preview.Generate; reusing bare `nil` here
			// reintroduced the identical class of bug in a different
			// function. See ListSchemas' doc comment in internal/catalog
			// for the fullest explanation of why this matters.
			return summary, []string{}
		}
		// The actual DROP happens later, executed by the reaper once the
		// rollback window closes — what ddlflow itself runs synchronously
		// is only the soft-drop rename.
		summary += " (two-phase soft drop: renamed, then permanently removed once the rollback window closed)"
		stmt := fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO %s", qualified, quoteIdent(job.ColumnName), quoteIdent(job.DeprecatedColumnName))
		return summary, []string{stmt}

	case "ALTER_COLUMN_TYPE":
		summary = fmt.Sprintf("Changed column %q on %s.%s to type %s", job.ColumnName, job.SchemaName, job.TableName, job.ColumnType)
		if job.Strategy == "SHADOW_TABLE" {
			summary += " via a shadow table + logical replication (no single short statement represents this multi-step flow)"
			// []string{}, not nil — see the identical comment on the
			// DROP_COLUMN case above for why this specific detail is a
			// real, previously-shipped crash, not just style pedantry.
			return summary, []string{}
		}
		stmt := fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s USING %s::%s",
			qualified, quoteIdent(job.ColumnName), job.ColumnType, quoteIdent(job.ColumnName), job.ColumnType)
		return summary, []string{stmt}

	case "ADD_INDEX":
		summary = fmt.Sprintf("Created index %q on %s.%s (%s)", job.IndexName, job.SchemaName, job.TableName, job.ColumnName)
		stmt := fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s)", quoteIdent(job.IndexName), qualified, quoteIdent(job.ColumnName))
		return summary, []string{stmt}

	case "DROP_INDEX":
		summary = fmt.Sprintf("Dropped index %q from %s.%s", job.IndexName, job.SchemaName, job.TableName)
		stmt := fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s.%s", quoteIdent(job.SchemaName), quoteIdent(job.IndexName))
		return summary, []string{stmt}

	case "SET_NOT_NULL":
		summary = fmt.Sprintf("Made column %q NOT NULL on %s.%s", job.ColumnName, job.SchemaName, job.TableName)
		constraintName := job.ConstraintName
		if constraintName == "" {
			// Matches ddlflow's own auto-generation naming for a job
			// whose checkpoint predates the constraint name being
			// resolved yet, or where it's genuinely still empty.
			constraintName = fmt.Sprintf("%s_%s_not_null_check", job.TableName, job.ColumnName)
		}
		qConstraint := quoteIdent(constraintName)
		statements = []string{
			fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID", qualified, qConstraint, quoteIdent(job.ColumnName)),
			fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", qualified, qConstraint),
			fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", qualified, quoteIdent(job.ColumnName)),
			fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", qualified, qConstraint),
		}
		return summary, statements

	case "ADD_CONSTRAINT":
		summary = fmt.Sprintf("Added check constraint %q on %s.%s: %s", job.ConstraintName, job.SchemaName, job.TableName, job.CheckExpression)
		qConstraint := quoteIdent(job.ConstraintName)
		statements = []string{
			fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID", qualified, qConstraint, job.CheckExpression),
			fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", qualified, qConstraint),
		}
		return summary, statements

	case "RENAME_COLUMN":
		summary = fmt.Sprintf("Renamed column %q to %q on %s.%s (dual-write: both names work until the old one is explicitly dropped)",
			job.ColumnName, job.NewColumnName, job.SchemaName, job.TableName)
		statements = []string{
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s <same type as %s>", qualified, quoteIdent(job.NewColumnName), job.ColumnName),
			"-- a trigger keeps both columns synchronized on every INSERT/UPDATE",
			fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s IS NULL -- backfill, batched",
				qualified, quoteIdent(job.NewColumnName), quoteIdent(job.ColumnName), quoteIdent(job.NewColumnName)),
		}
		return summary, statements

	default:
		// Unknown/future operation — degrade gracefully rather than
		// showing nothing. []string{}, not nil — same reasoning as the
		// other two returns above.
		return fmt.Sprintf("%s on %s.%s", job.Operation, job.SchemaName, job.TableName), []string{}
	}
}
