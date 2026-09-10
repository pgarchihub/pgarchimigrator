package progress

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 0.01
}

func TestCompute_DirectDDL_AtPreparation(t *testing.T) {
	job := &state.Job{ID: "j1", Strategy: "DIRECT_DDL", Phase: state.PhasePreparation}
	report := Compute(job)

	if report.Terminal {
		t.Error("expected Terminal=false while still in PREPARATION")
	}
	if !almostEqual(report.PercentComplete, 50) {
		t.Errorf("expected ~50%%, got %.2f%%", report.PercentComplete)
	}
	if len(report.Stages) != 2 {
		t.Fatalf("expected 2 stages for DIRECT_DDL, got %d", len(report.Stages))
	}
	if report.Stages[0].Status != StageCurrent {
		t.Errorf("expected stage 0 to be CURRENT, got %s", report.Stages[0].Status)
	}
	if report.Stages[1].Status != StagePending {
		t.Errorf("expected stage 1 to be PENDING, got %s", report.Stages[1].Status)
	}
}

func TestCompute_DirectDDL_AtCompleted(t *testing.T) {
	job := &state.Job{ID: "j2", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted}
	report := Compute(job)

	if !report.Terminal {
		t.Error("expected Terminal=true at COMPLETED")
	}
	if report.PercentComplete != 100 {
		t.Errorf("expected 100%%, got %.2f%%", report.PercentComplete)
	}
	for i, s := range report.Stages {
		if s.Status != StageDone {
			t.Errorf("expected stage %d to be DONE at COMPLETED, got %s", i, s.Status)
		}
	}
}

func TestCompute_ExpandBackfill_AtSyncing(t *testing.T) {
	job := &state.Job{ID: "j3", Strategy: "EXPAND_BACKFILL", Phase: state.PhaseSyncing}
	report := Compute(job)

	// pipeline: [Preparation, Syncing, Validating, Completed] -> idx=1, len=4
	// percent = (1+0.5)/3*100 = 50
	if !almostEqual(report.PercentComplete, 50) {
		t.Errorf("expected ~50%%, got %.2f%%", report.PercentComplete)
	}
	if report.Stages[0].Status != StageDone {
		t.Errorf("expected PREPARATION to be DONE, got %s", report.Stages[0].Status)
	}
	if report.Stages[1].Status != StageCurrent {
		t.Errorf("expected SYNCING to be CURRENT, got %s", report.Stages[1].Status)
	}
	if report.Stages[2].Status != StagePending || report.Stages[3].Status != StagePending {
		t.Error("expected VALIDATING and COMPLETED to be PENDING")
	}
}

func TestCompute_ExpandBackfill_AtValidating(t *testing.T) {
	job := &state.Job{ID: "j4", Strategy: "EXPAND_BACKFILL", Phase: state.PhaseValidating}
	report := Compute(job)

	// idx=2, len=4 -> percent = (2+0.5)/3*100 = 83.33
	if !almostEqual(report.PercentComplete, 83.33) {
		t.Errorf("expected ~83.33%%, got %.2f%%", report.PercentComplete)
	}
}

func TestCompute_ShadowTable_FullPipelineLength(t *testing.T) {
	job := &state.Job{ID: "j5", Strategy: "SHADOW_TABLE", Phase: state.PhaseSwapping}
	report := Compute(job)

	if len(report.Stages) != 9 {
		t.Fatalf("expected 9 stages for SHADOW_TABLE, got %d", len(report.Stages))
	}
	// Swapping is index 5 (0=Preflight,1=Preparation,2=Syncing,3=DeltaSync,4=Validating,5=Swapping)
	if report.Stages[5].Status != StageCurrent {
		t.Errorf("expected SWAPPING (index 5) to be CURRENT, got %s", report.Stages[5].Status)
	}
	for i := 0; i < 5; i++ {
		if report.Stages[i].Status != StageDone {
			t.Errorf("expected stage %d to be DONE, got %s", i, report.Stages[i].Status)
		}
	}
	for i := 6; i < 9; i++ {
		if report.Stages[i].Status != StagePending {
			t.Errorf("expected stage %d to be PENDING, got %s", i, report.Stages[i].Status)
		}
	}
}

func TestCompute_Failed_ReportsInterruptionHonestly(t *testing.T) {
	job := &state.Job{ID: "j6", Strategy: "EXPAND_BACKFILL", Phase: state.PhaseFailed, LastError: "backfill incomplete: 3 row(s) still NULL"}
	report := Compute(job)

	if !report.Terminal {
		t.Error("expected Terminal=true for FAILED")
	}
	if !report.Failed {
		t.Error("expected Failed=true")
	}
	if report.PercentComplete != 0 {
		t.Errorf("expected 0%% for FAILED (no reliable stage data), got %.2f%%", report.PercentComplete)
	}
	if report.LastError == "" {
		t.Error("expected LastError to be populated")
	}
}

