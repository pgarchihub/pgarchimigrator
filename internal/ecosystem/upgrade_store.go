package ecosystem

import (
	"context"
	"encoding/json"
	"log"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/entitlement"
	"github.com/pgarchihub/pgarchimigrator/internal/upgrade"
)

// UpgradeStore wraps a real upgrade.Store and publishes an ecosystem
// Envelope after Job-level lifecycle transitions — the internal/upgrade
// analog of Store (which does the identical thing for state.Store),
// same decorator pattern, same "the wrapped flow is completely unaware
// this exists" property: internal/upgrade.Flow already calls
// f.Store.CreateJob/UpdateJobPhase/UpdateJobPhaseWithError for every
// upgrade, exactly as it did before this type existed.
//
// Deliberately does NOT wrap the per-table methods (CreateTable,
// UpdateTablePhase, UpdateTableRowsSynced) — those can fire far more
// often than job-level transitions (see upgrade.SQLiteStore.
// UpdateTableRowsSynced's own "write-frugal" doc comment for how often
// even THAT already-throttled method is expected to be called), and an
// ecosystem event per table-progress update would defeat the entire
// point of that throttling one layer up. A consumer wanting per-table
// detail already has GET /api/upgrades/{id}, which returns every
// table's current state directly — polling that is the intended way to
// watch fine-grained upgrade progress, not an event stream at that
// granularity.
type UpgradeStore struct {
	upgrade.Store
	publisher      Publisher
	entitlement    entitlement.Checker
	instanceID     string
	productVersion string
}

func NewUpgradeStore(inner upgrade.Store, publisher Publisher, checker entitlement.Checker, instanceID, productVersion string) *UpgradeStore {
	return &UpgradeStore{
		Store:          inner,
		publisher:      publisher,
		entitlement:    checker,
		instanceID:     instanceID,
		productVersion: productVersion,
	}
}

func (s *UpgradeStore) CreateJob(ctx context.Context, job *upgrade.Job) error {
	if err := s.Store.CreateJob(ctx, job); err != nil {
		return err
	}
	s.publishForJob(ctx, job, EventTypeUpgradeRequested, "")
	return nil
}

func (s *UpgradeStore) UpdateJobPhase(ctx context.Context, jobID string, phase upgrade.Phase) error {
	if err := s.Store.UpdateJobPhase(ctx, jobID, phase); err != nil {
		return err
	}
	s.publishForPhaseTransition(ctx, jobID, phase, "")
	return nil
}

func (s *UpgradeStore) UpdateJobPhaseWithError(ctx context.Context, jobID string, phase upgrade.Phase, lastError string) error {
	if err := s.Store.UpdateJobPhaseWithError(ctx, jobID, phase, lastError); err != nil {
		return err
	}
	s.publishForPhaseTransition(ctx, jobID, phase, lastError)
	return nil
}

func (s *UpgradeStore) publishForPhaseTransition(ctx context.Context, jobID string, phase upgrade.Phase, lastError string) {
	job, err := s.Store.GetJob(ctx, jobID)
	if err != nil {
		// Same "never fail the caller's real operation over an event"
		// principle as Store.publishForPhaseTransition's own identical
		// re-read failure handling.
		log.Printf("ecosystem: could not re-read upgrade job %s to publish an event: %v", jobID, err)
		return
	}

	switch phase {
	case upgrade.PhaseReady:
		s.publishForJob(ctx, job, EventTypeUpgradeCompleted, lastError)
	case upgrade.PhaseFailed:
		s.publishForJob(ctx, job, EventTypeUpgradeFailed, lastError)
	default:
		// PhaseIntrospecting, PhaseSchemaCreated, PhaseSyncing,
		// PhaseValidating — every intermediate phase, Enterprise tier
		// only, same granularity split as Store's own identical switch.
		if s.entitlement.IsEnterprise() {
			s.publishPhaseChanged(ctx, job)
		}
	}
}

// upgradeEventData is EventTypeUpgrade*'s own Data payload — the
// upgrade.Job analog of MigrationEventData, same "small, stable,
// this product's own public contract" reasoning as that type's own doc
// comment.
type upgradeEventData struct {
	JobID          string `json:"jobId"`
	Phase          string `json:"phase"`
	TablesTotal    int    `json:"tablesTotal"`
	TablesSynced   int    `json:"tablesSynced"`
	TablesVerified int    `json:"tablesVerified"`
	Error          string `json:"error,omitempty"`
}

func (s *UpgradeStore) publishForJob(ctx context.Context, job *upgrade.Job, eventType EventType, errText string) {
	data := upgradeEventData{
		JobID:          job.ID,
		Phase:          string(job.Phase),
		TablesTotal:    job.TablesTotal,
		TablesSynced:   job.TablesSynced,
		TablesVerified: job.TablesVerified,
		Error:          errText,
	}
	s.publish(ctx, job, eventType, data)
}

func (s *UpgradeStore) publishPhaseChanged(ctx context.Context, job *upgrade.Job) {
	s.publishForJob(ctx, job, EventTypeUpgradePhaseChanged, "")
}

// publish mirrors Store.publish's own envelope-construction logic
// exactly, adapted for upgrade.Job's own (correlation-context-free)
// shape — see upgrade.Job's own fields: unlike state.Job, it carries no
// CorrelationID/CausationID/LifecycleID, since internal/upgrade isn't
// yet reachable through any ecosystem-initiated (OAuth2/202-pattern)
// endpoint the way POST /api/v1/migrations is — see
// docs/ecosystem/ARCHITECTURE.md's own "What's next" list. Every
// upgrade event published today therefore has empty correlation
// fields, matching how a migration started directly through this
// product's own CLI/dashboard (never through the ecosystem) also
// publishes with empty correlation fields — see
// Store.publishForJob's own identical behavior for that case.
func (s *UpgradeStore) publish(ctx context.Context, job *upgrade.Job, eventType EventType, data any) {
	payload, err := json.Marshal(data)
	if err != nil {
		log.Printf("ecosystem: could not encode event payload for upgrade job %s: %v", job.ID, err)
		return
	}

	now := time.Now().UTC()
	env := Envelope{
		SpecVersion: EnvelopeSpecVersion,
		EventID:     NewEventID(),
		EventType:   eventType,
		Source:      NewSource(s.instanceID),
		Subject:     ResourceTypeUpgradeJob + "/" + job.ID,
		OccurredAt:  now,
		PublishedAt: now,
		Producer: Producer{
			ProductID:      ProductID,
			ProductVersion: s.productVersion,
		},
		ResourceRef: ResourceRef{
			SourceProduct: ProductID,
			ResourceType:  ResourceTypeUpgradeJob,
			ResourceID:    job.ID,
		},
		Data: payload,
	}
	env.Trace = traceFromContext(ctx)

	if err := s.publisher.Publish(ctx, env); err != nil {
		log.Printf("ecosystem: failed to publish %s for upgrade job %s: %v", eventType, job.ID, err)
	}
}
