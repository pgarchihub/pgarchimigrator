// Package upgrade implements PostgreSQL major-version upgrades — syncing
// an entire database's schema and data from an old-version instance to
// a new-version one, via the same logical-replication mechanism
// internal/shadowflow already uses for a single table, generalized
// across every table in scope.
//
// Deliberately a SEPARATE package with its OWN Job/Store, not a new
// operation type layered onto internal/strategy/internal/state.Job —
// see docs/ecosystem/ARCHITECTURE.md's "PostgreSQL upgrade" design note
// for the full reasoning: state.Job's whole shape (SchemaName,
// TableName, a single Strategy, a single set of DDL-operation fields)
// is built around "one change to one table." An upgrade's actual scope
// — every table across an entire database, syncing between two
// DIFFERENT PostgreSQL instances rather than one instance's live
// replica — doesn't fit that shape, and forcing it in would have
// meant either a state.Job with a dozen more nullable,
// upgrade-only fields, or a TableName that sometimes means "the one
// table this job changes" and sometimes means nothing at all. A
// parallel package with its own Job keeps internal/state's existing,
// well-tested 39-field Job exactly as focused as it already is.
//
// Scope split with pgArchiNode: this package does NOT provision,
// install, or configure the new-version PostgreSQL instance — that's
// pgArchiNode's own lifecycle-domain responsibility (see
// docs/ecosystem/source-specs' pgArchiNode adapter example,
// database.postgresql.cluster.upgrade). This package assumes both the
// old and new instances already exist and are reachable, and owns
// everything from "introspect the source schema" through "confirm the
// target is caught up and verified" — see Job.Phase's own doc comment
// for exactly where this package's own responsibility ends. The actual
// cutover (pointing application traffic at the new instance) is
// deliberately out of scope too — this tool has no way to control DNS,
// connection strings, or load balancer configuration, and the ecosystem
// principle of "don't provision infrastructure this product doesn't own
// (see the identical reasoning already applied to PARTITION_TABLE not
// provisioning storage) applies here at instance scope, not just table
// scope.
package upgrade

import (
	"context"
	"time"
)

// Phase tracks an upgrade job's progress — deliberately a DIFFERENT set
// of values from state.Phase (not reused), since an upgrade's real
// stages don't map onto a single migration's (there's no equivalent of
// SWAPPING or ROLLBACK_WINDOW here — see Phase's own values below for
// why "swap" isn't this package's concept at all).
type Phase string

const (
	// PhaseIntrospecting: enumerating every table in scope (see
	// Job.Schemas' own doc comment) on the source instance and building
	// the DDL to recreate them on the target — internal/catalog.ListColumns,
	// generalized across a whole database rather than one table, the
	// same introspection internal/shadowflow.createPartitionedShadowTable
	// already established the pattern for.
	PhaseIntrospecting Phase = "INTROSPECTING"
	// PhaseSchemaCreated: every in-scope table now exists on the target,
	// empty — schema-only, no data synced yet.
	PhaseSchemaCreated Phase = "SCHEMA_CREATED"
	// PhaseSyncing: logical replication running, copying existing rows
	// and streaming ongoing changes — the multi-table, cross-instance
	// generalization of internal/shadowflow's own initial sync + delta
	// sync, run once per in-scope table rather than the single table
	// SHADOW_TABLE handles.
	PhaseSyncing Phase = "SYNCING"
	// PhaseValidating: replication has caught up (source and target
	// are no longer meaningfully diverging); row counts/checksums are
	// being compared per table.
	PhaseValidating Phase = "VALIDATING"
	// PhaseReady is this package's own terminal success state — every
	// table synced and verified, target instance is a faithful,
	// up-to-date copy of the source. THIS IS AS FAR AS THIS PACKAGE
	// GOES — see the package doc comment's "cutover is out of scope"
	// paragraph. There is no PhaseCompleted/PhaseCutover; a caller
	// (a human operator, or an ecosystem workflow coordinating with
	// pgArchiNode) decides what happens next.
	PhaseReady   Phase = "READY"
	PhaseFailed  Phase = "FAILED"
	PhaseAborted Phase = "ABORTED"
)

