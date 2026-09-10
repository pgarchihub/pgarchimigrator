package ecosystem

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/entitlement"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// Store wraps a real state.Store and publishes an ecosystem Envelope
// after every state-mutating call that corresponds to a lifecycle
// moment the Archi ecosystem cares about — WITHOUT internal/engines/postgresql/ddlflow or
// internal/engines/postgresql/shadowflow needing a single line of new code. Those packages
// already call f.Store.Create/UpdatePhase/UpdatePhaseWithError for
// every migration, exactly as they did before this package existed;
// wiring pgArchiMigrator into the ecosystem is entirely a matter of
// handing the orchestrator THIS Store instead of the plain SQLite one
// at startup (see cmd/pgarchimigrator's wiring) — a config-level
// decision, not a change to any operation's own logic.
//
// Every other state.Store method is forwarded completely unchanged via
// the embedded interface — this type only overrides the three methods
// that correspond to a lifecycle transition worth telling the ecosystem
// about.
type Store struct {
	state.Store
	publisher   Publisher
	entitlement entitlement.Checker
	instanceID  string
	// productVersion is this build's own version — see
	// internal/version.Version; passed in rather than imported directly
	// so this package doesn't need a dependency on internal/version
	// purely for one string.
	productVersion string
}

// NewStore builds an event-publishing Store wrapping inner. publisher
// determines where events actually go (NoopPublisher for "nowhere",
// LogPublisher for today's real self-hosted behavior, or a future
// broker/webhook implementation); checker decides Community vs
// Enterprise event granularity; instanceID/productVersion populate
// every envelope's Source/Producer fields.
func NewStore(inner state.Store, publisher Publisher, checker entitlement.Checker, instanceID, productVersion string) *Store {
	return &Store{
		Store:          inner,
		publisher:      publisher,
		entitlement:    checker,
		instanceID:     instanceID,
		productVersion: productVersion,
	}
}

func (s *Store) Create(ctx context.Context, job *state.Job) error {
	if err := s.Store.Create(ctx, job); err != nil {
		return err
	}
	s.publishForJob(ctx, job, EventTypeMigrationRequested, "")
	return nil
}

func (s *Store) UpdatePhase(ctx context.Context, jobID string, phase state.Phase) error {
	if err := s.Store.UpdatePhase(ctx, jobID, phase); err != nil {
		return err
	}
	s.publishForPhaseTransition(ctx, jobID, phase, "")
	return nil
}

func (s *Store) UpdatePhaseWithError(ctx context.Context, jobID string, phase state.Phase, lastError string) error {
	if err := s.Store.UpdatePhaseWithError(ctx, jobID, phase, lastError); err != nil {
		return err
	}
	s.publishForPhaseTransition(ctx, jobID, phase, lastError)
	return nil
}

// publishForPhaseTransition resolves jobID to its current state.Job
// (needed for the envelope's Operation/SchemaName/TableName/etc. —
// UpdatePhase's own signature only carries jobID and the new phase) and
// decides which event, if any, this specific phase transition warrants.
//
// This one extra Store.Get call per phase transition is a deliberate,
// accepted cost — a migration job transitions phases at most a handful
// of times over what's typically a minutes-to-hours-long operation, so
// one additional SQLite read per transition is immaterial next to the
// operation's own cost, and it keeps every event's payload genuinely
// current rather than threading job state through call sites that
// don't otherwise need it.
func (s *Store) publishForPhaseTransition(ctx context.Context, jobID string, phase state.Phase, lastError string) {
	job, err := s.Store.Get(ctx, jobID)
	if err != nil {
		// The phase update itself already succeeded (we only reach
		// here after that succeeded) — a failed re-read for event
		// purposes must never fail the caller's actual operation. See
		// Publisher's own doc comment for the same "never fail the
		// real operation over an event" principle applied one layer
		// down.
		log.Printf("ecosystem: could not re-read job %s to publish an event: %v", jobID, err)
		return
	}

	switch phase {
	case state.PhaseCompleted:
		s.publishForJob(ctx, job, EventTypeMigrationCompleted, lastError)
	case state.PhaseFailed:
		s.publishForJob(ctx, job, EventTypeMigrationFailed, lastError)
	case state.PhaseAborted:
		s.publishForJob(ctx, job, EventTypeMigrationRolledBack, lastError)
	case state.PhasePreflight, state.PhasePreparation:
		// The first phase(s) any strategy passes through — a coarse
		// "this job is now actively running" signal every edition
		// gets, matching EventTypeMigrationRequested's own "every
		// edition" treatment just above.
		s.publishForJob(ctx, job, EventTypeMigrationStarted, lastError)
	default:
		// Every other intermediate phase (SYNCING, DELTA_SYNC,
		// VALIDATING, SWAPPING, ROLLBACK_WINDOW, CLEANUP) — Enterprise
		// tier only. A Community instance's entitlement.Checker makes
		// this a silent no-op, not a degraded/partial event.
		if s.entitlement.IsEnterprise() {
			s.publishPhaseChanged(ctx, job)
		}
	}
}

func (s *Store) publishForJob(ctx context.Context, job *state.Job, eventType EventType, errText string) {
	data := MigrationEventData{
		JobID:      job.ID,
		SchemaName: job.SchemaName,
		TableName:  job.TableName,
		Operation:  job.Operation,
		Strategy:   job.Strategy,
		Phase:      string(job.Phase),
		Error:      errText,
	}
	s.publish(ctx, job, eventType, data)
}

func (s *Store) publishPhaseChanged(ctx context.Context, job *state.Job) {
	data := MigrationPhaseChangedData{
		MigrationEventData: MigrationEventData{
			JobID:      job.ID,
			SchemaName: job.SchemaName,
			TableName:  job.TableName,
			Operation:  job.Operation,
			Strategy:   job.Strategy,
			Phase:      string(job.Phase),
		},
		RowsProcessed: job.RowsProcessed,
	}
	s.publish(ctx, job, EventTypeMigrationPhaseChanged, data)
}

// publish marshals data, builds the envelope around it, and calls the
// configured Publisher — the one place this whole file actually talks
// to Publisher, so every event this package emits is guaranteed to go
// through the exact same envelope-construction logic.
func (s *Store) publish(ctx context.Context, job *state.Job, eventType EventType, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		log.Printf("ecosystem: could not encode event payload for job %s: %v", job.ID, err)
		return
	}

	now := time.Now().UTC()
	env := Envelope{
		SpecVersion:   EnvelopeSpecVersion,
		EventID:       NewEventID(),
		EventType:     eventType,
		Source:        NewSource(s.instanceID),
		Subject:       ResourceTypeMigrationJob + "/" + job.ID,
		OccurredAt:    now,
		PublishedAt:   now,
		CorrelationID: job.CorrelationID,
		CausationID:   job.CausationID,
		LifecycleID:   job.LifecycleID,
		Producer: Producer{
			ProductID:      ProductID,
			ProductVersion: s.productVersion,
		},
		ResourceRef: ResourceRef{
			SourceProduct: ProductID,
			ResourceType:  ResourceTypeMigrationJob,
			ResourceID:    job.ID,
		},
		Data: payload,
	}
	env.Trace = traceFromContext(ctx)

	if err := s.publisher.Publish(ctx, env); err != nil {
		// Never propagated to the caller — see Publisher's own doc
		// comment. Logged here so a misconfigured/unreachable publisher
		// is at least diagnosable, not silently swallowed twice over.
		log.Printf("ecosystem: failed to publish %s for job %s: %v", eventType, job.ID, err)
	}
}
