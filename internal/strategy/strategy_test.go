package strategy

import "testing"

func TestDecide_SmallTable_AlwaysDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 500_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAlterType, TypeConversionCompatible: false}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL for small table, got %s", got)
	}
}

func TestDecide_LargeTable_IncompatibleTypeChange_RequiresShadowTable(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAlterType, TypeConversionCompatible: false}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyShadowTable {
		t.Errorf("expected ShadowTable, got %s", got)
	}
}

func TestDecide_LargeTable_ShadowTable_WithoutPrimaryKey_Fails(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: false}
	change := ColumnChange{Operation: OpAlterType, TypeConversionCompatible: false}

	_, err := Decide(stats, change, "")
	if err == nil {
		t.Fatal("expected an error for shadow-table without primary key")
	}
}

func TestDecide_PartitionedTable_AlwaysRejected(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 100, IsPartitioned: true}
	change := ColumnChange{Operation: OpAddColumn}

	_, err := Decide(stats, change, "")
	if err == nil {
		t.Fatal("expected an error for partitioned table")
	}
}

func TestDecide_VolatileDefault_UsesExpandBackfill(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 5_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAddColumn, DefaultValue: "now()", IsVolatileDefault: true}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyExpandBackfill {
		t.Errorf("expected ExpandBackfill, got %s", got)
	}
}

func TestDecide_UserOverride_IsRespected(t *testing.T) {
	// ADD_COLUMN's own natural decision (no volatile default) would be
	// DIRECT_DDL — this override forces EXPAND_BACKFILL instead, which
	// IS in ADD_COLUMN's whitelist (see validStrategiesByOperation),
	// unlike the SHADOW_TABLE override this test used before a real
	// incident (see that same doc comment) showed forcing an
	// operation through a strategy whose flow has no logic for it at
	// all silently does nothing useful.
	stats := TableStats{EstimatedRowCount: 5_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAddColumn}

	got, err := Decide(stats, change, StrategyExpandBackfill)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyExpandBackfill {
		t.Errorf("expected ExpandBackfill after override, got %s", got)
	}
}

// TestDecide_UserOverride_RejectsOperationStrategyMismatch is the direct
// regression test for a real bug found via manual testing: forcing
// ADD_INDEX through SHADOW_TABLE used to be silently accepted, then
// silently did nothing useful (internal/engines/postgresql/shadowflow has no ADD_INDEX
// logic at all — it just copied the whole table via CREATE TABLE ...
// LIKE ... INCLUDING ALL, replicated everything, and swapped for an
// unchanged copy, reporting COMPLETED without ever creating the
// requested index). See validStrategiesByOperation's doc comment for
// the full incident.
func TestDecide_UserOverride_RejectsOperationStrategyMismatch(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 5_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAddIndex}

	_, err := Decide(stats, change, StrategyShadowTable)
	if err == nil {
		t.Fatal("expected an error forcing ADD_INDEX through SHADOW_TABLE — this combination must be rejected, not silently accepted")
	}
}

