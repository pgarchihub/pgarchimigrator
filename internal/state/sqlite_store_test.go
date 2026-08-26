package state

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("failed to create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestSQLiteStore_CreateAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &Job{
		ID:         "job-1",
		SchemaName: "public",
		TableName:  "orders",
		Strategy:   "SHADOW_TABLE",
		Phase:      PhasePreflight,
	}

	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	got, err := store.Get(ctx, "job-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.TableName != "orders" || got.Phase != PhasePreflight {
		t.Errorf("unexpected job content: %+v", got)
	}
	if got.CreatedAt.IsZero() || got.UpdatedAt.IsZero() {
		t.Error("CreatedAt/UpdatedAt should have been auto-populated")
	}
}

func TestSQLiteStore_Get_NotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.Get(context.Background(), "nonexistent-id")
	if err != ErrJobNotFound {
		t.Errorf("expected ErrJobNotFound, got %v", err)
	}
}

func TestSQLiteStore_UpdatePhase(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &Job{ID: "job-2", SchemaName: "public", TableName: "orders", Strategy: "DIRECT_DDL", Phase: PhasePreparation}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if err := store.UpdatePhase(ctx, "job-2", PhaseSyncing); err != nil {
		t.Fatalf("UpdatePhase failed: %v", err)
	}

	got, err := store.Get(ctx, "job-2")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.Phase != PhaseSyncing {
		t.Errorf("expected Phase=SYNCING, got %s", got.Phase)
	}
}

func TestSQLiteStore_UpdatePhase_NotFound(t *testing.T) {
	store := newTestStore(t)
	err := store.UpdatePhase(context.Background(), "nonexistent-id", PhaseSyncing)
	if err != ErrJobNotFound {
		t.Errorf("expected ErrJobNotFound, got %v", err)
	}
}