func TestCompute_Aborted_ReportsInterruptionHonestly(t *testing.T) {
	job := &state.Job{ID: "j7", Strategy: "SHADOW_TABLE", Phase: state.PhaseAborted}
	report := Compute(job)

	if !report.Terminal {
		t.Error("expected Terminal=true for ABORTED")
	}
	if report.Failed {
		t.Error("expected Failed=false for ABORTED (it's not a failure, it's a cleanup)")
	}
}

func TestCompute_UnknownStrategy_FallsBackGracefully(t *testing.T) {
	job := &state.Job{ID: "j8", Strategy: "SOMETHING_UNKNOWN", Phase: state.PhasePreparation}
	report := Compute(job)

	if len(report.Stages) != 2 {
		t.Fatalf("expected the 2-stage fallback pipeline, got %d stages", len(report.Stages))
	}
}

func TestRender_ProducesNonEmptyOutput(t *testing.T) {
	job := &state.Job{ID: "j9", Strategy: "DIRECT_DDL", Phase: state.PhasePreparation}
	report := Compute(job)

	output := report.Render()
	if output == "" {
		t.Fatal("expected non-empty rendered output")
	}
	if !strings.Contains(output, "j9") {
		t.Error("expected the rendered output to contain the job ID")
	}
}

// TestCompute_DropColumn_AtRollbackWindow is a regression test for a real
// bug found via manual CLI testing: DIRECT_DDL previously always mapped to
// the plain 2-stage ADD_COLUMN pipeline ([PREPARATION, COMPLETED]),
// regardless of Operation. A DROP_COLUMN job sitting in ROLLBACK_WINDOW
// (a phase that pipeline doesn't contain at all) was therefore reported
// as "0% complete, nothing done" no matter how long it had actually been
// running — `status` looked identical whether the job had just started or
// was about to finalize. pipelineFor is now Operation-aware for
// DIRECT_DDL specifically to fix this.
func TestCompute_DropColumn_AtRollbackWindow(t *testing.T) {
	job := &state.Job{ID: "j10", Strategy: "DIRECT_DDL", Operation: "DROP_COLUMN", Phase: state.PhaseRollbackWindow}
	report := Compute(job)

	if report.Terminal {
		t.Error("expected Terminal=false while still in ROLLBACK_WINDOW")
	}
	if len(report.Stages) != 3 {
		t.Fatalf("expected 3 stages for DIRECT_DDL/DROP_COLUMN (PREPARATION, ROLLBACK_WINDOW, COMPLETED), got %d", len(report.Stages))
	}
	if report.Stages[0].Status != StageDone {
		t.Errorf("expected PREPARATION (stage 0) to be DONE, got %s", report.Stages[0].Status)
	}
	if report.Stages[1].Status != StageCurrent {
		t.Errorf("expected ROLLBACK_WINDOW (stage 1) to be CURRENT, got %s", report.Stages[1].Status)
	}
	if report.Stages[2].Status != StagePending {
		t.Errorf("expected COMPLETED (stage 2) to be PENDING, got %s", report.Stages[2].Status)
	}
	// The core of the bug: percent must NOT be stuck at 0 just because the
	// phase wasn't found in a pipeline that didn't know about it.
	if report.PercentComplete <= 0 {
		t.Errorf("expected PercentComplete > 0 at ROLLBACK_WINDOW (stage 2 of 3), got %.2f%%", report.PercentComplete)
	}
}

// TestCompute_AddColumn_DirectDDL_StillUsesTwoStagePipeline verifies the
// fix above didn't regress the plain ADD_COLUMN case, which must still
// use the simple 2-stage pipeline (no rollback window exists for it).
func TestCompute_AddColumn_DirectDDL_StillUsesTwoStagePipeline(t *testing.T) {
	job := &state.Job{ID: "j11", Strategy: "DIRECT_DDL", Operation: "ADD_COLUMN", Phase: state.PhasePreparation}
	report := Compute(job)

	if len(report.Stages) != 2 {
		t.Fatalf("expected the plain 2-stage ADD_COLUMN pipeline, got %d stages", len(report.Stages))
	}
}

// TestCompute_AddIndex_DirectDDL_HasExplicitValidatingStage is the direct
// regression test for a real gap found via code review (not a bug per
// se — internal/engines/postgresql/ddlflow.executeAddIndex already enforced index validity
// and already failed the job on an invalid index — but that guarantee
// was invisible, folded into the same plain 2-stage pipeline as
// ADD_COLUMN, with no distinct VALIDATING stage the Migration Detail
// page's Health Card (see MigrationDetail.tsx's computeHealthSummary)
// could pick up automatically the way it already does for SET_NOT_NULL/
// ADD_CONSTRAINT/EXPAND_BACKFILL.
func TestCompute_AddIndex_DirectDDL_HasExplicitValidatingStage(t *testing.T) {
	job := &state.Job{ID: "j11b", Strategy: "DIRECT_DDL", Operation: "ADD_INDEX", Phase: state.PhaseCompleted}
	report := Compute(job)

	if len(report.Stages) != 3 {
		t.Fatalf("expected a 3-stage pipeline (PREPARATION, VALIDATING, COMPLETED) for ADD_INDEX, got %d stages: %+v", len(report.Stages), report.Stages)
	}
	if report.Stages[1].Phase != state.PhaseValidating {
		t.Errorf("expected stage 1 to be VALIDATING, got %s", report.Stages[1].Phase)
	}
}

