// Package strategy is the code counterpart of Architecture Doc v2 Section 4.0
// "Strategy Decision Matrix". It decides whether a requested DDL operation
// should go through the Direct DDL, Expand & Backfill, or Shadow Table +
// Logical Replication flow.
package strategy

import "fmt"

// Operation represents the schema change requested by the user.
type Operation string

const (
	OpAddColumn  Operation = "ADD_COLUMN"
	OpDropColumn Operation = "DROP_COLUMN"
	OpAlterType  Operation = "ALTER_COLUMN_TYPE"
	OpAlterOther Operation = "ALTER_COLUMN_OTHER"
	// OpAddIndex/OpDropIndex use PostgreSQL's own native CONCURRENTLY
	// mechanism (CREATE INDEX CONCURRENTLY / DROP INDEX CONCURRENTLY),
	// which doesn't take the long-lived, write-blocking lock a plain
	// CREATE/DROP INDEX would — that's PostgreSQL's own built-in
	// zero-downtime primitive for indexes, so neither operation needs the
	// Expand&Backfill or Shadow Table machinery this package built for
	// column changes. Both always resolve to StrategyDirectDDL below —
	// see engines/postgresql/ddlflow.executeAddIndex/executeDropIndex for why that
	// strategy name means "no shadow-table replication needed" here, not
	// literally "instant" (CONCURRENTLY index builds can take real time on
	// large tables, just without blocking writes while they do).
	OpAddIndex  Operation = "ADD_INDEX"
	OpDropIndex Operation = "DROP_INDEX"

	// OpSetNotNull/OpAddConstraint both use PostgreSQL's own "expand and
	// validate" pattern for constraints (available since PG 12): add the
	// constraint NOT VALID (instant, metadata-only), then VALIDATE
	// CONSTRAINT separately (a SHARE UPDATE EXCLUSIVE scan — non-blocking
	// for concurrent reads/writes, unlike the ACCESS EXCLUSIVE lock a
	// naive SET NOT NULL or ADD CONSTRAINT would hold for its own
	// verification scan). Like ADD_INDEX/DROP_INDEX, this is PostgreSQL's
	// own built-in zero-downtime mechanism, so neither ever needs
	// Expand&Backfill or Shadow Table.
	OpSetNotNull    Operation = "SET_NOT_NULL"
	OpAddConstraint Operation = "ADD_CONSTRAINT"

	// OpRenameColumn is NOT a plain ALTER TABLE ... RENAME COLUMN — that
	// statement is metadata-only and instant, but it breaks any
	// application code still using the old name IMMEDIATELY, which
	// defeats the whole point of a "zero-downtime" migration (the
	// downtime just moves from the database to every caller that hasn't
	// been redeployed yet). Instead this uses a real expand & contract
	// pattern: add a new column under the new name, keep both columns in
	// sync with a trigger, backfill existing data, and land in a
	// "dual-write" state where EITHER name works. See
	// engines/postgresql/ddlflow.executeRenameColumn's doc comment for the full
	// mechanism, and for why finishing the rename (dropping the old name)
	// is a deliberate, separate, later DROP_COLUMN migration rather than
	// something this operation does automatically.
	OpRenameColumn Operation = "RENAME_COLUMN"

	// OpRenameTable, like OpRenameColumn just above, deliberately does
	// NOT use a plain ALTER TABLE ... RENAME TO on its own — that
	// statement is metadata-only and instant, but ANY caller (a running
	// app instance not yet redeployed to the new name) querying the old
	// name would start failing immediately with "relation does not
	// exist". This is if anything a MORE severe version of
	// OpRenameColumn's exact problem, since most queries against a
	// table reference the table name, where a column rename might only
	// break queries touching that one column. Instead this renames the
	// table and leaves a compatibility VIEW under the OLD name selecting
	// from the new one — see engines/postgresql/ddlflow.executeRenameTable's doc
	// comment for why a plain "SELECT * FROM new_table" view is both
	// sufficient and, for the common case, automatically read/write
	// updatable by PostgreSQL itself with no extra machinery needed.
	// Like OpRenameColumn, dropping that compatibility view once every
	// caller has moved to the new name is a deliberate, separate,
	// later action this operation does not do automatically.
	OpRenameTable Operation = "RENAME_TABLE"

	// OpAddForeignKey uses the same NOT VALID + VALIDATE CONSTRAINT
	// pattern as OpAddConstraint above — PostgreSQL supports this for
	// foreign keys too, not just CHECK constraints, so this gets the
	// exact same "instant to add, non-blocking to validate" treatment.
	// See engines/postgresql/ddlflow.executeAddForeignKey's doc comment for the
	// full mechanism and for why the referenced column's own
	// UNIQUE/PRIMARY KEY requirement doesn't need separate validation
	// here — PostgreSQL enforces it natively with a clear error if it's
	// missing.
	OpAddForeignKey Operation = "ADD_FOREIGN_KEY"

	// OpAddGeneratedColumn is deliberately NOT metadata-only on a large
	// table the way ADD_COLUMN's constant-default path is — PostgreSQL
	// requires computing (and storing) every existing row's value for a
	// GENERATED ALWAYS AS (...) STORED column, which is a full table
	// rewrite under ACCESS EXCLUSIVE regardless of how "simple" the
	// expression is. See engines/postgresql/ddlflow.executeAddGeneratedColumn's
	// own doc comment for the two-path mechanism this uses to avoid
	// that rewrite on a large table: a real, native GENERATED column on
	// a small table (where the rewrite is cheap enough not to matter —
	// same reasoning as the small-table shortcut in Decide below), or a
	// plain column kept in sync by a BEFORE INSERT/UPDATE trigger plus
	// a batched backfill on a large one — behaviorally equivalent, but
	// not a true native GENERATED column (see that doc comment for what
	// this trade-off actually costs).
	OpAddGeneratedColumn Operation = "ADD_GENERATED_COLUMN"

	// OpPartitionTable converts an existing, regular table into a
	// partitioned one — the hardest operation this package supports.
	// PostgreSQL has no in-place way to do this: partitioning can only
	// be declared at CREATE TABLE time, never added to an existing
	// table via ALTER. This is why OpPartitionTable is the one
	// operation outside ALTER_COLUMN_TYPE's incompatible-cast case that
	// can require StrategyShadowTable — see
	// engines/postgresql/shadowflow.prepare's own doc comment for the mechanism:
	// a NEW, genuinely partitioned table is built alongside the
	// original, kept in sync via the same logical-replication pipeline
	// SHADOW_TABLE already uses for ALTER_COLUMN_TYPE, then swapped in
	// atomically. Everything downstream of table creation (dependent
	// objects, publication, replication slot, sync engine, validation,
	// swap) is reused unchanged; only the initial CREATE TABLE
	// statement differs.
	OpPartitionTable Operation = "PARTITION_TABLE"
)