// Job is one upgrade run — the whole-database analog of state.Job,
// deliberately NOT that type (see the package doc comment).
type Job struct {
	ID    string
	Phase Phase
	// Schemas is the explicit set of PostgreSQL schemas to include IN
	// FULL — every table in each listed schema. Empty means "every
	// schema on the source instance" (matching PARTITION_TABLE's own
	// "explicit list, or a sensible default" precedent, see
	// strategy.ExpandPartitionRule's doc comment for that earlier
	// design decision).
	//
	// Ignored entirely when Tables (below) is non-empty — see that
	// field's own doc comment for why the two are mutually exclusive
	// rather than combined.
	Schemas []string
	// Tables is an explicit list of SPECIFIC tables to include —
	// table-level scoping, as opposed to Schemas' own whole-schema
	// scoping. When non-empty, Introspect uses EXACTLY this list and
	// does not discover anything on its own; Schemas is ignored
	// entirely in that case. This is deliberately an either/or with
	// Schemas, not a union of the two — the dashboard's own
	// schema/table checkbox picker (see NewUpgrade.tsx) always expands
	// "this whole schema is checked" into every one of its tables
	// client-side before submission, so by the time a request reaches
	// this field, there is never any remaining ambiguity between "a
	// whole schema" and "some of its tables" for this package's own
	// logic to resolve. A caller using Schemas directly (the CLI's own
	// `--schemas` flag, or an ecosystem caller that doesn't need
	// table-level granularity) is unaffected — this field simply stays
	// empty for them, and today's original whole-schema behavior
	// applies exactly as before.
	Tables []TableRef
	// SourceConnectionRef/TargetConnectionRef are OPAQUE identifiers a
	// ConnectionProvider resolves to an actual DSN at the moment one is
	// needed — never a raw connection string stored directly on the
	// Job. See ConnectionProvider's own doc comment for why: a stored
	// job record living for the whole duration of a (potentially
	// hours-long) upgrade is exactly the kind of long-lived artifact
	// that shouldn't hold a long-lived plaintext credential if this
	// package can avoid it.
	SourceConnectionRef string
	TargetConnectionRef string
	// SourceReplicationRef is an OPTIONAL, separate connection reference
	// used ONLY for the CREATE SUBSCRIPTION statement in sync.go — see
	// that function's own doc comment for why this can genuinely differ
	// from SourceConnectionRef. Empty means "same as SourceConnectionRef"
	// (see ConnectionProvider.ReplicationDSN's own fallback behavior),
	// which is correct whenever this process and the target instance's
	// own PostgreSQL server have the same network view of the source
	// (the common case: a shared private network, or public/routable
	// hostnames). It must be set explicitly whenever they DON'T — the
	// concrete, real-world case that surfaced this field: a local
	// Docker Compose test where this process reaches the source via
	// "localhost:55432" (a host-mapped port) but the TARGET container's
	// own postgres server, issuing CREATE SUBSCRIPTION on ITS OWN
	// behalf, must instead use the Compose network's own service
	// hostname ("pg-logical:5432") — "localhost" from inside that
	// container refers to the container itself, not the host machine.
	SourceReplicationRef string
	LastError            string
	CreatedAt            time.Time
	UpdatedAt            time.Time
	// TablesTotal/TablesSynced/TablesVerified give a coarse, whole-job
	// progress signal — see Table's own doc comment for the per-table
	// detail this deliberately doesn't duplicate here; a caller wanting
	// per-table status calls Store.ListTables(ctx, job.ID) rather than
	// this package growing an ever-larger Job struct as more per-table
	// detail becomes useful (state.Job's own 39-field growth over this
	// project's history is the precedent being deliberately avoided
	// here).
	TablesTotal    int
	TablesSynced   int
	TablesVerified int
}

// Table tracks one in-scope table's own progress within a Job — the
// per-table detail Job itself deliberately doesn't carry (see
// Job.TablesTotal's own doc comment).
type Table struct {
	JobID      string
	SchemaName string
	TableName  string
	Phase      Phase // a subset of Job's own Phase values apply per-table: Introspecting/SchemaCreated/Syncing/Validating/Ready/Failed — never Aborted, which is job-scoped only
	RowsSynced int64
	LastError  string
}