// TestCompute_DropIndex_DirectDDL_StillUsesTwoStagePipeline confirms the
// fix above didn't overreach: DROP_INDEX has nothing to validate (an
// index either exists and gets dropped, or the drop fails outright — see
// executeDropIndex, which has no validity check at all), so it must
// still use the plain 2-stage pipeline, not gain a phantom VALIDATING
// stage that never actually transitions anywhere.
func TestCompute_DropIndex_DirectDDL_StillUsesTwoStagePipeline(t *testing.T) {
	job := &state.Job{ID: "j11c", Strategy: "DIRECT_DDL", Operation: "DROP_INDEX", Phase: state.PhaseCompleted}
	report := Compute(job)

	if len(report.Stages) != 2 {
		t.Fatalf("expected the plain 2-stage DROP_INDEX pipeline, got %d stages: %+v", len(report.Stages), report.Stages)
	}
}

// TestCompute_RenameTable_DirectDDL_UsesTwoStagePipeline confirms
// RENAME_TABLE (RENAME TO + a single CREATE VIEW, see
// strategy.OpRenameTable's own doc comment) also uses the plain 2-stage
// pipeline — like DROP_INDEX above, there's no separate "is this
// correct" check beyond the two statements themselves succeeding, so no
// distinct VALIDATING stage is warranted the way ADD_INDEX's genuinely
// has one.
func TestCompute_RenameTable_DirectDDL_UsesTwoStagePipeline(t *testing.T) {
	job := &state.Job{ID: "j11d", Strategy: "DIRECT_DDL", Operation: "RENAME_TABLE", Phase: state.PhaseCompleted}
	report := Compute(job)

	if len(report.Stages) != 2 {
		t.Fatalf("expected the plain 2-stage RENAME_TABLE pipeline, got %d stages: %+v", len(report.Stages), report.Stages)
	}
}

// TestRender_RenameTable_MentionsCompatibilityView is the direct
// regression test for Render()'s RENAME_TABLE summary actually
// describing the real, distinguishing behavior (a compatibility view
// under the old name) — not just a generic "renamed" message that would
// be misleading about what actually happens to callers still using the
// old name.
func TestRender_RenameTable_MentionsCompatibilityView(t *testing.T) {
	job := &state.Job{
		ID: "j11e", Strategy: "DIRECT_DDL", Operation: "RENAME_TABLE", Phase: state.PhaseCompleted,
		SchemaName: "public", TableName: "orders", NewTableName: "orders_v2",
	}
	report := Compute(job)

	if !strings.Contains(report.OperationSummary, "orders_v2") {
		t.Errorf("expected the summary to mention the new table name, got %q", report.OperationSummary)
	}
	if !strings.Contains(report.OperationSummary, "compatibility view") {
		t.Errorf("expected the summary to mention the compatibility view left under the old name, got %q", report.OperationSummary)
	}
}

// TestCompute_AddForeignKey_DirectDDL_HasValidatingStage confirms
// ADD_FOREIGN_KEY gets the same 3-stage VALIDATING pipeline as
// SET_NOT_NULL/ADD_CONSTRAINT — it uses the identical NOT VALID +
// VALIDATE CONSTRAINT mechanism (see executeAddForeignKey's own doc
// comment), so the same guarantee deserves the same visibility.
func TestCompute_AddForeignKey_DirectDDL_HasValidatingStage(t *testing.T) {
	job := &state.Job{ID: "j11f", Strategy: "DIRECT_DDL", Operation: "ADD_FOREIGN_KEY", Phase: state.PhaseCompleted}
	report := Compute(job)

	if len(report.Stages) != 3 {
		t.Fatalf("expected a 3-stage pipeline (PREPARATION, VALIDATING, COMPLETED) for ADD_FOREIGN_KEY, got %d stages: %+v", len(report.Stages), report.Stages)
	}
	if report.Stages[1].Phase != state.PhaseValidating {
		t.Errorf("expected stage 1 to be VALIDATING, got %s", report.Stages[1].Phase)
	}
}

// TestRender_AddForeignKey_MentionsReferencedTableAndColumn is the
// direct regression test for Render()'s summary describing WHAT the
// foreign key actually points at, not just that "a foreign key was
// added" — a reviewer reading this needs to know the referenced table
// and column to understand the relationship being enforced.
func TestRender_AddForeignKey_MentionsReferencedTableAndColumn(t *testing.T) {
	job := &state.Job{
		ID: "j11g", Strategy: "DIRECT_DDL", Operation: "ADD_FOREIGN_KEY", Phase: state.PhaseCompleted,
		SchemaName: "public", TableName: "orders", ColumnName: "customer_id",
		ConstraintName: "fk_orders_customer", ReferencedTable: "customers", ReferencedColumn: "id",
		OnDelete: "CASCADE",
	}
	report := Compute(job)

	for _, want := range []string{"customer_id", "customers", "id", "CASCADE"} {
		if !strings.Contains(report.OperationSummary, want) {
			t.Errorf("expected the summary to mention %q, got %q", want, report.OperationSummary)
		}
	}
}