// TestValidStrategiesFor_MatchesDecideForEveryOperation is a
// consistency check: for every operation this package recognizes, every
// strategy Decide's own automatic (non-override) decision could
// possibly reach must also appear in that operation's whitelist — this
// would fail loudly if a future change to Decide's automatic logic ever
// drifted out of sync with validStrategiesByOperation (e.g. someone adds
// a new automatic path to a strategy the whitelist doesn't know about
// for that operation, which would then make Decide contradict itself:
// accepting a strategy automatically while rejecting the exact same
// strategy if a caller passed it as an explicit override).
func TestValidStrategiesFor_MatchesDecideForEveryOperation(t *testing.T) {
	cases := []struct {
		operation      Operation
		change         ColumnChange
		wantAutoResult Strategy
	}{
		{OpAddColumn, ColumnChange{Operation: OpAddColumn}, StrategyDirectDDL},
		{OpAddColumn, ColumnChange{Operation: OpAddColumn, DefaultValue: "now()", IsVolatileDefault: true}, StrategyExpandBackfill},
		{OpDropColumn, ColumnChange{Operation: OpDropColumn}, StrategyDirectDDL},
		{OpAddIndex, ColumnChange{Operation: OpAddIndex}, StrategyDirectDDL},
		{OpDropIndex, ColumnChange{Operation: OpDropIndex}, StrategyDirectDDL},
		{OpSetNotNull, ColumnChange{Operation: OpSetNotNull}, StrategyDirectDDL},
		{OpAddConstraint, ColumnChange{Operation: OpAddConstraint}, StrategyDirectDDL},
		{OpRenameColumn, ColumnChange{Operation: OpRenameColumn}, StrategyExpandBackfill},
		{OpRenameTable, ColumnChange{Operation: OpRenameTable, NewTableName: "orders_v2"}, StrategyDirectDDL},
		{OpAddForeignKey, ColumnChange{Operation: OpAddForeignKey, ColumnName: "customer_id", ConstraintName: "fk_orders_customer", ReferencedTable: "customers", ReferencedColumn: "id"}, StrategyDirectDDL},
		{OpAddGeneratedColumn, ColumnChange{Operation: OpAddGeneratedColumn, ColumnName: "total", NewType: "numeric", GeneratedExpression: "price * quantity"}, StrategyExpandBackfill},
		{OpPartitionTable, ColumnChange{Operation: OpPartitionTable, PartitionColumn: "created_at", PartitionStrategy: "RANGE"}, StrategyShadowTable},
		{OpAlterType, ColumnChange{Operation: OpAlterType, TypeConversionCompatible: true}, StrategyDirectDDL},
		{OpAlterType, ColumnChange{Operation: OpAlterType, TypeConversionCompatible: false}, StrategyShadowTable},
	}
	bigTableStats := TableStats{EstimatedRowCount: 5_000_000, HasPrimaryKey: true}

	for _, tc := range cases {
		t.Run(string(tc.operation), func(t *testing.T) {
			got, err := Decide(bigTableStats, tc.change, "")
			if err != nil {
				t.Fatalf("Decide failed: %v", err)
			}
			if got != tc.wantAutoResult {
				t.Fatalf("Decide's automatic choice was %s, expected %s — test setup assumption is wrong, fix the test", got, tc.wantAutoResult)
			}
			if !isValidStrategyFor(tc.operation, got) {
				t.Errorf("Decide automatically chose %s for %s, but that strategy is NOT in validStrategiesByOperation's whitelist for this operation — "+
					"an override of the exact same strategy would be incorrectly rejected", got, tc.operation)
			}
		})
	}
}

// TestDecide_LargeTable_* below cover every operation added after the
// original decision matrix — a real gap found via a failing preview
// integration test that (incorrectly) assumed RENAME_COLUMN's strategy
// without ever exercising Decide()'s large-table path for it directly.
// EstimatedRowCount is set well above smallTableRowThreshold specifically
// so these tests reach the operation-specific switch below the small-table
// shortcut, rather than being masked by it (exactly what caused that
// failure).

func TestDecide_LargeTable_DropColumn_AlwaysDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpDropColumn}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL (PG defers physical deletion regardless of table size), got %s", got)
	}
}

func TestDecide_LargeTable_AddIndex_AlwaysDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAddIndex}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL (CONCURRENTLY is PostgreSQL's own zero-downtime mechanism), got %s", got)
	}
}

func TestDecide_LargeTable_DropIndex_AlwaysDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpDropIndex}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL, got %s", got)
	}
}

func TestDecide_LargeTable_SetNotNull_AlwaysDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpSetNotNull}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL (NOT VALID + VALIDATE CONSTRAINT is PostgreSQL's own zero-downtime mechanism), got %s", got)
	}
}

func TestDecide_LargeTable_AddConstraint_AlwaysDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAddConstraint}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL, got %s", got)
	}
}

func TestDecide_LargeTable_RenameColumn_UsesExpandBackfill(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpRenameColumn}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyExpandBackfill {
		t.Errorf("expected ExpandBackfill (RENAME_COLUMN genuinely needs an application-level batched backfill, unlike the CONCURRENTLY/NOT VALID operations above), got %s", got)
	}
}

