// Mirrors internal/api's JSON shapes exactly — see internal/progress.Report,
// internal/preview.Report, and internal/api/server.go's request/response
// structs. Keep these in sync manually; there is no code generation step
// (yet) between the Go backend and this TypeScript client.

export type Phase =
  | "PREFLIGHT"
  | "PREPARATION"
  | "SYNCING"
  | "DELTA_SYNC"
  | "VALIDATING"
  | "SWAPPING"
  | "ROLLBACK_WINDOW"
  | "CLEANUP"
  | "COMPLETED"
  | "FAILED"
  | "ABORTED";

export type Role = "viewer" | "operator" | "admin";

export type Operation =
  | "ADD_COLUMN"
  | "DROP_COLUMN"
  | "ALTER_COLUMN_TYPE"
  | "ADD_INDEX"
  | "DROP_INDEX"
  | "SET_NOT_NULL"
  | "ADD_CONSTRAINT"
  | "RENAME_COLUMN";

// Mirrors strategy.ValidStrategyMatrix()'s JSON shape — keyed by
// Operation, each value the list of strategies that operation actually
// supports (see internal/strategy's validStrategiesByOperation doc
// comment for why this whitelist exists at all: forcing an operation
// through a strategy whose flow has no logic for it used to be silently
// accepted and then silently did nothing useful).
export type StrategyMatrix = Partial<Record<Operation, string[]>>;

// Mirrors internal/api's writeLoadEstimate — a SERVER-WIDE (not
// table-specific; PostgreSQL doesn't offer per-table WAL generation
// stats) snapshot of current write volume, sampled over
// bytesPerSecond/sampleSeconds. caution is advisory only — see the Go
// struct's own doc comment for why this is never used to block a
// migration from starting, only to inform the person deciding.
export interface WriteLoadEstimate {
  bytesPerSecond: number;
  sampleSeconds: number;
  caution: boolean;
}

// Mirrors internal/progress.Analytics/StrategyStats — computed entirely
// from existing job records server-side (no new database queries
// against the target PostgreSQL server), served by GET /api/analytics.
export interface StrategyStats {
  count: number;
  failureRate: number;
  averageDurationMs: number;
}

export interface Analytics {
  totalMigrations: number;
  terminalMigrations: number;
  failureRate: number;
  averageDurationMs: number;
  strategyBreakdown: Record<string, StrategyStats>;
}

export interface StageView {
  Phase: Phase;
  // Mirrors internal/progress's StageStatus constants exactly —
  // "DONE"/"CURRENT"/"PENDING", uppercase. A prior version of this type
  // declared these lowercase, which silently never matched the real
  // backend values: every comparison against it fell through to the
  // "pending" branch everywhere it was used (PhaseTrack's colors,
  // MigrationDetail's step list), making a completed migration's
  // progress track render as if nothing had happened yet. Caught via a
  // real user report; the bug survived undetected because this session's
  // own tests used the same wrong lowercase literals as fixtures,
  // validating consistency with the wrong assumption rather than the
  // real contract.
  Status: "DONE" | "CURRENT" | "PENDING";
}

