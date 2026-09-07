package ecosystem

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"
)

// NewEventID produces a reasonably unique, sortable-by-time event ID
// without an external UUID/ULID dependency — same reasoning and same
// mechanism as internal/orchestrator.generateJobID (a nanosecond
// timestamp plus 8 random bytes), prefixed "evt-" rather than "job-".
// This deliberately doesn't attempt to match AC-PF-003's own illustrative
// "evt_01J..." (ULID-shaped) examples exactly — the spec requires
// producer-generated global uniqueness, not a specific ID scheme, and
// pulling in a ULID library for this alone isn't worth the added
// dependency (see this project's own established "no external ID
// generator" precedent).
func NewEventID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf) // crypto/rand.Read never returns a partial read on success
	return fmt.Sprintf("evt-%d-%s", time.Now().UnixNano(), hex.EncodeToString(buf))
}

// NewSource builds the envelope's "source" field — AC-PF-003's own
// example is "archi://archifabric/instance/prod-01". instanceID
// identifies THIS running instance (not a job, not a tenant) — callers
// typically pass a stable value configured once at process startup
// (e.g. hostname, or an operator-assigned instance name); an empty
// instanceID still produces a valid, if less specific, source.
func NewSource(instanceID string) string {
	if instanceID == "" {
		return fmt.Sprintf("archi://%s/instance/unknown", ProductID)
	}
	return fmt.Sprintf("archi://%s/instance/%s", ProductID, instanceID)
}

// EventType values this package emits — see envelope.go's own doc
// comment on Envelope.EventType for the <product>.<domain>.
// <past-tense-event>.v<major> naming convention this follows (AC-PF-003
// Section 11.3). "migration" is this product's one lifecycle domain
// today (matches archi-product-manifest.yaml's spec.lifecycleDomains).
//
// The four coarse-grained types are published regardless of edition —
// see entitlement.Checker's own doc comment for why Community gets
// these but not EventTypeMigrationPhaseChanged: a Community operator
// watching ArchiConsole should see that a migration started and how it
// ended, without this product needing Enterprise-tier telemetry
// plumbing to prove it works at all.
const (
	EventTypeMigrationRequested  EventType = ProductID + ".migration.requested.v1"
	EventTypeMigrationStarted    EventType = ProductID + ".migration.started.v1"
	EventTypeMigrationCompleted  EventType = ProductID + ".migration.completed.v1"
	EventTypeMigrationFailed     EventType = ProductID + ".migration.failed.v1"
	EventTypeMigrationRolledBack EventType = ProductID + ".migration.rolledback.v1"

	// EventTypeMigrationPhaseChanged is Enterprise-only (see
	// entitlement_gate.go) — one event per internal phase transition
	// (PREFLIGHT, PREPARATION, SYNCING, DELTA_SYNC, VALIDATING,
	// SWAPPING, ROLLBACK_WINDOW, CLEANUP), matching the fine-grained
	// "every phase transition, replication lag, source-level details"
	// telemetry depth the Enterprise tier promises ArchiMonitor/
	// Lifecycle Intelligence.
	EventTypeMigrationPhaseChanged EventType = ProductID + ".migration.phase_changed.v1"

	// Upgrade* mirrors the Migration* set above exactly, one domain
	// segment over ("upgrade" rather than "migration") — internal/upgrade
	// is its own package with its own Job/Phase (see that package's own
	// doc comment for why), but the ecosystem-facing event shape it
	// publishes follows this package's already-established convention
	// rather than inventing a different one. There is no
	// EventTypeUpgradeStarted distinct from Requested: internal/upgrade's
	// own CreateJob synchronously sets the job's first real phase
	// (PhaseIntrospecting) before returning — unlike a migration job,
	// where Requested (Create) and Started (the first phase transition)
	// are genuinely two separate moments in time — so a second, always
	// simultaneous event would carry no information Requested didn't
	// already carry.
	EventTypeUpgradeRequested EventType = ProductID + ".upgrade.requested.v1"
	EventTypeUpgradeCompleted EventType = ProductID + ".upgrade.completed.v1" // job reached PhaseReady
	EventTypeUpgradeFailed    EventType = ProductID + ".upgrade.failed.v1"
	// EventTypeUpgradePhaseChanged is Enterprise-only, same reasoning as
	// EventTypeMigrationPhaseChanged — one event per intermediate phase
	// (PhaseSchemaCreated, PhaseSyncing, PhaseValidating).
	EventTypeUpgradePhaseChanged EventType = ProductID + ".upgrade.phase_changed.v1"
)

// EventType is a distinct string type (not a bare string) so a call
// site passing a raw string literal where an EventType is expected is a
// compile error, not a silently-accepted typo — the same reasoning
// entitlement.Edition already follows.
type EventType string