// TestCompute_AddGeneratedColumn_DirectDDL_UsesTwoStagePipeline confirms
// the small-table (native GENERATED) path uses the plain 2-stage
// pipeline — a single ALTER TABLE statement, no separate validation step
// the way ADD_INDEX/SET_NOT_NULL genuinely have one.
func TestCompute_AddGeneratedColumn_DirectDDL_UsesTwoStagePipeline(t *testing.T) {
	job := &state.Job{ID: "j11h", Strategy: "DIRECT_DDL", Operation: "ADD_GENERATED_COLUMN", Phase: state.PhaseCompleted}
	report := Compute(job)

	if len(report.Stages) != 2 {
		t.Fatalf("expected the plain 2-stage pipeline for the DIRECT_DDL path, got %d stages: %+v", len(report.Stages), report.Stages)
	}
}

// TestCompute_AddGeneratedColumn_ExpandBackfill_UsesFourStagePipeline
// confirms the large-table (trigger + backfill) path gets the same
// 4-stage pipeline as ADD_COLUMN's volatile-default backfill and
// RENAME_COLUMN — see executeAddGeneratedColumnViaBackfill's phase
// sequence (Preparation -> Syncing -> Validating -> Completed), which
// this must match.
func TestCompute_AddGeneratedColumn_ExpandBackfill_UsesFourStagePipeline(t *testing.T) {
	job := &state.Job{ID: "j11i", Strategy: "EXPAND_BACKFILL", Operation: "ADD_GENERATED_COLUMN", Phase: state.PhaseCompleted}
	report := Compute(job)

	if len(report.Stages) != 4 {
		t.Fatalf("expected the 4-stage EXPAND_BACKFILL pipeline, got %d stages: %+v", len(report.Stages), report.Stages)
	}
}

// TestRender_AddGeneratedColumn_Direct_MentionsExpressionAndNativeGenerated
// is the direct regression test for the summary correctly describing
// the small-table path as a REAL, native generated column.
func TestRender_AddGeneratedColumn_Direct_MentionsExpressionAndNativeGenerated(t *testing.T) {
	job := &state.Job{
		ID: "j11j", Strategy: "DIRECT_DDL", Operation: "ADD_GENERATED_COLUMN", Phase: state.PhaseCompleted,
		SchemaName: "public", TableName: "orders", ColumnName: "total", ColumnType: "numeric",
		GeneratedExpression: "price * quantity",
	}
	report := Compute(job)

	if !strings.Contains(report.OperationSummary, "price * quantity") {
		t.Errorf("expected the summary to mention the expression, got %q", report.OperationSummary)
	}
	if !strings.Contains(report.OperationSummary, "generated column") {
		t.Errorf("expected the summary to describe this as a generated column, got %q", report.OperationSummary)
	}
}

// TestRender_AddGeneratedColumn_ExpandBackfill_MentionsTriggerNotNative is
// the direct regression test for the summary being HONEST about the
// large-table path's real trade-off — it must NOT claim this is a
// native generated column, since it genuinely isn't one (see
// executeAddGeneratedColumn's own doc comment for exactly what differs).
// Misleadingly calling this "generated" the same way the DIRECT_DDL path
// is described would misrepresent a real behavioral difference (this
// path silently recomputes on explicit writes rather than rejecting
// them).
func TestRender_AddGeneratedColumn_ExpandBackfill_MentionsTriggerNotNative(t *testing.T) {
	job := &state.Job{
		ID: "j11k", Strategy: "EXPAND_BACKFILL", Operation: "ADD_GENERATED_COLUMN", Phase: state.PhaseCompleted,
		SchemaName: "public", TableName: "orders", ColumnName: "total", ColumnType: "numeric",
		GeneratedExpression: "price * quantity",
	}
	report := Compute(job)

	if !strings.Contains(report.OperationSummary, "trigger") {
		t.Errorf("expected the summary to mention the trigger mechanism, got %q", report.OperationSummary)
	}
	if !strings.Contains(report.OperationSummary, "not a native generated column") {
		t.Errorf("expected the summary to explicitly disclaim being a native generated column, got %q", report.OperationSummary)
	}
}

// TestRender_Aborted_DoesNotClaimOrphanCleanupSpecifically is a
// regression test for the second bug found alongside the pipeline one: a
// successful, deliberate user Rollback() and internal/engines/postgresql/reaper's orphan
// cleanup both set Phase=ABORTED with no distinguishing signal in the
// data model, but Render() unconditionally said "cleaned up as an
// orphaned job" — actively misleading for a normal, successful rollback
// the user themselves requested.
func TestRender_Aborted_DoesNotClaimOrphanCleanupSpecifically(t *testing.T) {
	job := &state.Job{ID: "j12", Strategy: "DIRECT_DDL", Operation: "DROP_COLUMN", Phase: state.PhaseAborted}
	output := Compute(job).Render()

	if strings.Contains(output, "ABORTED (cleaned up as an orphaned job)") {
		t.Error("Render must not claim a job was specifically orphan-cleaned when it could equally have been a deliberate user rollback")
	}
	if !strings.Contains(output, "ABORTED") {
		t.Error("expected the output to still clearly state ABORTED")
	}
}