// Mirrors internal/progress.Report. CreatedAt/UpdatedAt are Go time.Time
// values, which encoding/json serializes as RFC3339 strings — kept as
// `string` here and parsed with `new Date(...)` at the point of use
// rather than eagerly, since a zero time.Time (e.g. a field genuinely
// never set) still encodes to a valid-looking string
// ("0001-01-01T00:00:00Z") that call sites need to guard against anyway.
export interface MigrationReport {
  JobID: string;
  SchemaName: string;
  TableName: string;
  Strategy: string;
  CurrentPhase: Phase;
  Stages: StageView[];
  PercentComplete: number;
  Terminal: boolean;
  Failed: boolean;
  LastError: string;
  CreatedAt: string;
  UpdatedAt: string;
  EstimatedRowCount: number;
  RowsProcessed: number;
  // Name/Description are optional human-facing labels (see
  // state.Job.Name's doc comment) — always present as strings (possibly
  // empty), never undefined, since internal/state's SQLite columns
  // default to ''.
  Name: string;
  Description: string;
  Operation: string;
  OperationSummary: string;
  Statements: string[];
  // Present only when this migration is a SHADOW_TABLE job with an
  // active replication slot right now — absent (undefined), not null,
  // otherwise (mirrors the Go struct's `json:",omitempty"` tags).
  // ReplicationLagTrend is "unknown" for exactly one poll cycle right
  // after the slot is created (no prior reading to compare against
  // yet), then "growing" | "shrinking" | "stable".
  ReplicationLagBytes?: number;
  ReplicationLagTrend?: "growing" | "shrinking" | "stable" | "unknown";
  // Present only once lag has been CONTINUOUSLY growing (no intervening
  // stable/shrinking reading) for a few minutes straight — the UI's
  // signal to escalate from the routine "Growing" badge to an explicit
  // "may not converge" warning. See the Go field's own doc comment for
  // why this is advisory only, never an automatic-abort trigger — the
  // decision to actually stop a migration is left to the person
  // watching.
  ReplicationLagGrowingForSeconds?: number;
  // Present only for a TERMINAL job (see the Go struct's own doc
  // comment for why) — every relevant transient resource for the job's
  // strategy, whether confirmed gone (the healthy, common case) or
  // still lingering. Absent (undefined), not an empty array, when the
  // job isn't terminal yet.
  ResourceStatus?: ResourceStatus[];
  // Present (and true) only while a migration is actively running AND
  // PostgreSQL's checkpoints are currently being forced more often than
  // scheduled — an external, environmental signal (max_wal_size likely
  // undersized for current write volume), not something this migration
  // itself did. Absent otherwise — a plain, situational note rather than
  // a persistent status indicator (see the Go field's own doc comment).
  CheckpointPressureDetected?: boolean;
  // Present only when explicitly requested (see api.getMigration's
  // measureImpact parameter) — the one trust-layer indicator that's
  // opt-in rather than always-on, since the underlying query has real,
  // non-negligible cost unlike the others (see the Go field's own doc
  // comment). ImpactPeakQueryDurationSeconds is a running maximum across
  // the whole migration, not just the latest reading.
  ImpactActiveQueries?: number;
  ImpactPeakQueryDurationSeconds?: number;
}

// Mirrors internal/progress.ResourceStatus — a LIVE, directly-verified
// check, not a log entry. See MigrationReport.ResourceStatus's own doc
// comment for when this is populated.
export interface ResourceStatus {
  name: string;
  detail: string;
  exists: boolean;
}

// Mirrors internal/preview.Report.
export interface PreviewReport {
  SchemaName: string;
  TableName: string;
  Operation: string;
  Strategy: string;
  EstimatedRows: number;
  Statements: string[];
  Warnings: string[];
  Notes: string[];
}

export interface CurrentUser {
  id: string;
  email: string;
  role: Role;
}

export interface StartMigrationRequest {
  schema?: string;
  table: string;
  column?: string;
  operation: Operation;
  type?: string;
  default?: string;
  volatile_default?: boolean;
  strategy_override?: string;
  index_name?: string;
  constraint_name?: string;
  check_expression?: string;
  new_column_name?: string;
  name?: string;
  description?: string;
}

export interface ManagedUser {
  id: string;
  email: string;
  role: Role;
}

// Mirrors internal/catalog.ColumnInfo.
export interface ColumnInfo {
  Name: string;
  Type: string;
  Nullable: boolean;
  IsPrimaryKey: boolean;
  Default: string;
}

// Mirrors internal/strategy.TableStats.
export interface TableStats {
  SchemaName: string;
  TableName: string;
  EstimatedRowCount: number;
  IsPartitioned: boolean;
  HasPrimaryKey: boolean;
  ReplicaIdentity: string;
}

// Mirrors internal/catalog.SampleRowsResult.
export interface SampleRowsResult {
  Columns: string[];
  Rows: string[][];
}

// Mirrors internal/db.ConnectionInfo — deliberately has no password
// field, see that type's own doc comment for why.
export interface ConnectionInfo {
  Host: string;
  Port: number;
  Username: string;
  Database: string;
  // 0/"" (not present/loading) is a legitimate state — see the Go
  // struct's own doc comment for why these two can lag behind the other
  // fields.
  PostgresVersion: number;
  PostgresVersionString: string;
  // Mirrors internal/db.VersionSupportStatus's exact string values —
  // "" (unknown), "below_minimum", "supported", "newer_than_tested".
  // Computed server-side (see internal/db.ClassifyVersion) so this
  // screen never needs to duplicate the supported-version thresholds
  // itself.
  VersionSupportStatus: "" | "below_minimum" | "supported" | "newer_than_tested";
}

export interface SetupRequiredResponse {
  required: boolean;
}