// Strategy tells the orchestrator which flow to run.
type Strategy string

const (
	// StrategyDirectDDL: Architecture Doc 4.0 rows 1/3/4 — metadata-only, milliseconds.
	StrategyDirectDDL Strategy = "DIRECT_DDL"
	// StrategyExpandBackfill: Architecture Doc 4.0 row 2 — volatile default or computed backfill.
	StrategyExpandBackfill Strategy = "EXPAND_BACKFILL"
	// StrategyShadowTable: Architecture Doc 4.0 row 5 — incompatible type conversion, full table rewrite.
	StrategyShadowTable Strategy = "SHADOW_TABLE"
)

// TableStats carries the information the Strategy Selector collects via
// pg_class and pg_depend. Matches Architecture Doc Section 3.1 "Strategy
// Selector" and Section 4.1 "Preflight Check".
type TableStats struct {
	SchemaName        string
	TableName         string
	EstimatedRowCount int64  // pg_class.reltuples
	IsPartitioned     bool   // TR-12: partitioned tables are not supported
	HasPrimaryKey     bool   // REPLICA IDENTITY precondition (Section 3.2)
	ReplicaIdentity   string // "DEFAULT" | "FULL" | "NOTHING" | "INDEX"
}

// ColumnChange carries the details of the requested column change.
type ColumnChange struct {
	Operation                Operation
	ColumnName               string
	NewType                  string // for ALTER_COLUMN_TYPE
	DefaultValue             string // for ADD_COLUMN; empty means "no default"
	IsVolatileDefault        bool   // volatile expressions such as now(), random()
	TypeConversionCompatible bool   // e.g. varchar(50)->varchar(100) is compatible, text->integer is not

	// IndexName is used by ADD_INDEX/DROP_INDEX. For ADD_INDEX, an empty
	// value falls back to an auto-generated name (see
	// engines/postgresql/ddlflow.defaultIndexName); for DROP_INDEX it is required —
	// there's no column-based default to fall back to when dropping.
	IndexName string

	// ConstraintName is used by SET_NOT_NULL (optional — auto-generated
	// as "<table>_<column>_not_null_check" if empty) and ADD_CONSTRAINT
	// (required — there's no reasonable default name for an arbitrary
	// user-supplied check).
	ConstraintName string
	// CheckExpression is the raw CHECK(...) expression body for
	// ADD_CONSTRAINT (e.g. "price > 0"). Not used by SET_NOT_NULL, which
	// always checks "<column> IS NOT NULL" internally.
	CheckExpression string

	// NewColumnName is used ONLY by RENAME_COLUMN: ColumnName holds the
	// EXISTING (old) name, NewColumnName holds the name it's being
	// renamed to. Both are required.
	NewColumnName string

	// NewTableName is used ONLY by RENAME_TABLE — the table being
	// renamed is the request's own TableName (see
	// orchestrator.MigrationRequest), this field holds the name it's
	// being renamed to. Required; there is no column involved at all
	// for this operation (see NewMigration.tsx's isReadyForPreview,
	// which correctly doesn't require a Column field for this one
	// operation, unlike every other operation this package supports).
	NewTableName string

	// The fields below are used ONLY by ADD_FOREIGN_KEY. ColumnName
	// (already defined above) holds the LOCAL column the foreign key is
	// added to; ConstraintName (already defined above, shared with
	// ADD_CONSTRAINT) holds the new constraint's name.
	//
	// ReferencedTable/ReferencedColumn are both required — the table
	// and column the foreign key points at. Assumed to be in the SAME
	// schema as the table being modified; a genuinely cross-schema
	// foreign key isn't supported via a dedicated field in this
	// version.
	ReferencedTable  string
	ReferencedColumn string
	// OnDelete is optional — one of "CASCADE", "SET NULL", "SET
	// DEFAULT", "RESTRICT", "NO ACTION" (validated against exactly this
	// allow-list, not PostgreSQL's own general expression grammar,
	// since this is a closed set of literal keyword phrases, not an
	// arbitrary expression — see ValidateOnDeleteAction). Empty means
	// PostgreSQL's own default, NO ACTION.
	OnDelete string
	// GeneratedExpression is used ONLY by ADD_GENERATED_COLUMN — the
	// expression the new column's value is computed from (e.g. "price *
	// quantity"), evaluated against the OTHER columns of the same row.
	// ColumnName/NewType (already defined above) hold the new column's
	// name/type, same fields ADD_COLUMN uses. Subject to the same
	// SQL-injection blocklist as DefaultValue/CheckExpression — see
	// ValidateSQLExpression — since, like those two fields, it's
	// inlined directly into DDL text (PostgreSQL doesn't support
	// parameter binding inside ALTER TABLE), not a plain identifier.
	GeneratedExpression string

	// The four fields below are used ONLY by PARTITION_TABLE.
	//
	// PartitionColumn is the column values are partitioned on.
	// PartitionStrategy is "RANGE" or "LIST" (PostgreSQL's own two most
	// common partitioning strategies — HASH isn't supported by this
	// version, since it doesn't fit the "shrink one huge table by a
	// meaningful key" use case this operation targets).
	//
	// PartitionBoundsJSON is a JSON-encoded array of the ACTUAL,
	// EXPLICIT partition definitions — always fully expanded by the
	// time a request reaches this struct, REGARDLESS of whether the
	// caller originally specified them explicitly or via the
	// convenience rule-based generator (see ExpandPartitionRule) — a
	// deliberate design choice so engines/postgresql/ddlflow/engines/postgresql/shadowflow
	// never need to know "rules" exist at all, only ever handling one,
	// simpler, already-expanded shape. Each element has the shape
	// {"name": "...", "from": "...", "to": "..."} for RANGE or
	// {"name": "...", "values": ["...", "..."]} for LIST — see
	// PartitionBound.
	PartitionColumn     string
	PartitionStrategy   string
	PartitionBoundsJSON string
	// PartitionIncludeDefault adds a DEFAULT partition catching any row
	// that doesn't match one of the explicit bounds — PostgreSQL
	// requires either a DEFAULT partition or genuinely exhaustive
	// coverage; without one, an out-of-range/unlisted value at
	// migration time (or from a future write) would simply fail to
	// insert. Recommended unless the caller is certain their bounds are
	// exhaustive.
	PartitionIncludeDefault bool
}