// TestCompute_CopiesTimestampsAndRowCounts verifies Compute copies
// CreatedAt/UpdatedAt/EstimatedRowCount/RowsProcessed straight from the
// job — added alongside the Migration Detail page's new "started,
// finished, duration, rows processed" display.
func TestCompute_CopiesTimestampsAndRowCounts(t *testing.T) {
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 8, 20, 10, 5, 30, 0, time.UTC)
	job := &state.Job{
		ID: "j13", Strategy: "DIRECT_DDL", Phase: state.PhasePreparation,
		CreatedAt: created, UpdatedAt: updated,
		EstimatedRowCount: 5000, RowsProcessed: 1200,
	}
	report := Compute(job)

	if !report.CreatedAt.Equal(created) {
		t.Errorf("expected CreatedAt=%v, got %v", created, report.CreatedAt)
	}
	if !report.UpdatedAt.Equal(updated) {
		t.Errorf("expected UpdatedAt=%v, got %v", updated, report.UpdatedAt)
	}
	if report.EstimatedRowCount != 5000 {
		t.Errorf("expected EstimatedRowCount=5000, got %d", report.EstimatedRowCount)
	}
	if report.RowsProcessed != 1200 {
		t.Errorf("expected RowsProcessed=1200, got %d", report.RowsProcessed)
	}
}

// TestRender_TerminalJob_ShowsStartFinishDuration verifies the CLI output
// includes the started/finished/duration lines for a completed job, using
// exact fixed timestamps so the rendered duration is deterministic.
func TestRender_TerminalJob_ShowsStartFinishDuration(t *testing.T) {
	created := time.Date(2026, 8, 20, 10, 0, 0, 0, time.UTC)
	updated := time.Date(2026, 8, 20, 10, 5, 30, 0, time.UTC) // 5m30s later
	job := &state.Job{
		ID: "j14", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted,
		CreatedAt: created, UpdatedAt: updated,
	}
	output := Compute(job).Render()

	if !strings.Contains(output, "Started: 2026-08-20T10:00:00Z") {
		t.Errorf("expected a Started line with the exact timestamp, got: %s", output)
	}
	if !strings.Contains(output, "Finished: 2026-08-20T10:05:30Z") {
		t.Errorf("expected a Finished line with the exact timestamp, got: %s", output)
	}
	if !strings.Contains(output, "duration: 5m30s") {
		t.Errorf("expected the duration to be computed as 5m30s, got: %s", output)
	}
}

// TestRender_InProgressJob_ShowsElapsedNotFinished verifies a
// still-running job shows "Elapsed" rather than a "Finished" line it
// can't honestly provide yet.
func TestRender_InProgressJob_ShowsElapsedNotFinished(t *testing.T) {
	job := &state.Job{
		ID: "j15", Strategy: "DIRECT_DDL", Phase: state.PhasePreparation,
		CreatedAt: time.Now().Add(-2 * time.Minute), UpdatedAt: time.Now(),
	}
	output := Compute(job).Render()

	if !strings.Contains(output, "Elapsed:") {
		t.Errorf("expected an Elapsed line for a non-terminal job, got: %s", output)
	}
	if strings.Contains(output, "Finished:") {
		t.Errorf("did not expect a Finished line for a job that hasn't finished, got: %s", output)
	}
}

// TestRender_ZeroCreatedAt_OmitsTimingLines verifies older/test job
// literals that never set CreatedAt (a zero time.Time) don't render a
// nonsensical "Started: 0001-01-01T00:00:00Z" line — this also protects
// every pre-existing Render() test in this file that doesn't set
// timestamps from silently gaining unexpected new output lines.
func TestRender_ZeroCreatedAt_OmitsTimingLines(t *testing.T) {
	job := &state.Job{ID: "j16", Strategy: "DIRECT_DDL", Phase: state.PhasePreparation}
	output := Compute(job).Render()

	if strings.Contains(output, "Started:") || strings.Contains(output, "Elapsed:") {
		t.Errorf("expected no timing lines when CreatedAt is zero, got: %s", output)
	}
}

