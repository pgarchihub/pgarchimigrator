//go:build integration

// Run with:
//
//	docker compose -f deploy/docker-compose.dev.yml up -d
//	go test ./internal/api/... -tags=integration -v -run ImpactMeasurement
package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/progress"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// TestHandleGetMigration_ImpactMeasurement_TerminalJob_ShowsPersistedPeakUnconditionally
// is the direct regression test for Faz D's "automatic post-migration
// impact report": a TERMINAL job's already-persisted peak (see
// state.Job.ImpactPeakQueryDurationSeconds's own doc comment) must show
// up WITHOUT needing measureImpact=true on this specific request — see
// attachImpactMeasurement's own doc comment for why (reading an
// already-fetched job field costs nothing extra, unlike the live,
// still-running path's genuinely non-trivial per-poll query).
func TestHandleGetMigration_ImpactMeasurement_TerminalJob_ShowsPersistedPeakUnconditionally(t *testing.T) {
	pool := connectPoolForResourceStatus(t)
	store := newFakeStore()
	ctx := context.Background()

	peak := 2.35
	job := &state.Job{
		ID: "impact-terminal-job-1", SchemaName: "public", TableName: "orders",
		Strategy: "SHADOW_TABLE", Phase: state.PhaseCompleted,
		ImpactPeakQueryDurationSeconds: &peak,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job in fake store: %v", err)
	}

	srv, users := newTestServerWithRealPool(t, pool, store)
	// Deliberately NOT passing ?measureImpact=true — the whole point of
	// this test is that the terminal path doesn't need it.
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/"+job.ID, nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var report progress.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}
	if report.ImpactPeakQueryDurationSeconds == nil || *report.ImpactPeakQueryDurationSeconds != 2.35 {
		t.Errorf("expected the persisted peak (2.35) to appear unconditionally for a terminal job, got %v", report.ImpactPeakQueryDurationSeconds)
	}
}

// TestHandleGetMigration_ImpactMeasurement_TerminalJob_NeverMeasured_ShowsNothing
// is the direct regression test for the "nil means never measured, not
// zero" contract — a terminal job that never had impact measurement
// turned on while running must not show a misleading 0.
func TestHandleGetMigration_ImpactMeasurement_TerminalJob_NeverMeasured_ShowsNothing(t *testing.T) {
	pool := connectPoolForResourceStatus(t)
	store := newFakeStore()
	ctx := context.Background()

	job := &state.Job{
		ID: "impact-terminal-job-2", SchemaName: "public", TableName: "orders",
		Strategy: "SHADOW_TABLE", Phase: state.PhaseCompleted,
		// ImpactPeakQueryDurationSeconds deliberately left nil.
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job in fake store: %v", err)
	}

	srv, users := newTestServerWithRealPool(t, pool, store)
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/"+job.ID, nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var report progress.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}
	if report.ImpactPeakQueryDurationSeconds != nil {
		t.Errorf("expected no impact peak for a job that never had measurement turned on, got %v", *report.ImpactPeakQueryDurationSeconds)
	}
}

// TestHandleGetMigration_ImpactMeasurement_RunningJob_WriteThroughPersistsToStore
// is the direct regression test for the write-through design: a LIVE
// poll (measureImpact=true, job still running) must persist its reading
// to the durable store on every call, not just remember it in the
// in-memory tracker — see impactTracker's own doc comment for why
// (catching the exact terminal-transition moment isn't reliable, so
// continuous persistence means the last successfully-written value is
// already correct, or very close, regardless of exact timing).
func TestHandleGetMigration_ImpactMeasurement_RunningJob_WriteThroughPersistsToStore(t *testing.T) {
	pool := connectPoolForResourceStatus(t)
	store := newFakeStore()
	ctx := context.Background()

	job := &state.Job{
		ID: "impact-running-job-1", SchemaName: "public", TableName: "orders",
		Strategy: "SHADOW_TABLE", Phase: state.PhaseSyncing, // not terminal
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job in fake store: %v", err)
	}

	srv, users := newTestServerWithRealPool(t, pool, store)
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/"+job.ID+"?measureImpact=true", nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The fake store's job record should now have SOME peak persisted
	// (even 0.0 for "no active queries right now" is a real, valid,
	// non-nil measurement — the point being it's no longer nil).
	stored, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("could not read back the job: %v", err)
	}
	if stored.ImpactPeakQueryDurationSeconds == nil {
		t.Error("expected the live poll to have persisted SOME peak value (even 0.0) via write-through, got nil")
	}
}

// TestHandleGetMigration_ImpactMeasurement_RunningJob_WithoutMeasureFlag_DoesNothing
// confirms the live path's opt-in gate still holds — a running job
// polled WITHOUT measureImpact=true must not run the live query or
// persist anything.
func TestHandleGetMigration_ImpactMeasurement_RunningJob_WithoutMeasureFlag_DoesNothing(t *testing.T) {
	pool := connectPoolForResourceStatus(t)
	store := newFakeStore()
	ctx := context.Background()

	job := &state.Job{
		ID: "impact-running-job-2", SchemaName: "public", TableName: "orders",
		Strategy: "SHADOW_TABLE", Phase: state.PhaseSyncing,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("could not create job in fake store: %v", err)
	}

	srv, users := newTestServerWithRealPool(t, pool, store)
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/"+job.ID, nil, users.viewer) // no ?measureImpact=true
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var report progress.Report
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}
	if report.ImpactPeakQueryDurationSeconds != nil || report.ImpactActiveQueries != nil {
		t.Errorf("expected no impact data without the opt-in flag, got peak=%v activeQueries=%v",
			report.ImpactPeakQueryDurationSeconds, report.ImpactActiveQueries)
	}

	stored, err := store.Get(ctx, job.ID)
	if err != nil {
		t.Fatalf("could not read back the job: %v", err)
	}
	if stored.ImpactPeakQueryDurationSeconds != nil {
		t.Error("expected nothing to have been persisted without the opt-in flag")
	}
}
