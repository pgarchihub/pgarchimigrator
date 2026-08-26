package orchestrator_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
)

// fakeStore is an in-memory implementation of state.Store for pure unit
// testing — no real database (and no SQLite file) needed.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[string]*state.Job
}

func newFakeStore() *fakeStore {
	return &fakeStore{jobs: map[string]*state.Job{}}
}

func (f *fakeStore) Create(ctx context.Context, job *state.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *job
	f.jobs[job.ID] = &cp
	return nil
}

func (f *fakeStore) UpdatePhase(ctx context.Context, jobID string, phase state.Phase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.Phase = phase
	return nil
}

func (f *fakeStore) UpdatePhaseWithError(ctx context.Context, jobID string, phase state.Phase, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.Phase = phase
	job.LastError = lastError
	return nil
}

func (f *fakeStore) UpdateResources(ctx context.Context, jobID string, slotName, shadowTableName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.ReplicationSlotName = slotName
	job.ShadowTableName = shadowTableName
	return nil
}

func (f *fakeStore) UpdateRollbackDeadline(ctx context.Context, jobID string, deadline time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.RollbackDeadline = &deadline
	return nil
}

func (f *fakeStore) UpdateImpactPeak(ctx context.Context, jobID string, peakSeconds float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.ImpactPeakQueryDurationSeconds = &peakSeconds
	return nil
}

func (f *fakeStore) Get(ctx context.Context, jobID string) (*state.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return nil, state.ErrJobNotFound
	}
	cp := *job
	return &cp, nil
}

func (f *fakeStore) ListStale(ctx context.Context, olderThan time.Duration) ([]*state.Job, error) {
	return nil, nil // not exercised by these tests
}

func (f *fakeStore) ListAll(ctx context.Context) ([]*state.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var jobs []*state.Job
	for _, job := range f.jobs {
		cp := *job
		jobs = append(jobs, &cp)
	}
	return jobs, nil
}

func (f *fakeStore) ListExpiredRollbackWindows(ctx context.Context) ([]*state.Job, error) {
	return nil, nil // not exercised by these tests
}

func (f *fakeStore) UpdateDeprecatedColumnName(ctx context.Context, jobID string, deprecatedName string) error {
	return nil // not exercised by these tests
}

func (f *fakeStore) UpdateIndexName(ctx context.Context, jobID string, indexName string) error {
	return nil // not exercised by these tests
}

func (f *fakeStore) UpdateIndexDefinition(ctx context.Context, jobID string, definition string) error {
	return nil // not exercised by these tests
}

func (f *fakeStore) UpdateConstraintName(ctx context.Context, jobID string, constraintName string) error {
	return nil // not exercised by these tests
}

func (f *fakeStore) IncrementRowsProcessed(ctx context.Context, jobID string, delta int64) error {
	return nil // not exercised by these tests
}

func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

// fakeFlow is a configurable orchestrator.Flow that never touches a real
// database — it just records whether Execute/Rollback were called and
// returns a preconfigured error (or nil).
type fakeFlow struct {
	executeErr  error
	rollbackErr error

	executeCalled  bool
	rollbackCalled bool
}

func (f *fakeFlow) Execute(ctx context.Context, job *state.Job) error {
	f.executeCalled = true
	return f.executeErr
}

func (f *fakeFlow) Rollback(ctx context.Context, job *state.Job) error {
	f.rollbackCalled = true
	return f.rollbackErr
}

// smallTableStats returns stats that will make strategy.Decide pick
// DIRECT_DDL for a plain ADD_COLUMN with a fixed default (the simplest,
// least ambiguous path through the decision matrix — see internal/strategy).
func smallTableStats(ctx context.Context, schema, table string) (strategy.TableStats, error) {
	return strategy.TableStats{EstimatedRowCount: 100, HasPrimaryKey: true}, nil
}

func fixedDefaultAddColumnChange() strategy.ColumnChange {
	return strategy.ColumnChange{Operation: strategy.OpAddColumn, ColumnName: "status", DefaultValue: "'active'"}
}

// TestStartMigration_CopiesIndexNameToJob is a regression test for a real
// bug found via manual CLI testing: StartMigration's Job construction
// listed every ColumnChange field it copies onto the new state.Job
// EXCEPT IndexName, which was silently dropped — a DROP_INDEX request
// with a correctly-supplied --index-name still reached DDLFlow with an
// empty job.IndexName, failing with a confusing "index name required"
// error despite the user having supplied one.
func TestStartMigration_CopiesIndexNameToJob(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders",
		Change: strategy.ColumnChange{Operation: strategy.OpDropIndex, IndexName: "idx_orders_status"},
	})
	if err != nil {
		t.Fatalf("StartMigration failed: %v", err)
	}
	if job.IndexName != "idx_orders_status" {
		t.Errorf("expected job.IndexName to be copied from the request, got %q", job.IndexName)
	}
}