// Store persists upgrade jobs and their per-table progress — its own
// interface, its own eventual SQLite implementation, deliberately not
// sharing internal/state.Store (same "parallel, not shoehorned in"
// reasoning as Job itself).
type Store interface {
	CreateJob(ctx context.Context, job *Job) error
	GetJob(ctx context.Context, jobID string) (*Job, error)
	ListJobs(ctx context.Context) ([]*Job, error)
	UpdateJobPhase(ctx context.Context, jobID string, phase Phase) error
	UpdateJobPhaseWithError(ctx context.Context, jobID string, phase Phase, lastError string) error
	UpdateJobProgress(ctx context.Context, jobID string, tablesTotal, tablesSynced, tablesVerified int) error

	CreateTable(ctx context.Context, table *Table) error
	ListTables(ctx context.Context, jobID string) ([]*Table, error)
	UpdateTablePhase(ctx context.Context, jobID, schemaName, tableName string, phase Phase, lastError string) error
	UpdateTableRowsSynced(ctx context.Context, jobID, schemaName, tableName string, rowsSynced int64) error
}

// ConnectionProvider resolves a Job's SourceConnectionRef/
// TargetConnectionRef to an actual, usable PostgreSQL connection string
// at the moment one is needed — the seam a deployment topology plugs
// into, matching internal/entitlement.Checker's own "depend on the
// interface, not on how it's implemented today" reasoning (see that
// package's doc comment) for the identical future-proofing reason.
//
// Community/Enterprise self-hosted's normal case (StaticConnectionProvider,
// below) just returns whatever connection string was supplied at job
// creation, stored as-is. A future Cloud deployment — see
// docs/ecosystem/ARCHITECTURE.md's own "Cloud's connection model" open
// question — can implement this same interface to resolve a connection
// ref through a customer-side agent/connector instead, without this
// package's own sync/validate logic needing to know the difference.
type ConnectionProvider interface {
	SourceDSN(ctx context.Context, job *Job) (string, error)
	TargetDSN(ctx context.Context, job *Job) (string, error)
	// ReplicationDSN returns the connection string the TARGET
	// instance's own PostgreSQL server should use in its
	// CREATE SUBSCRIPTION statement — see Job.SourceReplicationRef's
	// own doc comment for why this is a genuinely separate value from
	// SourceDSN (which is what THIS PROCESS uses to connect to the
	// source directly), not just an alias for it.
	ReplicationDSN(ctx context.Context, job *Job) (string, error)
}

// StaticConnectionProvider is today's only ConnectionProvider — treats
// SourceConnectionRef/TargetConnectionRef as the literal DSN, unchanged.
// This means the connection string genuinely is stored on the Job
// record for this implementation — an accepted limitation for the
// self-hosted case (see Job.SourceConnectionRef's own doc comment for
// why that's not ideal in the abstract), matching how this project's
// own primary PGARCHIMIGRATOR_DATABASE_URL is handled today: a plain
// connection string, not run through a secrets manager. Revisiting this
// with real secret storage is possible later WITHOUT changing this
// interface — only which ConnectionProvider implementation a deployment
// wires up.
type StaticConnectionProvider struct{}

func (StaticConnectionProvider) SourceDSN(ctx context.Context, job *Job) (string, error) {
	return job.SourceConnectionRef, nil
}

func (StaticConnectionProvider) TargetDSN(ctx context.Context, job *Job) (string, error) {
	return job.TargetConnectionRef, nil
}

// ReplicationDSN falls back to SourceConnectionRef when
// SourceReplicationRef wasn't set — see that field's own doc comment
// for exactly when a caller must set it explicitly instead.
func (StaticConnectionProvider) ReplicationDSN(ctx context.Context, job *Job) (string, error) {
	if job.SourceReplicationRef != "" {
		return job.SourceReplicationRef, nil
	}
	return job.SourceConnectionRef, nil
}