// ListStale is the query the Architecture Doc Section 3.3 Orphan Resource
// Reaper relies on.
func TestSQLiteStore_ListStale(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	// An old job still considered IN_PROGRESS (SYNCING) -> should be returned as stale.
	staleJob := &Job{ID: "stale-1", SchemaName: "public", TableName: "orders", Strategy: "SHADOW_TABLE", Phase: PhaseSyncing}
	if err := store.Create(ctx, staleJob); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	// Push updated_at into the past directly via SQL (shortcut instead of
	// waiting 30 real minutes in the test).
	oldTimestamp := time.Now().UTC().Add(-1 * time.Hour).Format(timeLayout)
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, oldTimestamp, "stale-1"); err != nil {
		t.Fatalf("failed to manipulate updated_at: %v", err)
	}

	// A completed job -> should not be considered stale, it's terminal.
	completedJob := &Job{ID: "completed-1", SchemaName: "public", TableName: "orders", Strategy: "DIRECT_DDL", Phase: PhaseCompleted}
	if err := store.Create(ctx, completedJob); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE jobs SET updated_at = ? WHERE id = ?`, oldTimestamp, "completed-1"); err != nil {
		t.Fatalf("failed to manipulate updated_at: %v", err)
	}

	// A fresh, still-running job -> should not be considered stale (threshold not yet passed).
	freshJob := &Job{ID: "fresh-1", SchemaName: "public", TableName: "orders", Strategy: "SHADOW_TABLE", Phase: PhaseSyncing}
	if err := store.Create(ctx, freshJob); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	stale, err := store.ListStale(ctx, 30*time.Minute)
	if err != nil {
		t.Fatalf("ListStale failed: %v", err)
	}
	if len(stale) != 1 {
		t.Fatalf("expected 1 stale job, got %d", len(stale))
	}
	if stale[0].ID != "stale-1" {
		t.Errorf("expected stale-1, got %s", stale[0].ID)
	}
}

func TestSQLiteStore_RollbackDeadline_RoundTrip(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	deadline := time.Now().UTC().Add(10 * time.Minute)
	job := &Job{
		ID: "job-3", SchemaName: "public", TableName: "orders",
		Strategy: "SHADOW_TABLE", Phase: PhaseRollbackWindow,
		RollbackDeadline: &deadline,
	}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	got, err := store.Get(ctx, "job-3")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.RollbackDeadline == nil {
		t.Fatal("RollbackDeadline should not be nil")
	}
	if !got.RollbackDeadline.Equal(deadline) {
		t.Errorf("RollbackDeadline mismatch: expected %v, got %v", deadline, *got.RollbackDeadline)
	}
}

func TestSQLiteStore_UpdateResources(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &Job{ID: "job-4", SchemaName: "public", TableName: "orders", Strategy: "SHADOW_TABLE", Phase: PhasePreparation}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if err := store.UpdateResources(ctx, "job-4", "pgam_slot_abc", "__pgam_shadow_orders_abc"); err != nil {
		t.Fatalf("UpdateResources failed: %v", err)
	}

	got, err := store.Get(ctx, "job-4")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.ReplicationSlotName != "pgam_slot_abc" {
		t.Errorf("expected ReplicationSlotName='pgam_slot_abc', got %q", got.ReplicationSlotName)
	}
	if got.ShadowTableName != "__pgam_shadow_orders_abc" {
		t.Errorf("expected ShadowTableName='__pgam_shadow_orders_abc', got %q", got.ShadowTableName)
	}
}

func TestSQLiteStore_UpdateResources_NotFound(t *testing.T) {
	store := newTestStore(t)
	err := store.UpdateResources(context.Background(), "nonexistent-id", "slot", "table")
	if err != ErrJobNotFound {
		t.Errorf("expected ErrJobNotFound, got %v", err)
	}
}

func TestSQLiteStore_UpdateRollbackDeadline(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &Job{ID: "job-5", SchemaName: "public", TableName: "orders", Strategy: "SHADOW_TABLE", Phase: PhaseSwapping}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	deadline := time.Now().UTC().Add(10 * time.Minute)
	if err := store.UpdateRollbackDeadline(ctx, "job-5", deadline); err != nil {
		t.Fatalf("UpdateRollbackDeadline failed: %v", err)
	}

	got, err := store.Get(ctx, "job-5")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.RollbackDeadline == nil || !got.RollbackDeadline.Equal(deadline) {
		t.Errorf("expected RollbackDeadline=%v, got %v", deadline, got.RollbackDeadline)
	}
}

func TestSQLiteStore_UpdateImpactPeak_PersistsAndIsReadableViaGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &Job{ID: "job-impact-1", SchemaName: "public", TableName: "orders", Strategy: "SHADOW_TABLE", Phase: PhaseSyncing}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// A freshly created job must have a nil peak — "never measured", not
	// "measured, found to be zero" (see Job.ImpactPeakQueryDurationSeconds's
	// own doc comment).
	got, err := store.Get(ctx, "job-impact-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.ImpactPeakQueryDurationSeconds != nil {
		t.Fatalf("expected a freshly created job to have a nil impact peak, got %v", *got.ImpactPeakQueryDurationSeconds)
	}

	if err := store.UpdateImpactPeak(ctx, "job-impact-1", 2.35); err != nil {
		t.Fatalf("UpdateImpactPeak failed: %v", err)
	}

	got, err = store.Get(ctx, "job-impact-1")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if got.ImpactPeakQueryDurationSeconds == nil || *got.ImpactPeakQueryDurationSeconds != 2.35 {
		t.Errorf("expected ImpactPeakQueryDurationSeconds=2.35, got %v", got.ImpactPeakQueryDurationSeconds)
	}
}

// TestSQLiteStore_UpdateImpactPeak_DoesNotBumpUpdatedAt is the direct
// regression test for a real correctness concern caught during review:
// unlike most Update* methods here, UpdateImpactPeak is called
// repeatedly (write-through, on every poll while impact measurement is
// on) — if it also bumped updated_at the way UpdateRollbackDeadline
// does, it would silently inflate a migration's displayed duration
// (Health Card, Fleet analytics, the Migration Detail page's own
// duration stat all compute duration as UpdatedAt - CreatedAt) to match
// however long someone happened to have the impact checkbox on, not the
// migration's real duration.
func TestSQLiteStore_UpdateImpactPeak_DoesNotBumpUpdatedAt(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	job := &Job{ID: "job-impact-2", SchemaName: "public", TableName: "orders", Strategy: "SHADOW_TABLE", Phase: PhaseSyncing}
	if err := store.Create(ctx, job); err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	before, err := store.Get(ctx, "job-impact-2")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}

	// A real, measurable gap — if UpdatedAt gets bumped by
	// UpdateImpactPeak, this sleep makes that difference unambiguous
	// rather than potentially hidden by sub-millisecond timing noise.
	time.Sleep(50 * time.Millisecond)

	if err := store.UpdateImpactPeak(ctx, "job-impact-2", 1.0); err != nil {
		t.Fatalf("UpdateImpactPeak failed: %v", err)
	}

	after, err := store.Get(ctx, "job-impact-2")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	if !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("expected UpdateImpactPeak to leave UpdatedAt untouched, got before=%v after=%v", before.UpdatedAt, after.UpdatedAt)
	}
}

func TestSQLiteStore_UpdateImpactPeak_UnknownJob_ReturnsErrJobNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.UpdateImpactPeak(ctx, "does-not-exist", 1.0)
	if err != ErrJobNotFound {
		t.Errorf("expected ErrJobNotFound, got %v", err)
	}
}
