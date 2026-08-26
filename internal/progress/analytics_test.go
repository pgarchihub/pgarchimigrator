package progress

import (
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

func job(strategy string, phase state.Phase, durationMin float64) *state.Job {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &state.Job{
		Strategy:  strategy,
		Phase:     phase,
		CreatedAt: created,
		UpdatedAt: created.Add(time.Duration(durationMin * float64(time.Minute))),
	}
}

func TestComputeAnalytics_EmptyJobList_ReturnsZeroesNotNaN(t *testing.T) {
	a := ComputeAnalytics(nil)
	if a.TotalMigrations != 0 || a.TerminalMigrations != 0 {
		t.Errorf("expected zero counts, got %+v", a)
	}
	if a.FailureRate != 0 || a.AverageDurationMs != 0 {
		t.Errorf("expected FailureRate/AverageDurationMs to be 0, not NaN, got %+v", a)
	}
}

func TestComputeAnalytics_OnlyNonTerminalJobs_ExcludedFromAllStats(t *testing.T) {
	jobs := []*state.Job{
		job("DIRECT_DDL", state.PhaseSyncing, 5),
		job("DIRECT_DDL", state.PhaseSyncing, 3),
	}
	a := ComputeAnalytics(jobs)
	if a.TotalMigrations != 2 {
		t.Errorf("expected TotalMigrations=2, got %d", a.TotalMigrations)
	}
	if a.TerminalMigrations != 0 {
		t.Errorf("expected TerminalMigrations=0 (neither job is terminal), got %d", a.TerminalMigrations)
	}
	if a.FailureRate != 0 || a.AverageDurationMs != 0 {
		t.Errorf("expected zero rates for an all-non-terminal fleet, got %+v", a)
	}
}

func TestComputeAnalytics_ComputesOverallFailureRate(t *testing.T) {
	jobs := []*state.Job{
		job("DIRECT_DDL", state.PhaseCompleted, 1),
		job("DIRECT_DDL", state.PhaseCompleted, 1),
		job("DIRECT_DDL", state.PhaseCompleted, 1),
		job("SHADOW_TABLE", state.PhaseFailed, 1),
	}
	a := ComputeAnalytics(jobs)
	if a.TerminalMigrations != 4 {
		t.Fatalf("expected 4 terminal migrations, got %d", a.TerminalMigrations)
	}
	want := 0.25 // 1 failed out of 4
	if a.FailureRate != want {
		t.Errorf("expected FailureRate=%v, got %v", want, a.FailureRate)
	}
}

func TestComputeAnalytics_TreatsAbortedAsTerminalButNotFailed(t *testing.T) {
	// ABORTED (a deliberate rollback or reaper cleanup) is a distinct
	// outcome from FAILED (an actual error) — see internal/progress's
	// own Render() distinction for the same reasoning applied
	// elsewhere. Analytics' FailureRate should only count genuine
	// FAILED jobs, not every non-COMPLETED terminal outcome.
	jobs := []*state.Job{
		job("DIRECT_DDL", state.PhaseCompleted, 1),
		job("DIRECT_DDL", state.PhaseAborted, 1),
	}
	a := ComputeAnalytics(jobs)
	if a.TerminalMigrations != 2 {
		t.Fatalf("expected ABORTED to count as terminal, got %d terminal migrations", a.TerminalMigrations)
	}
	if a.FailureRate != 0 {
		t.Errorf("expected FailureRate=0 (ABORTED is not FAILED), got %v", a.FailureRate)
	}
}

func TestComputeAnalytics_ComputesAverageDurationInMilliseconds(t *testing.T) {
	jobs := []*state.Job{
		job("DIRECT_DDL", state.PhaseCompleted, 2), // 2 minutes
		job("DIRECT_DDL", state.PhaseCompleted, 4), // 4 minutes
	}
	a := ComputeAnalytics(jobs)
	want := 3.0 * 60 * 1000 // average of 2 and 4 minutes = 3 minutes, in ms
	if a.AverageDurationMs != want {
		t.Errorf("expected AverageDurationMs=%v, got %v", want, a.AverageDurationMs)
	}
}

// TestComputeAnalytics_StrategyBreakdown_IsPerStrategyNotJustOverall is
// the direct regression test for why a per-strategy breakdown exists at
// all: a fleet dominated by many fast DIRECT_DDL jobs would otherwise
// make one genuinely slow SHADOW_TABLE migration invisible in a single
// fleet-wide average.
func TestComputeAnalytics_StrategyBreakdown_IsPerStrategyNotJustOverall(t *testing.T) {
	jobs := []*state.Job{
		job("DIRECT_DDL", state.PhaseCompleted, 0.1),
		job("DIRECT_DDL", state.PhaseCompleted, 0.1),
		job("DIRECT_DDL", state.PhaseCompleted, 0.1),
		job("SHADOW_TABLE", state.PhaseCompleted, 45),
	}
	a := ComputeAnalytics(jobs)

	ddl := a.StrategyBreakdown["DIRECT_DDL"]
	if ddl == nil || ddl.Count != 3 {
		t.Fatalf("expected DIRECT_DDL count=3, got %+v", ddl)
	}
	shadow := a.StrategyBreakdown["SHADOW_TABLE"]
	if shadow == nil || shadow.Count != 1 {
		t.Fatalf("expected SHADOW_TABLE count=1, got %+v", shadow)
	}
	// The overall average (dominated by 3 fast jobs) must NOT equal the
	// SHADOW_TABLE-specific average (which reflects its own genuinely
	// slow 45-minute run) — this is the whole point of the breakdown.
	if a.AverageDurationMs == shadow.AverageDurationMs {
		t.Error("expected the overall average and SHADOW_TABLE's own average to differ — the breakdown exists precisely because they can")
	}
	wantShadowMs := 45.0 * 60 * 1000
	if shadow.AverageDurationMs != wantShadowMs {
		t.Errorf("expected SHADOW_TABLE's own average to be exactly its single 45-minute run (%v), got %v", wantShadowMs, shadow.AverageDurationMs)
	}
}

func TestComputeAnalytics_PerStrategyFailureRate(t *testing.T) {
	jobs := []*state.Job{
		job("SHADOW_TABLE", state.PhaseCompleted, 1),
		job("SHADOW_TABLE", state.PhaseFailed, 1),
		job("DIRECT_DDL", state.PhaseCompleted, 1),
	}
	a := ComputeAnalytics(jobs)
	shadow := a.StrategyBreakdown["SHADOW_TABLE"]
	if shadow.FailureRate != 0.5 {
		t.Errorf("expected SHADOW_TABLE FailureRate=0.5, got %v", shadow.FailureRate)
	}
	ddl := a.StrategyBreakdown["DIRECT_DDL"]
	if ddl.FailureRate != 0 {
		t.Errorf("expected DIRECT_DDL FailureRate=0, got %v", ddl.FailureRate)
	}
}

func TestComputeAnalytics_NegativeDuration_ClampedToZero(t *testing.T) {
	// Should never legitimately happen, but a malformed record with
	// UpdatedAt before CreatedAt must not skew the fleet average with a
	// negative number.
	j := job("DIRECT_DDL", state.PhaseCompleted, -5)
	a := ComputeAnalytics([]*state.Job{j})
	if a.AverageDurationMs != 0 {
		t.Errorf("expected a negative duration to be clamped to 0, got %v", a.AverageDurationMs)
	}
}