// TestRender_RowsProcessed_ShownWithAndWithoutEstimate covers both the
// "we know the estimated total" and "estimate unknown" phrasings.
func TestRender_RowsProcessed_ShownWithAndWithoutEstimate(t *testing.T) {
	withEstimate := &state.Job{
		ID: "j17", Strategy: "EXPAND_BACKFILL", Phase: state.PhaseSyncing,
		RowsProcessed: 300, EstimatedRowCount: 1000,
	}
	out := Compute(withEstimate).Render()
	if !strings.Contains(out, "Rows processed: 300 (~1000 estimated)") {
		t.Errorf("expected a rows-processed line with the estimate, got: %s", out)
	}

	withoutEstimate := &state.Job{
		ID: "j18", Strategy: "EXPAND_BACKFILL", Phase: state.PhaseSyncing,
		RowsProcessed: 300,
	}
	out2 := Compute(withoutEstimate).Render()
	if !strings.Contains(out2, "Rows processed: 300\n") {
		t.Errorf("expected a rows-processed line without an estimate, got: %s", out2)
	}
	if strings.Contains(out2, "estimated") {
		t.Errorf("did not expect an estimate mention when EstimatedRowCount is 0, got: %s", out2)
	}
}

// TestCompute_OperationSummaryAndStatements covers all 8 operation types —
// added alongside the Migration Detail page's new "what happened" panel.
// Each case checks the summary mentions the key parameters and, where
// applicable, that Statements reconstructs the real DDL ddlflow issues
// (matching internal/engines/postgresql/ddlflow's own quoteIdent-quoted identifier style).
func TestCompute_OperationSummaryAndStatements(t *testing.T) {
	t.Run("ADD_COLUMN with fixed default", func(t *testing.T) {
		job := &state.Job{
			ID: "j20", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "ADD_COLUMN", ColumnName: "status", ColumnType: "text", DefaultValue: "'active'",
		}
		report := Compute(job)
		if !strings.Contains(report.OperationSummary, `"status"`) || !strings.Contains(report.OperationSummary, "text") {
			t.Errorf("expected summary to mention column name and type, got: %s", report.OperationSummary)
		}
		want := `ALTER TABLE "public"."orders" ADD COLUMN "status" text DEFAULT 'active'`
		if len(report.Statements) != 1 || report.Statements[0] != want {
			t.Errorf("expected statement %q, got %v", want, report.Statements)
		}
	})

	t.Run("ADD_COLUMN volatile default mentions batched backfill", func(t *testing.T) {
		job := &state.Job{
			ID: "j21", Strategy: "EXPAND_BACKFILL", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "ADD_COLUMN", ColumnName: "created_ts", ColumnType: "timestamptz",
			DefaultValue: "now()", IsVolatileDefault: true,
		}
		report := Compute(job)
		if !strings.Contains(report.OperationSummary, "backfilled in batches") {
			t.Errorf("expected the volatile-default backfill note, got: %s", report.OperationSummary)
		}
	})

	t.Run("DROP_COLUMN before finalization shows the soft-drop rename", func(t *testing.T) {
		job := &state.Job{
			ID: "j22", Strategy: "DIRECT_DDL", Phase: state.PhaseRollbackWindow,
			SchemaName: "public", TableName: "orders",
			Operation: "DROP_COLUMN", ColumnName: "legacy_note", DeprecatedColumnName: "__pgam_dropped_legacy_note_j22",
		}
		report := Compute(job)
		want := `ALTER TABLE "public"."orders" RENAME COLUMN "legacy_note" TO "__pgam_dropped_legacy_note_j22"`
		if len(report.Statements) != 1 || report.Statements[0] != want {
			t.Errorf("expected statement %q, got %v", want, report.Statements)
		}
		if !strings.Contains(report.OperationSummary, "two-phase") {
			t.Errorf("expected the two-phase soft-drop explanation, got: %s", report.OperationSummary)
		}
	})

	t.Run("ALTER_COLUMN_TYPE direct DDL reconstructs the ALTER statement", func(t *testing.T) {
		job := &state.Job{
			ID: "j23", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "ALTER_COLUMN_TYPE", ColumnName: "amount", ColumnType: "numeric(12,2)",
		}
		report := Compute(job)
		want := `ALTER TABLE "public"."orders" ALTER COLUMN "amount" TYPE numeric(12,2) USING "amount"::numeric(12,2)`
		if len(report.Statements) != 1 || report.Statements[0] != want {
			t.Errorf("expected statement %q, got %v", want, report.Statements)
		}
	})

	t.Run("ALTER_COLUMN_TYPE via SHADOW_TABLE has no statements, only prose", func(t *testing.T) {
		job := &state.Job{
			ID: "j24", Strategy: "SHADOW_TABLE", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "ALTER_COLUMN_TYPE", ColumnName: "amount", ColumnType: "text",
		}
		report := Compute(job)
		if len(report.Statements) != 0 {
			t.Errorf("expected no statements for a SHADOW_TABLE type change, got %v", report.Statements)
		}
		if !strings.Contains(report.OperationSummary, "shadow table") {
			t.Errorf("expected the summary to explain the shadow-table mechanism, got: %s", report.OperationSummary)
		}
	})

	t.Run("ADD_INDEX", func(t *testing.T) {
		job := &state.Job{
			ID: "j25", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "ADD_INDEX", ColumnName: "customer_id", IndexName: "idx_orders_customer_id",
		}
		report := Compute(job)
		want := `CREATE INDEX CONCURRENTLY IF NOT EXISTS "idx_orders_customer_id" ON "public"."orders" ("customer_id")`
		if len(report.Statements) != 1 || report.Statements[0] != want {
			t.Errorf("expected statement %q, got %v", want, report.Statements)
		}
	})

	t.Run("DROP_INDEX", func(t *testing.T) {
		job := &state.Job{
			ID: "j26", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "DROP_INDEX", IndexName: "idx_orders_old",
		}
		report := Compute(job)
		want := `DROP INDEX CONCURRENTLY IF EXISTS "public"."idx_orders_old"`
		if len(report.Statements) != 1 || report.Statements[0] != want {
			t.Errorf("expected statement %q, got %v", want, report.Statements)
		}
	})

	t.Run("SET_NOT_NULL produces the 4-statement NOT VALID sequence", func(t *testing.T) {
		job := &state.Job{
			ID: "j27", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "SET_NOT_NULL", ColumnName: "email", ConstraintName: "orders_email_not_null_check",
		}
		report := Compute(job)
		if len(report.Statements) != 4 {
			t.Fatalf("expected 4 statements, got %d: %v", len(report.Statements), report.Statements)
		}
		if !strings.Contains(report.Statements[0], "NOT VALID") {
			t.Errorf("expected statement 0 to add the constraint NOT VALID, got: %s", report.Statements[0])
		}
		if !strings.Contains(report.Statements[2], "SET NOT NULL") {
			t.Errorf("expected statement 2 to SET NOT NULL, got: %s", report.Statements[2])
		}
	})

	t.Run("ADD_CONSTRAINT includes the check expression verbatim", func(t *testing.T) {
		job := &state.Job{
			ID: "j28", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "ADD_CONSTRAINT", ConstraintName: "price_positive", CheckExpression: "price > 0",
		}
		report := Compute(job)
		if len(report.Statements) != 2 {
			t.Fatalf("expected 2 statements, got %d: %v", len(report.Statements), report.Statements)
		}
		if !strings.Contains(report.Statements[0], "price > 0") {
			t.Errorf("expected the check expression in statement 0, got: %s", report.Statements[0])
		}
	})

	t.Run("RENAME_COLUMN explains the dual-write mechanism", func(t *testing.T) {
		job := &state.Job{
			ID: "j29", Strategy: "EXPAND_BACKFILL", Phase: state.PhaseCompleted,
			SchemaName: "public", TableName: "orders",
			Operation: "RENAME_COLUMN", ColumnName: "old_name", NewColumnName: "new_name",
		}
		report := Compute(job)
		if !strings.Contains(report.OperationSummary, `"old_name"`) || !strings.Contains(report.OperationSummary, `"new_name"`) {
			t.Errorf("expected both column names in the summary, got: %s", report.OperationSummary)
		}
		if !strings.Contains(report.OperationSummary, "dual-write") {
			t.Errorf("expected a dual-write explanation, got: %s", report.OperationSummary)
		}
		if len(report.Statements) == 0 {
			t.Error("expected at least one statement describing the rename mechanism")
		}
	})

	t.Run("Name and Description are copied straight from the job", func(t *testing.T) {
		job := &state.Job{
			ID: "j30", Strategy: "DIRECT_DDL", Phase: state.PhasePreparation,
			Operation: "ADD_COLUMN", ColumnName: "status",
			Name: "Q3 billing update", Description: "Adds the status column ahead of the promo launch",
		}
		report := Compute(job)
		if report.Name != "Q3 billing update" {
			t.Errorf("expected Name to be copied, got %q", report.Name)
		}
		if report.Description != "Adds the status column ahead of the promo launch" {
			t.Errorf("expected Description to be copied, got %q", report.Description)
		}
	})

	t.Run("still populated on a FAILED job (early-return path)", func(t *testing.T) {
		job := &state.Job{
			ID: "j31", Strategy: "DIRECT_DDL", Phase: state.PhaseFailed,
			SchemaName: "public", TableName: "orders",
			Operation: "ADD_COLUMN", ColumnName: "status", ColumnType: "text",
		}
		report := Compute(job)
		if report.OperationSummary == "" {
			t.Error("expected OperationSummary to still be populated on a FAILED job, not cleared by the early-return path")
		}
	})
}

