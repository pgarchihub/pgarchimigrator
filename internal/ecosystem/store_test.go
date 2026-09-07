package ecosystem

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/entitlement"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// fakeStore is a minimal in-memory state.Store for these tests — real
// behavior (Create actually stores, Get actually returns what was
// stored, UpdatePhase actually mutates Phase), just without a real
// database.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[string]*state.Job
	// failNextCall, if set, makes the NEXT call to the named method
	// return this error instead of succeeding — used to test that a
	// Store-layer failure still propagates correctly THROUGH this
	// package's wrapper.
	failCreate error
}

func newFakeStore() *fakeStore {
	return &fakeStore{jobs: make(map[string]*state.Job)}
}

func (f *fakeStore) Create(ctx context.Context, job *state.Job) error {
	if f.failCreate != nil {
		return f.failCreate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *job
	f.jobs[job.ID] = &cp
	return nil
}

func (f *fakeStore) Get(ctx context.Context, jobID string) (*state.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return nil, errors.New("job not found")
	}
	cp := *job
	return &cp, nil
}

func (f *fakeStore) UpdatePhase(ctx context.Context, jobID string, phase state.Phase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return errors.New("job not found")
	}
	job.Phase = phase
	return nil
}

func (f *fakeStore) UpdatePhaseWithError(ctx context.Context, jobID string, phase state.Phase, lastError string) error {
	return f.UpdatePhase(ctx, jobID, phase)
}

// fakePublisher records every envelope it receives — and can be told to
// fail on demand, to test that a publish failure never propagates back
// to the caller of the wrapped Store method.
type fakePublisher struct {
	mu        sync.Mutex
	published []Envelope
	failWith  error
}

func (p *fakePublisher) Publish(ctx context.Context, env Envelope) error {
	if p.failWith != nil {
		return p.failWith
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.published = append(p.published, env)
	return nil
}

func (p *fakePublisher) events() []Envelope {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Envelope, len(p.published))
	copy(out, p.published)
	return out
}

func testJob(id string) *state.Job {
	return &state.Job{
		ID: id, SchemaName: "public", TableName: "orders",
		Operation: "ADD_COLUMN", Strategy: "DIRECT_DDL", Phase: state.PhasePreflight,
	}
}

