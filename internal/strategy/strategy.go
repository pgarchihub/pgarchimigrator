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
	// see internal/ddlflow.executeAddIndex/executeDropIndex for why that
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
	// internal/ddlflow.executeRenameColumn's doc comment for the full
	// mechanism, and for why finishing the rename (dropping the old name)
	// is a deliberate, separate, later DROP_COLUMN migration rather than
	// something this operation does automatically.
	OpRenameColumn Operation = "RENAME_COLUMN"
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
	// internal/ddlflow.defaultIndexName); for DROP_INDEX it is required —
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
// useful: internal/shadowflow has no ADD_INDEX-specific logic anywhere,
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
	OpAddColumn:     {StrategyDirectDDL, StrategyExpandBackfill},
	OpDropColumn:    {StrategyDirectDDL},
	OpAddIndex:      {StrategyDirectDDL},
	OpDropIndex:     {StrategyDirectDDL},
	OpSetNotNull:    {StrategyDirectDDL},
	OpAddConstraint: {StrategyDirectDDL},
	OpRenameColumn:  {StrategyExpandBackfill},
	OpAlterType:     {StrategyDirectDDL, StrategyShadowTable},
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

	case OpRenameColumn:
		// Unlike ADD_INDEX/SET_NOT_NULL, this genuinely needs an
		// application-level batched backfill (syncing the new column from
		// the old one) rather than a PostgreSQL-native non-blocking
		// primitive — the same mechanism ADD_COLUMN's volatile-default
		// path uses, so it gets the same strategy label.
		return StrategyExpandBackfill, nil

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
