package api

import (
	"encoding/json"
	"testing"

	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
)

// TestBuildMigrationRequest_RenameTable_RequiresNewTableName is the
// direct regression test for a real bug class: RENAME_TABLE is the one
// operation this package supports that does NOT need a Column at all
// (it acts on the table itself — see strategy.ColumnChange.NewTableName's
// own doc comment). Without an explicit case for it, this operation
// would fall into buildMigrationRequest's default branch, which
// requires Column — silently blocking every valid RENAME_TABLE request
// with a confusing "column is required" error that has nothing to do
// with the actual problem.
func TestBuildMigrationRequest_RenameTable_RequiresNewTableName(t *testing.T) {
	_, err := buildMigrationRequest(startMigrationRequest{
		Table:     "orders",
		Operation: "RENAME_TABLE",
		// NewTableName deliberately left empty.
	})
	if err == nil {
		t.Fatal("expected an error for a missing new_table_name")
	}
}

func TestBuildMigrationRequest_RenameTable_DoesNotRequireColumn(t *testing.T) {
	req, err := buildMigrationRequest(startMigrationRequest{
		Table:        "orders",
		Operation:    "RENAME_TABLE",
		NewTableName: "orders_v2",
		// Column deliberately left empty — must NOT be required for
		// this operation.
	})
	if err != nil {
		t.Fatalf("expected no error for a valid RENAME_TABLE request with no column, got: %v", err)
	}
	if req.Change.NewTableName != "orders_v2" {
		t.Errorf("expected NewTableName to be copied through, got %q", req.Change.NewTableName)
	}
}