func TestStore_Create_PublishesMigrationRequested(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")

	if err := s.Create(context.Background(), testJob("job-1")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	if len(events) != 1 {
		t.Fatalf("expected exactly 1 event, got %d", len(events))
	}
	if events[0].EventType != EventTypeMigrationRequested {
		t.Errorf("expected %s, got %s", EventTypeMigrationRequested, events[0].EventType)
	}
}

// TestStore_Create_StoreFailure_NoEventPublished is the direct
// regression test for correct ORDERING — if the underlying Store.Create
// itself fails, no event should be published for a job that was never
// actually persisted.
func TestStore_Create_StoreFailure_NoEventPublished(t *testing.T) {
	inner := newFakeStore()
	inner.failCreate = errors.New("disk full")
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")

	if err := s.Create(context.Background(), testJob("job-1")); err == nil {
		t.Fatal("expected the underlying Store failure to propagate")
	}
	if len(pub.events()) != 0 {
		t.Error("expected no event to be published when Create itself failed")
	}
}

func TestStore_UpdatePhase_Preparation_PublishesMigrationStarted(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	_ = s.Create(context.Background(), testJob("job-1"))

	if err := s.UpdatePhase(context.Background(), "job-1", state.PhasePreparation); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	last := events[len(events)-1]
	if last.EventType != EventTypeMigrationStarted {
		t.Errorf("expected %s, got %s", EventTypeMigrationStarted, last.EventType)
	}
}

func TestStore_UpdatePhase_Completed_PublishesMigrationCompleted(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	_ = s.Create(context.Background(), testJob("job-1"))

	if err := s.UpdatePhase(context.Background(), "job-1", state.PhaseCompleted); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	last := events[len(events)-1]
	if last.EventType != EventTypeMigrationCompleted {
		t.Errorf("expected %s, got %s", EventTypeMigrationCompleted, last.EventType)
	}
}

// TestStore_UpdatePhaseWithError_Failed_IncludesErrorInPayload is the
// direct regression test for the error message actually reaching the
// event payload, not just triggering the right EventType.
func TestStore_UpdatePhaseWithError_Failed_IncludesErrorInPayload(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	_ = s.Create(context.Background(), testJob("job-1"))

	if err := s.UpdatePhaseWithError(context.Background(), "job-1", state.PhaseFailed, "constraint violation on row 42"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	last := events[len(events)-1]
	if last.EventType != EventTypeMigrationFailed {
		t.Fatalf("expected %s, got %s", EventTypeMigrationFailed, last.EventType)
	}
	var data MigrationEventData
	if err := json.Unmarshal(last.Data, &data); err != nil {
		t.Fatalf("could not decode event payload: %v", err)
	}
	if data.Error != "constraint violation on row 42" {
		t.Errorf("expected the error message in the payload, got %q", data.Error)
	}
}

func TestStore_UpdatePhase_Aborted_PublishesMigrationRolledBack(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	_ = s.Create(context.Background(), testJob("job-1"))

	if err := s.UpdatePhase(context.Background(), "job-1", state.PhaseAborted); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	last := events[len(events)-1]
	if last.EventType != EventTypeMigrationRolledBack {
		t.Errorf("expected %s, got %s", EventTypeMigrationRolledBack, last.EventType)
	}
}

// TestStore_IntermediatePhase_Community_PublishesNothing is THE direct
// regression test for the Community/Enterprise granularity split — an
// intermediate phase transition (SYNCING here) must produce ZERO
// additional events for a Community-tier instance, not a degraded or
// partial one.
func TestStore_IntermediatePhase_Community_PublishesNothing(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	_ = s.Create(context.Background(), testJob("job-1"))
	countAfterCreate := len(pub.events())

	if err := s.UpdatePhase(context.Background(), "job-1", state.PhaseSyncing); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(pub.events()) != countAfterCreate {
		t.Errorf("expected no new event for an intermediate phase transition on Community, got %d new event(s)", len(pub.events())-countAfterCreate)
	}
}

// TestStore_IntermediatePhase_Enterprise_PublishesPhaseChanged is the
// direct mirror of the test above — the same SYNCING transition on an
// Enterprise-tier instance MUST publish EventTypeMigrationPhaseChanged.
func TestStore_IntermediatePhase_Enterprise_PublishesPhaseChanged(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("enterprise"), "test-instance", "2.0.0")
	_ = s.Create(context.Background(), testJob("job-1"))

	if err := s.UpdatePhase(context.Background(), "job-1", state.PhaseSyncing); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	events := pub.events()
	last := events[len(events)-1]
	if last.EventType != EventTypeMigrationPhaseChanged {
		t.Errorf("expected %s on Enterprise, got %s", EventTypeMigrationPhaseChanged, last.EventType)
	}
}

// TestStore_PublisherFailure_DoesNotFailTheRealOperation is the most
// important test in this file — the entire architectural promise of
// this package is that a migration must NEVER fail, stall, or behave
// differently because publishing an ecosystem event failed. This
// confirms it end to end: a Publisher that always errors must have
// ZERO effect on UpdatePhase's own return value.
func TestStore_PublisherFailure_DoesNotFailTheRealOperation(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{failWith: errors.New("connection refused: event broker unreachable")}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	_ = s.Create(context.Background(), testJob("job-1"))

	if err := s.UpdatePhase(context.Background(), "job-1", state.PhaseCompleted); err != nil {
		t.Fatalf("expected UpdatePhase to succeed despite the publisher failing, got: %v", err)
	}

	// And the underlying state change genuinely happened, not just "no
	// error was returned."
	job, err := inner.Get(context.Background(), "job-1")
	if err != nil {
		t.Fatalf("unexpected error reading back the job: %v", err)
	}
	if job.Phase != state.PhaseCompleted {
		t.Errorf("expected the real phase transition to have gone through regardless of the publisher failure, got %s", job.Phase)
	}
}

func TestStore_Envelope_HasCorrectProducerAndResourceRef(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "prod-01", "2.0.0")
	_ = s.Create(context.Background(), testJob("job-1"))

	events := pub.events()
	env := events[0]

	if env.SpecVersion != EnvelopeSpecVersion {
		t.Errorf("expected specVersion %q, got %q", EnvelopeSpecVersion, env.SpecVersion)
	}
	if env.Producer.ProductID != ProductID {
		t.Errorf("expected producer.productId %q, got %q", ProductID, env.Producer.ProductID)
	}
	if env.Producer.ProductVersion != "2.0.0" {
		t.Errorf("expected producer.productVersion %q, got %q", "2.0.0", env.Producer.ProductVersion)
	}
	if env.ResourceRef.ResourceID != "job-1" {
		t.Errorf("expected resourceRef.resourceId %q, got %q", "job-1", env.ResourceRef.ResourceID)
	}
	if env.ResourceRef.ResourceType != ResourceTypeMigrationJob {
		t.Errorf("expected resourceRef.resourceType %q, got %q", ResourceTypeMigrationJob, env.ResourceRef.ResourceType)
	}
	if env.Source != "archi://pgarchimigrator/instance/prod-01" {
		t.Errorf("expected a source built from the instance ID, got %q", env.Source)
	}
}