// TestCompute_StatementsIsNeverNil is a direct regression guard for a
// real, user-reported production crash: describeOperation's
// SHADOW_TABLE-ALTER_COLUMN_TYPE and default/unknown-operation branches
// used to `return summary, nil` — a Go nil slice, which
// encoding/json marshals as JSON `null`. The frontend does
// `job.Statements.length` unconditionally, and `null.length` crashed the
// entire Migration Detail screen. This is the exact same bug class
// already found and fixed once in internal/engines/postgresql/preview.Generate — this test
// exists specifically because that earlier fix did NOT prevent the same
// mistake from being made again in a different function, and checks both
// the Go-level nil and the real JSON encoding, matching
// TestGenerate_NeverReturnsNilSlices' approach in internal/engines/postgresql/preview.
func TestCompute_StatementsIsNeverNil(t *testing.T) {
	cases := []struct {
		name string
		job  *state.Job
	}{
		{
			name: "ALTER_COLUMN_TYPE via SHADOW_TABLE (the exact case that crashed)",
			job: &state.Job{
				ID: "j32", Strategy: "SHADOW_TABLE", Phase: state.PhaseCompleted,
				SchemaName: "public", TableName: "orders",
				Operation: "ALTER_COLUMN_TYPE", ColumnName: "amount", ColumnType: "text",
			},
		},
		{
			name: "DROP_COLUMN before the deprecated name is resolved",
			job: &state.Job{
				ID: "j33", Strategy: "DIRECT_DDL", Phase: state.PhasePreparation,
				SchemaName: "public", TableName: "orders",
				Operation: "DROP_COLUMN", ColumnName: "legacy_note", DeprecatedColumnName: "",
			},
		},
		{
			name: "an unrecognized/future operation (default branch)",
			job: &state.Job{
				ID: "j34", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted,
				SchemaName: "public", TableName: "orders",
				Operation: "SOME_FUTURE_OPERATION",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := Compute(tc.job)
			if report.Statements == nil {
				t.Fatal("expected Statements to be a non-nil (possibly empty) slice")
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatalf("failed to marshal report: %v", err)
			}
			if strings.Contains(string(encoded), `"Statements":null`) {
				t.Error("Statements serialized as JSON null — this is exactly what crashed the frontend (null.length is not a function)")
			}
		})
	}
}