// TestStartMigration_CopiesEveryColumnChangeField is a broader,
// harder-to-accidentally-defeat version of the regression test above: it
// sets EVERY field on ColumnChange that is supposed to reach the Job to a
// distinct, recognizable value, and asserts every one of them arrived —
// specifically so that adding a NEW field to ColumnChange in the future
// and forgetting to also copy it in StartMigration (exactly what happened
// with IndexName) gets caught by this one test, rather than needing a new
// per-field test to be remembered and added by hand each time.
//
// TypeConversionCompatible is deliberately NOT checked here — it's an
// input to strategy.Decide's strategy CHOICE only, not job state any flow
// reads later, so state.Job has no corresponding field for it at all.
func TestStartMigration_CopiesEveryColumnChangeField(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)

	change := strategy.ColumnChange{
		Operation:         strategy.OpAddColumn,
		ColumnName:        "field-test-column",
		NewType:           "field_test_type",
		DefaultValue:      "field-test-default",
		IsVolatileDefault: true,
		IndexName:         "field-test-index",
		ConstraintName:    "field-test-constraint",
		CheckExpression:   "field-test-check-expr",
		NewColumnName:     "field-test-new-column",
	}

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: change,
		Name: "field-test-name", Description: "field-test-description",
	})
	if err != nil {
		t.Fatalf("StartMigration failed: %v", err)
	}

	checks := []struct {
		name, want, got string
	}{
		{"Operation", string(change.Operation), job.Operation},
		{"ColumnName", change.ColumnName, job.ColumnName},
		{"ColumnType (from NewType)", change.NewType, job.ColumnType},
		{"DefaultValue", change.DefaultValue, job.DefaultValue},
		{"IndexName", change.IndexName, job.IndexName},
		{"ConstraintName", change.ConstraintName, job.ConstraintName},
		{"CheckExpression", change.CheckExpression, job.CheckExpression},
		{"NewColumnName", change.NewColumnName, job.NewColumnName},
		{"Name (from MigrationRequest, not ColumnChange)", "field-test-name", job.Name},
		{"Description (from MigrationRequest, not ColumnChange)", "field-test-description", job.Description},
	}
	for _, c := range checks {
		if c.want != c.got {
			t.Errorf("%s: expected %q to be copied onto the job, got %q", c.name, c.want, c.got)
		}
	}
	if job.IsVolatileDefault != change.IsVolatileDefault {
		t.Errorf("IsVolatileDefault: expected %v, got %v", change.IsVolatileDefault, job.IsVolatileDefault)
	}
}

// TestStartMigration_CopiesEstimatedRowCountFromStats is a companion to
// TestStartMigration_CopiesEveryColumnChangeField: EstimatedRowCount
// doesn't come from ColumnChange at all — it's copied from the
// TableStats already fetched to decide the strategy (see rawStats in
// StartMigration) — so it needed its own dedicated test rather than
// being covered by that one.
func TestStartMigration_CopiesEstimatedRowCountFromStats(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err != nil {
		t.Fatalf("StartMigration failed: %v", err)
	}
	if job.EstimatedRowCount != 100 { // smallTableStats always returns EstimatedRowCount: 100
		t.Errorf("expected job.EstimatedRowCount to be copied from TableStats (100), got %d", job.EstimatedRowCount)
	}
}

func TestStartMigration_Success(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err != nil {
		t.Fatalf("StartMigration failed: %v", err)
	}
	if job == nil {
		t.Fatal("expected a non-nil job")
	}
	if job.Strategy != string(strategy.StrategyDirectDDL) {
		t.Errorf("expected strategy DIRECT_DDL for a small table, got %s", job.Strategy)
	}
	if !flow.executeCalled {
		t.Error("expected Flow.Execute to have been called")
	}
	if store.count() != 1 {
		t.Errorf("expected 1 job to be persisted, got %d", store.count())
	}
}

func TestStartMigration_FlowExecuteFails_ReturnsJobAndError(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{executeErr: errors.New("boom")}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err == nil {
		t.Fatal("expected an error when Flow.Execute fails")
	}
	if job == nil {
		t.Fatal("expected StartMigration to still return the job on failure, so the caller can inspect it")
	}
}

func TestStartMigration_TableStatsFetchError_ReturnsError(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	failingStats := func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
		return strategy.TableStats{}, errors.New("db unreachable")
	}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, failingStats)

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err == nil {
		t.Fatal("expected an error when table stats can't be fetched")
	}
	if job != nil {
		t.Error("expected a nil job when stats-fetching fails before any job is created")
	}
	if store.count() != 0 {
		t.Errorf("expected no job to be persisted, got %d", store.count())
	}
	if flow.executeCalled {
		t.Error("Flow.Execute should never have been called")
	}
}