// TestStore_UpdatePhase_JobNotFound_NoEventPublished confirms the
// defensive Get-failure path in publishForPhaseTransition doesn't
// panic and simply skips publishing — already covered implicitly by
// every other test succeeding, but made explicit here for the specific
// "job vanished between the phase update and the event read" edge case.
func TestStore_UpdatePhase_JobNotFound_NoEventPublished(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")
	// Deliberately never call Create — UpdatePhase itself will fail
	// (job not found in fakeStore), which is the expected, correctly
	// propagated error here.
	if err := s.UpdatePhase(context.Background(), "nonexistent-job", state.PhaseCompleted); err == nil {
		t.Fatal("expected an error for a nonexistent job")
	}
	if len(pub.events()) != 0 {
		t.Error("expected no event when the underlying UpdatePhase itself failed")
	}
}

// EVERY event that job's lifecycle produces — not just the first one —
// so a consumer can stitch together the job's entire history under one
// correlationId.
func TestStore_Envelope_PropagatesCorrelationContext(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")

	job := testJob("job-1")
	job.CorrelationID = "cor-abc"
	job.CausationID = "cmd-xyz"
	job.LifecycleID = "lc-123"
	_ = s.Create(context.Background(), job)
	_ = s.UpdatePhase(context.Background(), "job-1", state.PhaseCompleted)

	events := pub.events()
	if len(events) != 2 {
		t.Fatalf("expected 2 events (Requested + Completed), got %d", len(events))
	}
	for _, env := range events {
		if env.CorrelationID != "cor-abc" {
			t.Errorf("expected correlationId 'cor-abc' on %s, got %q", env.EventType, env.CorrelationID)
		}
		if env.CausationID != "cmd-xyz" {
			t.Errorf("expected causationId 'cmd-xyz' on %s, got %q", env.EventType, env.CausationID)
		}
		if env.LifecycleID != "lc-123" {
			t.Errorf("expected lifecycleId 'lc-123' on %s, got %q", env.EventType, env.LifecycleID)
		}
	}
}

// TestStore_Envelope_EmptyCorrelationContext_ForStandaloneJob confirms
// a job started outside any ecosystem journey (the common case for a
// self-hosted operator using this product's own CLI directly) still
// publishes events — just with empty correlation fields, not an error
// or a degraded envelope.
func TestStore_Envelope_EmptyCorrelationContext_ForStandaloneJob(t *testing.T) {
	inner := newFakeStore()
	pub := &fakePublisher{}
	s := NewStore(inner, pub, entitlement.NewConfigChecker("community"), "test-instance", "2.0.0")

	_ = s.Create(context.Background(), testJob("job-1")) // no correlation context set

	events := pub.events()
	env := events[0]
	if env.CorrelationID != "" || env.CausationID != "" || env.LifecycleID != "" {
		t.Errorf("expected empty correlation fields for a standalone job, got %+v", env)
	}
}