// PartitionBound is one partition's boundary definition — see
// ColumnChange.PartitionBoundsJSON's own doc comment for the two JSON
// shapes this takes depending on PartitionStrategy. Name becomes the
// actual partition table's name (schema-qualified, quoted the same way
// every other identifier in this project is).
type PartitionBound struct {
	Name string `json:"name"`
	// RANGE only — PostgreSQL parses these contextually against the
	// partition column's actual type (a date, a number, etc.), so they
	// stay plain strings here rather than a typed Go value.
	From string `json:"from,omitempty"`
	To   string `json:"to,omitempty"`
	// LIST only — the explicit set of values this partition holds.
	Values []string `json:"values,omitempty"`
}

const smallTableRowThreshold = 1_000_000 // FR-01: < 1M rows -> small table

// Decide applies the Section 4.0 decision matrix.
// If override != "" (FR-02), the user override is returned as-is, but hard
// constraints such as partitioned tables and missing primary keys are still
// enforced.
// validStrategiesByOperation is the whitelist of strategies each
// operation can actually be executed under — the single source of truth
// both Decide's override validation and the API's exposed strategy
// matrix (see internal/api's handleStrategyMatrix) are built from, so
// they can never drift out of sync with each other.
//
// Why this exists — found via manual testing, not a theoretical concern:
// before this whitelist existed, StrategyOverride was accepted
// unconditionally for ANY operation (Decide only checked the
// PRIMARY KEY precondition, never whether the requested strategy's flow
// actually knows how to perform this operation at all). Forcing
// ADD_INDEX through SHADOW_TABLE, for example, silently did nothing
// useful: engines/postgresql/shadowflow has no ADD_INDEX-specific logic anywhere,
// so it just copied the entire table via CREATE TABLE ... LIKE ...
// INCLUDING ALL (missing the not-yet-existing new index by definition),
// replicated all 10M+ rows via logical replication (minutes of
// unnecessary load), swapped the table for an unchanged copy of itself,
// and reported COMPLETED — the requested index was never created, and
// nothing about the successful-looking result said so. A migration tool
// silently not doing what it claims to have done is close to the worst
// possible failure mode for a tool whose entire value proposition is
// "you can trust what this says happened."
var validStrategiesByOperation = map[Operation][]Strategy{
	OpAddColumn:          {StrategyDirectDDL, StrategyExpandBackfill},
	OpDropColumn:         {StrategyDirectDDL},
	OpAddIndex:           {StrategyDirectDDL},
	OpDropIndex:          {StrategyDirectDDL},
	OpSetNotNull:         {StrategyDirectDDL},
	OpAddConstraint:      {StrategyDirectDDL},
	OpRenameColumn:       {StrategyExpandBackfill},
	OpRenameTable:        {StrategyDirectDDL},
	OpAddForeignKey:      {StrategyDirectDDL},
	OpAddGeneratedColumn: {StrategyDirectDDL, StrategyExpandBackfill},
	OpPartitionTable:     {StrategyShadowTable},
	OpAlterType:          {StrategyDirectDDL, StrategyShadowTable},
}

