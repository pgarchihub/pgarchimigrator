package ecosystem

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/entitlement"
	"github.com/pgarchihub/pgarchimigrator/internal/upgrade"
)

// fakeUpgradeStore is a minimal in-memory upgrade.Store — mirrors
// store_test.go's own fakeStore pattern for state.Store, adapted for
// upgrade.Job/Table.
type fakeUpgradeStore struct {
	mu   sync.Mutex
	jobs map[string]*upgrade.Job
}

func newFakeUpgradeStore() *fakeUpgradeStore {
	return &fakeUpgradeStore{jobs: make(map[string]*upgrade.Job)}
}

func (f *fakeUpgradeStore) CreateJob(ctx context.Context, job *upgrade.Job) error {
	if job.ID == "" {
		job.ID = "upgrade-test-job"
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *job
	f.jobs[job.ID] = &cp
	return nil
}

func (f *fakeUpgradeStore) GetJob(ctx context.Context, jobID string) (*upgrade.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return nil, errors.New("job not found")
	}
	cp := *job
	return &cp, nil
}

func (f *fakeUpgradeStore) ListJobs(ctx context.Context) ([]*upgrade.Job, error) { return nil, nil }

func (f *fakeUpgradeStore) UpdateJobPhase(ctx context.Context, jobID string, phase upgrade.Phase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return errors.New("job not found")
	}
	job.Phase = phase
	return nil
}

func (f *fakeUpgradeStore) UpdateJobPhaseWithError(ctx context.Context, jobID string, phase upgrade.Phase, lastError string) error {
	return f.UpdateJobPhase(ctx, jobID, phase)
}

func (f *fakeUpgradeStore) UpdateJobProgress(ctx context.Context, jobID string, tablesTotal, tablesSynced, tablesVerified int) error {
	return nil
}
func (f *fakeUpgradeStore) CreateTable(ctx context.Context, table *upgrade.Table) error { return nil }
func (f *fakeUpgradeStore) ListTables(ctx context.Context, jobID string) ([]*upgrade.Table, error) {
	return nil, nil
}
func (f *fakeUpgradeStore) UpdateTablePhase(ctx context.Context, jobID, schemaName, tableName string, phase upgrade.Phase, lastError string) error {
	return nil
}
func (f *fakeUpgradeStore) UpdateTableRowsSynced(ctx context.Context, jobID, schemaName, tableName string, rowsSynced int64) error {
	return nil
}

func testUpgradeJob() *upgrade.Job {
	return &upgrade.Job{Phase: upgrade.PhaseIntrospecting}
}

func TestUpgradeStore_CreateJob_PublishesUpgradeRequested(t *testing.T) {
	inner := newFakeUpgradeStore()
	pub := &fakePublisher{}
	s := NewUpgradeStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")

	if err := s.CreateJob(context.Background(), testUpgradeJob()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(events))
	}
	if events[0].EventType != EventTypeUpgradeRequested {
		t.Errorf("expected %s, got %s", EventTypeUpgradeRequested, events[0].EventType)
	}
	if events[0].ResourceRef.ResourceType != ResourceTypeUpgradeJob {
		t.Errorf("expected resourceType %s, got %s", ResourceTypeUpgradeJob, events[0].ResourceRef.ResourceType)
	}
}

func TestUpgradeStore_UpdateJobPhase_Ready_PublishesUpgradeCompleted(t *testing.T) {
	inner := newFakeUpgradeStore()
	pub := &fakePublisher{}
	s := NewUpgradeStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	job := testUpgradeJob()
	_ = s.CreateJob(context.Background(), job)

	if err := s.UpdateJobPhase(context.Background(), job.ID, upgrade.PhaseReady); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	last := events[len(events)-1]
	if last.EventType != EventTypeUpgradeCompleted {
		t.Errorf("expected %s, got %s", EventTypeUpgradeCompleted, last.EventType)
	}
}

func TestUpgradeStore_UpdateJobPhaseWithError_Failed_PublishesUpgradeFailed(t *testing.T) {
	inner := newFakeUpgradeStore()
	pub := &fakePublisher{}
	s := NewUpgradeStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	job := testUpgradeJob()
	_ = s.CreateJob(context.Background(), job)

	if err := s.UpdateJobPhaseWithError(context.Background(), job.ID, upgrade.PhaseFailed, "connection refused"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	last := events[len(events)-1]
	if last.EventType != EventTypeUpgradeFailed {
		t.Errorf("expected %s, got %s", EventTypeUpgradeFailed, last.EventType)
	}
}

// TestUpgradeStore_IntermediatePhase_Community_PublishesNothing mirrors
// Store's own identical Community/Enterprise granularity test — the
// same split, applied here.
func TestUpgradeStore_IntermediatePhase_Community_PublishesNothing(t *testing.T) {
	inner := newFakeUpgradeStore()
	pub := &fakePublisher{}
	s := NewUpgradeStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	job := testUpgradeJob()
	_ = s.CreateJob(context.Background(), job)
	countAfterCreate := len(pub.events())

	if err := s.UpdateJobPhase(context.Background(), job.ID, upgrade.PhaseSyncing); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pub.events()) != countAfterCreate {
		t.Errorf("expected no new event for an intermediate phase on Community, got %d new event(s)", len(pub.events())-countAfterCreate)
	}
}

func TestUpgradeStore_IntermediatePhase_Enterprise_PublishesPhaseChanged(t *testing.T) {
	inner := newFakeUpgradeStore()
	pub := &fakePublisher{}
	s := NewUpgradeStore(inner, pub, entitlement.NewConfigChecker("enterprise"), "test-instance", "2.0.0")
	job := testUpgradeJob()
	_ = s.CreateJob(context.Background(), job)

	if err := s.UpdateJobPhase(context.Background(), job.ID, upgrade.PhaseSyncing); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	last := events[len(events)-1]
	if last.EventType != EventTypeUpgradePhaseChanged {
		t.Errorf("expected %s on Enterprise, got %s", EventTypeUpgradePhaseChanged, last.EventType)
	}
}

// TestUpgradeStore_PublisherFailure_DoesNotFailTheRealOperation mirrors
// Store's own most important test — the same guarantee, extended to
// upgrade jobs: an event-publishing failure must never affect the
// underlying upgrade.Store operation's own success.
func TestUpgradeStore_PublisherFailure_DoesNotFailTheRealOperation(t *testing.T) {
	inner := newFakeUpgradeStore()
	pub := &fakePublisher{failWith: errors.New("event broker unreachable")}
	s := NewUpgradeStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	job := testUpgradeJob()
	_ = s.CreateJob(context.Background(), job)

	if err := s.UpdateJobPhase(context.Background(), job.ID, upgrade.PhaseReady); err != nil {
		t.Fatalf("expected UpdateJobPhase to succeed despite the publisher failing, got: %v", err)
	}

	got, err := inner.GetJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("unexpected error reading back the job: %v", err)
	}
	if got.Phase != upgrade.PhaseReady {
		t.Errorf("expected the real phase transition to have gone through regardless of the publisher failure, got %s", got.Phase)
	}
}