func TestRender_PartitionTable_MentionsStrategyColumnAndCount(t *testing.T) {
	job := &state.Job{
		ID: "j-part-1", Strategy: "SHADOW_TABLE", Operation: "PARTITION_TABLE", Phase: state.PhaseCompleted,
		SchemaName: "public", TableName: "orders",
		PartitionColumn: "created_at", PartitionStrategy: "RANGE",
		PartitionBoundsJSON: `[{"name":"orders_2024_01","from":"2024-01-01","to":"2024-02-01"},{"name":"orders_2024_02","from":"2024-02-01","to":"2024-03-01"}]`,
	}
	report := Compute(job)

	for _, want := range []string{"RANGE", "created_at", "2 partitions"} {
		if !strings.Contains(report.OperationSummary, want) {
			t.Errorf("expected the summary to mention %q, got %q", want, report.OperationSummary)
		}
	}
}

// TestRender_PartitionTable_SingularWhenOnePartition is the direct
// regression test for correct pluralization — "1 partitions" would be
// an obvious, avoidable grammar mistake in a summary a reviewer reads
// directly.
func TestRender_PartitionTable_SingularWhenOnePartition(t *testing.T) {
	job := &state.Job{
		ID: "j-part-2", Strategy: "SHADOW_TABLE", Operation: "PARTITION_TABLE", Phase: state.PhaseCompleted,
		SchemaName: "public", TableName: "orders",
		PartitionColumn: "region", PartitionStrategy: "LIST",
		PartitionBoundsJSON: `[{"name":"orders_eu","values":["DE","FR"]}]`,
	}
	report := Compute(job)

	if !strings.Contains(report.OperationSummary, "1 partition ") {
		t.Errorf("expected singular '1 partition', got %q", report.OperationSummary)
	}
	if strings.Contains(report.OperationSummary, "1 partitions") {
		t.Errorf("expected singular 'partition', not 'partitions', got %q", report.OperationSummary)
	}
}

func TestRender_PartitionTable_MentionsDefaultPartitionWhenIncluded(t *testing.T) {
	job := &state.Job{
		ID: "j-part-3", Strategy: "SHADOW_TABLE", Operation: "PARTITION_TABLE", Phase: state.PhaseCompleted,
		SchemaName: "public", TableName: "orders",
		PartitionColumn: "region", PartitionStrategy: "LIST",
		PartitionBoundsJSON:     `[{"name":"orders_eu","values":["DE","FR"]}]`,
		PartitionIncludeDefault: true,
	}
	report := Compute(job)

	if !strings.Contains(report.OperationSummary, "DEFAULT partition") {
		t.Errorf("expected the summary to mention the DEFAULT partition, got %q", report.OperationSummary)
	}
}

// TestCompute_PartitionTable_UsesNineStageShadowTablePipeline confirms
// PARTITION_TABLE reuses the standard, unmodified 9-stage SHADOW_TABLE
// pipeline — see internal/engines/postgresql/shadowflow.prepare's own doc comment for why
// only the initial CREATE TABLE step differs; every phase transition
// after that (Syncing, DeltaSync, Validating, Swapping, RollbackWindow,
// Cleanup) is completely unchanged.
func TestCompute_PartitionTable_UsesNineStageShadowTablePipeline(t *testing.T) {
	job := &state.Job{ID: "j-part-4", Strategy: "SHADOW_TABLE", Operation: "PARTITION_TABLE", Phase: state.PhaseCompleted}
	report := Compute(job)

	if len(report.Stages) != 9 {
		t.Fatalf("expected the standard 9-stage SHADOW_TABLE pipeline, got %d stages: %+v", len(report.Stages), report.Stages)
	}
}