func TestBuildMigrationRequest_RenameTable_DefaultsSchemaToPublic(t *testing.T) {
	req, err := buildMigrationRequest(startMigrationRequest{
		Table:        "orders",
		Operation:    "RENAME_TABLE",
		NewTableName: "orders_v2",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.SchemaName != "public" {
		t.Errorf("expected SchemaName to default to \"public\", got %q", req.SchemaName)
	}
}

func TestBuildMigrationRequest_AddForeignKey_RequiresConstraintNameColumnAndReferencedFields(t *testing.T) {
	cases := []struct {
		name string
		req  startMigrationRequest
	}{
		{"missing column", startMigrationRequest{Table: "orders", Operation: "ADD_FOREIGN_KEY", ConstraintName: "fk_x", ReferencedTable: "customers", ReferencedColumn: "id"}},
		{"missing constraint_name", startMigrationRequest{Table: "orders", Operation: "ADD_FOREIGN_KEY", Column: "customer_id", ReferencedTable: "customers", ReferencedColumn: "id"}},
		{"missing referenced_table", startMigrationRequest{Table: "orders", Operation: "ADD_FOREIGN_KEY", Column: "customer_id", ConstraintName: "fk_x", ReferencedColumn: "id"}},
		{"missing referenced_column", startMigrationRequest{Table: "orders", Operation: "ADD_FOREIGN_KEY", Column: "customer_id", ConstraintName: "fk_x", ReferencedTable: "customers"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := buildMigrationRequest(c.req); err == nil {
				t.Errorf("expected an error for %s", c.name)
			}
		})
	}
}

func TestBuildMigrationRequest_AddForeignKey_ValidRequest_CopiesEveryField(t *testing.T) {
	req, err := buildMigrationRequest(startMigrationRequest{
		Table: "orders", Operation: "ADD_FOREIGN_KEY", Column: "customer_id",
		ConstraintName: "fk_orders_customer", ReferencedTable: "customers", ReferencedColumn: "id",
		OnDelete: "CASCADE",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Change.ReferencedTable != "customers" || req.Change.ReferencedColumn != "id" || req.Change.OnDelete != "CASCADE" {
		t.Errorf("expected all foreign-key fields to be copied through, got %+v", req.Change)
	}
}

// TestBuildMigrationRequest_AddForeignKey_InvalidOnDelete_Rejected is the
// direct regression test for buildMigrationRequest's own reasoning
// section — this isn't currently validated at this layer (only at
// internal/orchestrator.StartMigration and internal/engines/postgresql/ddlflow's execute
// functions), documenting that gap explicitly rather than silently
// assuming coverage exists here too.
func TestBuildMigrationRequest_AddForeignKey_InvalidOnDelete_NotRejectedAtThisLayer(t *testing.T) {
	_, err := buildMigrationRequest(startMigrationRequest{
		Table: "orders", Operation: "ADD_FOREIGN_KEY", Column: "customer_id",
		ConstraintName: "fk_x", ReferencedTable: "customers", ReferencedColumn: "id",
		OnDelete: "DROP TABLE users", // invalid — validated downstream in orchestrator/ddlflow, not here
	})
	if err != nil {
		t.Fatalf("buildMigrationRequest itself doesn't validate on_delete's content (only internal/orchestrator.StartMigration does) — got an unexpected error: %v", err)
	}
}

func TestBuildMigrationRequest_AddGeneratedColumn_RequiresColumnTypeAndExpression(t *testing.T) {
	cases := []struct {
		name string
		req  startMigrationRequest
	}{
		{"missing column", startMigrationRequest{Table: "orders", Operation: "ADD_GENERATED_COLUMN", Type: "numeric", GeneratedExpression: "price * quantity"}},
		{"missing type", startMigrationRequest{Table: "orders", Operation: "ADD_GENERATED_COLUMN", Column: "total", GeneratedExpression: "price * quantity"}},
		{"missing generated_expression", startMigrationRequest{Table: "orders", Operation: "ADD_GENERATED_COLUMN", Column: "total", Type: "numeric"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := buildMigrationRequest(c.req); err == nil {
				t.Errorf("expected an error for %s", c.name)
			}
		})
	}
}

func TestBuildMigrationRequest_AddGeneratedColumn_ValidRequest_CopiesEveryField(t *testing.T) {
	req, err := buildMigrationRequest(startMigrationRequest{
		Table: "orders", Operation: "ADD_GENERATED_COLUMN", Column: "total",
		Type: "numeric", GeneratedExpression: "price * quantity",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Change.NewType != "numeric" || req.Change.GeneratedExpression != "price * quantity" {
		t.Errorf("expected type/expression to be copied through, got %+v", req.Change)
	}
}

func TestBuildMigrationRequest_PartitionTable_RequiresColumnAndValidStrategy(t *testing.T) {
	cases := []struct {
		name string
		req  startMigrationRequest
	}{
		{"missing partition_column", startMigrationRequest{Table: "orders", Operation: "PARTITION_TABLE", PartitionStrategy: "RANGE", PartitionBounds: []strategy.PartitionBound{{Name: "p1", From: "a", To: "b"}}}},
		{"invalid partition_strategy", startMigrationRequest{Table: "orders", Operation: "PARTITION_TABLE", PartitionColumn: "created_at", PartitionStrategy: "HASH", PartitionBounds: []strategy.PartitionBound{{Name: "p1", From: "a", To: "b"}}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := buildMigrationRequest(c.req); err == nil {
				t.Errorf("expected an error for %s", c.name)
			}
		})
	}
}

func TestBuildMigrationRequest_PartitionTable_ListRequiresExplicitBounds(t *testing.T) {
	// LIST has no rule-based shortcut — see ExpandPartitionRule's own
	// doc comment for why. A LIST request with no explicit bounds and
	// no way to generate them must fail clearly, not silently produce
	// zero partitions.
	_, err := buildMigrationRequest(startMigrationRequest{
		Table: "orders", Operation: "PARTITION_TABLE", PartitionColumn: "region", PartitionStrategy: "LIST",
	})
	if err == nil {
		t.Fatal("expected an error for LIST partitioning with no explicit bounds")
	}
}

func TestBuildMigrationRequest_PartitionTable_ExplicitBounds_EncodedCorrectly(t *testing.T) {
	req, err := buildMigrationRequest(startMigrationRequest{
		Table: "orders", Operation: "PARTITION_TABLE", PartitionColumn: "region", PartitionStrategy: "LIST",
		PartitionBounds: []strategy.PartitionBound{
			{Name: "orders_eu", Values: []string{"DE", "FR"}},
			{Name: "orders_us", Values: []string{"US"}},
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded []strategy.PartitionBound
	if err := json.Unmarshal([]byte(req.Change.PartitionBoundsJSON), &decoded); err != nil {
		t.Fatalf("PartitionBoundsJSON did not round-trip as valid JSON: %v", err)
	}
	if len(decoded) != 2 || decoded[0].Name != "orders_eu" {
		t.Errorf("expected the explicit bounds to be preserved, got %+v", decoded)
	}
}

// TestBuildMigrationRequest_PartitionTable_RuleBasedShortcut_ExpandsCorrectly
// is the direct regression test for the convenience rule-based path
// actually working end to end: no explicit partition_bounds, only an
// interval/from/to rule, must still produce a correctly-populated
// PartitionBoundsJSON.
func TestBuildMigrationRequest_PartitionTable_RuleBasedShortcut_ExpandsCorrectly(t *testing.T) {
	req, err := buildMigrationRequest(startMigrationRequest{
		Table: "orders", Operation: "PARTITION_TABLE", PartitionColumn: "created_at", PartitionStrategy: "RANGE",
		PartitionInterval: "monthly", PartitionRuleFrom: "2024-01-01", PartitionRuleTo: "2024-04-01",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var decoded []strategy.PartitionBound
	if err := json.Unmarshal([]byte(req.Change.PartitionBoundsJSON), &decoded); err != nil {
		t.Fatalf("PartitionBoundsJSON did not round-trip as valid JSON: %v", err)
	}
	if len(decoded) != 3 {
		t.Fatalf("expected 3 monthly partitions from the rule, got %d: %+v", len(decoded), decoded)
	}
}

func TestBuildMigrationRequest_PartitionTable_InvalidRule_Rejected(t *testing.T) {
	_, err := buildMigrationRequest(startMigrationRequest{
		Table: "orders", Operation: "PARTITION_TABLE", PartitionColumn: "created_at", PartitionStrategy: "RANGE",
		PartitionInterval: "weekly", PartitionRuleFrom: "2024-01-01", PartitionRuleTo: "2024-04-01", // "weekly" isn't a supported interval
	})
	if err == nil {
		t.Fatal("expected an error for an invalid partition rule interval")
	}
}