// TestStartMigration_VersionCheckFails_RefusesBeforeCreatingAJob is the
// direct regression guard for a real gap this whole feature exists to
// close: before VersionCheck existed, StartMigration never validated
// PostgreSQL's version for DIRECT_DDL/EXPAND_BACKFILL migrations at
// all — only the SHADOW_TABLE-specific preflight check (internal/db's
// PgxPreflighter) enforced TR-11's minimum version, meaning every OTHER
// strategy ran completely unchecked against it.
func TestStartMigration_VersionCheckFails_RefusesBeforeCreatingAJob(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)
	o.VersionCheck = func(ctx context.Context) error {
		return errors.New("PostgreSQL version is 11, minimum supported version is 12 (TR-11)")
	}

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err == nil {
		t.Fatal("expected an error when VersionCheck fails")
	}
	if job != nil {
		t.Error("expected a nil job — the version check must run before any job is created")
	}
	if store.count() != 0 {
		t.Errorf("expected no job to be persisted, got %d", store.count())
	}
	if flow.executeCalled {
		t.Error("Flow.Execute should never have been called")
	}
}

// TestStartMigration_NilVersionCheck_SkipsValidationEntirely proves the
// nil-is-fine contract VersionCheck's doc comment promises (matching
// AuditWriter's identical pattern) — existing callers/tests that never
// set it (the overwhelming majority in this file) must keep working
// exactly as before.
func TestStartMigration_NilVersionCheck_SkipsValidationEntirely(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)
	// o.VersionCheck deliberately left nil.

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err != nil {
		t.Fatalf("expected success with a nil VersionCheck, got: %v", err)
	}
	if job == nil {
		t.Fatal("expected a non-nil job")
	}
}

func TestStartMigration_StrategyDecideError_ReturnsError_NoJobCreated(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	partitionedStats := func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
		return strategy.TableStats{IsPartitioned: true}, nil
	}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, partitionedStats)

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err == nil {
		t.Fatal("expected an error for a partitioned table (TR-12)")
	}
	if job != nil {
		t.Error("expected a nil job when strategy.Decide fails before any job is created")
	}
	if store.count() != 0 {
		t.Errorf("expected no job to be persisted, got %d", store.count())
	}
}

func TestStartMigration_UsesInjectedIDGenerator(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)
	o.IDGenerator = func() string { return "fixed-test-id" }

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err != nil {
		t.Fatalf("StartMigration failed: %v", err)
	}
	if job.ID != "fixed-test-id" {
		t.Errorf("expected job.ID='fixed-test-id', got %q", job.ID)
	}
}

func TestStartMigration_RespectsStrategyOverride(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders",
		Change:           strategy.ColumnChange{Operation: strategy.OpAddColumn},
		StrategyOverride: strategy.StrategyExpandBackfill,
	})
	if err != nil {
		t.Fatalf("StartMigration failed: %v", err)
	}
	if job.Strategy != string(strategy.StrategyExpandBackfill) {
		t.Errorf("expected the override strategy to be respected, got %s", job.Strategy)
	}
}

func TestRollbackMigration_Success(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)

	seed := &state.Job{ID: "job-1", SchemaName: "public", TableName: "orders", Strategy: "DIRECT_DDL", Phase: state.PhaseFailed}
	if err := store.Create(context.Background(), seed); err != nil {
		t.Fatalf("could not seed job: %v", err)
	}

	job, err := o.RollbackMigration(context.Background(), "job-1", "test-actor")
	if err != nil {
		t.Fatalf("RollbackMigration failed: %v", err)
	}
	if job == nil {
		t.Fatal("expected a non-nil job")
	}
	if !flow.rollbackCalled {
		t.Error("expected Flow.Rollback to have been called")
	}
}

func TestRollbackMigration_JobNotFound_ReturnsError(t *testing.T) {
	store := newFakeStore()
	flow := &fakeFlow{}
	o := orchestrator.New(store, func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil }, smallTableStats)

	_, err := o.RollbackMigration(context.Background(), "nonexistent-id", "test-actor")
	if err == nil {
		t.Fatal("expected an error for a nonexistent job")
	}
	if flow.rollbackCalled {
		t.Error("Flow.Rollback should never have been called")
	}
}

func TestStartMigration_FlowBuilderError_ReturnsJobAndError(t *testing.T) {
	store := newFakeStore()
	failingBuilder := func(strategy.Strategy) (orchestrator.Flow, error) {
		return nil, fmt.Errorf("no flow registered")
	}
	o := orchestrator.New(store, failingBuilder, smallTableStats)

	job, err := o.StartMigration(context.Background(), orchestrator.MigrationRequest{
		SchemaName: "public", TableName: "orders", Change: fixedDefaultAddColumnChange(),
	})
	if err == nil {
		t.Fatal("expected an error when FlowFor fails")
	}
	// The job checkpoint was already created before FlowFor is consulted,
	// so it should still be returned for inspection.
	if job == nil {
		t.Fatal("expected a non-nil job even when FlowFor fails")
	}
	if store.count() != 1 {
		t.Errorf("expected the job to have been persisted before the FlowFor failure, got %d", store.count())
	}
}