// TestDecide_LargeTable_RenameTable_AlwaysDirectDDL is the direct
// regression test for a deliberate contrast with RenameColumn just
// above: unlike a column rename, a table rename (RENAME TO + a single
// compatibility VIEW — see OpRenameTable's own doc comment) never needs
// row-by-row batched backfill machinery, regardless of table size.
func TestDecide_LargeTable_RenameTable_AlwaysDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpRenameTable, NewTableName: "orders_v2"}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL (RENAME TO + CREATE VIEW are both instant, metadata-level operations regardless of table size), got %s", got)
	}
}

// TestDecide_LargeTable_AddForeignKey_AlwaysDirectDDL is the direct
// regression test for ADD_FOREIGN_KEY reusing ADD_CONSTRAINT's exact
// NOT VALID + VALIDATE CONSTRAINT strategy — PostgreSQL supports this
// pattern for foreign keys too, so this needs neither EXPAND_BACKFILL
// nor SHADOW_TABLE regardless of table size.
func TestDecide_LargeTable_AddForeignKey_AlwaysDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{
		Operation: OpAddForeignKey, ColumnName: "customer_id",
		ConstraintName: "fk_orders_customer", ReferencedTable: "customers", ReferencedColumn: "id",
	}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL, got %s", got)
	}
}

// TestDecide_SmallTable_AddGeneratedColumn_UsesDirectDDL confirms the
// small-table shortcut applies here too — a real, native GENERATED
// column via a plain ALTER TABLE ADD COLUMN, since the table rewrite
// PostgreSQL requires is cheap enough on a small table not to matter.
func TestDecide_SmallTable_AddGeneratedColumn_UsesDirectDDL(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 500_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAddGeneratedColumn, ColumnName: "total", NewType: "numeric", GeneratedExpression: "price * quantity"}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyDirectDDL {
		t.Errorf("expected DirectDDL for a small table, got %s", got)
	}
}

// TestDecide_LargeTable_AddGeneratedColumn_UsesExpandBackfill is the
// direct regression test for the real reason this operation needs its
// own dedicated case at all, unlike ADD_COLUMN: there is no
// metadata-only variant for a GENERATED STORED column on a large table
// — PostgreSQL always needs to compute and store every existing row's
// value, a full table rewrite regardless of table size once past the
// small-table shortcut.
func TestDecide_LargeTable_AddGeneratedColumn_UsesExpandBackfill(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpAddGeneratedColumn, ColumnName: "total", NewType: "numeric", GeneratedExpression: "price * quantity"}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyExpandBackfill {
		t.Errorf("expected ExpandBackfill for a large table, got %s", got)
	}
}

// TestDecide_SmallTable_PartitionTable_StillUsesShadowTable is the
// direct regression test for OpPartitionTable's exemption from the
// small-table shortcut — every OTHER operation gets cheap DirectDDL on
// a small table, but PostgreSQL has no direct-DDL mechanism for
// partitioning conversion at ANY size, so this must always use
// ShadowTable even here.
func TestDecide_SmallTable_PartitionTable_StillUsesShadowTable(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 500, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpPartitionTable, PartitionColumn: "created_at", PartitionStrategy: "RANGE"}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyShadowTable {
		t.Errorf("expected ShadowTable even for a small table (no direct-DDL mechanism exists for this operation at any size), got %s", got)
	}
}

func TestDecide_LargeTable_PartitionTable_UsesShadowTable(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: true}
	change := ColumnChange{Operation: OpPartitionTable, PartitionColumn: "created_at", PartitionStrategy: "RANGE"}

	got, err := Decide(stats, change, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != StrategyShadowTable {
		t.Errorf("expected ShadowTable, got %s", got)
	}
}

func TestDecide_PartitionTable_NoPrimaryKey_Fails(t *testing.T) {
	stats := TableStats{EstimatedRowCount: 10_000_000, HasPrimaryKey: false}
	change := ColumnChange{Operation: OpPartitionTable, PartitionColumn: "created_at", PartitionStrategy: "RANGE"}

	_, err := Decide(stats, change, "")
	if err == nil {
		t.Fatal("expected an error for a table without a primary key")
	}
}