// ValidStrategiesFor returns the strategies operation can actually be
// executed under — an empty/nil slice for an operation this package
// doesn't recognize at all (see the "unsupported operation" branch in
// Decide below).
func ValidStrategiesFor(operation Operation) []Strategy {
	return validStrategiesByOperation[operation]
}

// ValidStrategyMatrix returns the whole operation→strategies whitelist —
// used by the API to expose it in one response the frontend fetches
// once (this is static, compile-time-known domain knowledge, not
// something that depends on database state), rather than the frontend
// hardcoding its own copy that could silently drift out of sync with
// this package's actual enforcement.
func ValidStrategyMatrix() map[Operation][]Strategy {
	matrix := make(map[Operation][]Strategy, len(validStrategiesByOperation))
	for op, strategies := range validStrategiesByOperation {
		matrix[op] = append([]Strategy(nil), strategies...) // defensive copy — callers must not be able to mutate the package-level map
	}
	return matrix
}

// isValidStrategyFor reports whether strategy is in operation's
// whitelist.
func isValidStrategyFor(operation Operation, s Strategy) bool {
	for _, valid := range validStrategiesByOperation[operation] {
		if valid == s {
			return true
		}
	}
	return false
}

func Decide(stats TableStats, change ColumnChange, override Strategy) (Strategy, error) {
	if stats.IsPartitioned {
		return "", fmt.Errorf("partitioned tables are not supported (TR-12): %s.%s", stats.SchemaName, stats.TableName)
	}

	if override != "" {
		if !isValidStrategyFor(change.Operation, override) {
			return "", fmt.Errorf(
				"strategy override %q is not valid for operation %q — this flow has no logic for actually "+
					"performing this operation, forcing it would silently do nothing useful (see internal/strategy's "+
					"validStrategiesByOperation doc comment for a real incident this exact combination caused); "+
					"valid strategies for %s are: %v",
				override, change.Operation, change.Operation, ValidStrategiesFor(change.Operation),
			)
		}
		if override == StrategyShadowTable && !stats.HasPrimaryKey {
			return "", fmt.Errorf("shadow table strategy requires a PRIMARY KEY / REPLICA IDENTITY (Architecture Doc 3.2): %s.%s", stats.SchemaName, stats.TableName)
		}
		return override, nil
	}

	// PARTITION_TABLE is the one operation exempt from the small-table
	// shortcut just below — unlike every other operation here,
	// PostgreSQL has NO direct-DDL mechanism for converting a table
	// into a partitioned one at ANY size (see OpPartitionTable's own
	// doc comment), so this always needs the shadow-table + logical
	// replication mechanism, table size notwithstanding.
	if change.Operation == OpPartitionTable {
		if !stats.HasPrimaryKey {
			return "", fmt.Errorf("shadow table strategy requires a PRIMARY KEY / REPLICA IDENTITY (Architecture Doc 3.2): %s.%s", stats.SchemaName, stats.TableName)
		}
		return StrategyShadowTable, nil
	}

	// Small table: not worth the shadow-table overhead (Section 4.0, last row).
	if stats.EstimatedRowCount < smallTableRowThreshold {
		return StrategyDirectDDL, nil
	}

	switch change.Operation {
	case OpAddColumn:
		if change.DefaultValue == "" || !change.IsVolatileDefault {
			return StrategyDirectDDL, nil // metadata-only (PG 11+)
		}
		return StrategyExpandBackfill, nil

	case OpDropColumn:
		return StrategyDirectDDL, nil // PG defers physical deletion

	case OpAddIndex, OpDropIndex:
		// CONCURRENTLY is PostgreSQL's own zero-downtime mechanism for
		// index changes — no shadow-table replication is ever needed
		// regardless of table size, unlike ALTER_COLUMN_TYPE below.
		return StrategyDirectDDL, nil

	case OpSetNotNull, OpAddConstraint:
		// The NOT VALID + VALIDATE CONSTRAINT pattern is PostgreSQL's own
		// zero-downtime mechanism for constraints — same reasoning as
		// ADD_INDEX/DROP_INDEX above.
		return StrategyDirectDDL, nil

	case OpAddForeignKey:
		// Same NOT VALID + VALIDATE CONSTRAINT mechanism as
		// SET_NOT_NULL/ADD_CONSTRAINT just above — PostgreSQL supports
		// it for foreign keys too, so this needs neither
		// EXPAND_BACKFILL nor SHADOW_TABLE regardless of table size.
		return StrategyDirectDDL, nil

	case OpAddGeneratedColumn:
		// Unlike ADD_COLUMN, there is no "cheap, metadata-only" variant
		// here even for a large table — see OpAddGeneratedColumn's own
		// doc comment for why a STORED generated column always
		// requires computing every existing row's value. The
		// small-table shortcut above already handles the case where
		// that computation is cheap enough not to matter; this branch
		// only runs for tables where it genuinely isn't.
		return StrategyExpandBackfill, nil

	case OpRenameColumn:
		// Unlike ADD_INDEX/SET_NOT_NULL, this genuinely needs an
		// application-level batched backfill (syncing the new column from
		// the old one) rather than a PostgreSQL-native non-blocking
		// primitive — the same mechanism ADD_COLUMN's volatile-default
		// path uses, so it gets the same strategy label.
		return StrategyExpandBackfill, nil

	case OpRenameTable:
		// Unlike OpRenameColumn, this doesn't need row-by-row batched
		// sync at all — RENAME TO plus a single CREATE VIEW are both
		// instant, metadata-level operations regardless of table size,
		// so this needs neither EXPAND_BACKFILL's batching machinery nor
		// SHADOW_TABLE's logical replication.
		return StrategyDirectDDL, nil

	case OpAlterType:
		if change.TypeConversionCompatible {
			return StrategyDirectDDL, nil
		}
		if !stats.HasPrimaryKey {
			return "", fmt.Errorf("incompatible type conversion requires shadow-table but the table has no PRIMARY KEY: %s.%s", stats.SchemaName, stats.TableName)
		}
		return StrategyShadowTable, nil

	default:
		return "", fmt.Errorf("unsupported operation: %s", change.Operation)
	}
}
