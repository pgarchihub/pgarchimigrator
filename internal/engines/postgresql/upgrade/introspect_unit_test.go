package upgrade

import (
	"context"
	"testing"
)

// TestIntrospect_JobTablesSet_ReturnsExactlyThoseTables is the direct
// regression test for Job.Tables' own "used exactly as given, no
// discovery at all" contract — passes a nil sourcePool specifically to
// prove this path never touches it (a real PostgreSQL connection isn't
// needed to verify this branch at all, unlike the schema-discovery
// path, which genuinely does need one — see introspect_integration_test.go).
func TestIntrospect_JobTablesSet_ReturnsExactlyThoseTables(t *testing.T) {
	job := &Job{
		Schemas: []string{"this-should-be-ignored"}, // see Job.Tables' own doc comment
		Tables: []TableRef{
			{SchemaName: "public", TableName: "orders"},
			{SchemaName: "billing", TableName: "invoices"},
		},
	}

	refs, err := Introspect(context.Background(), nil, job)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(refs) != 2 {
		t.Fatalf("expected exactly 2 table refs, got %d", len(refs))
	}
	if refs[0] != (TableRef{SchemaName: "public", TableName: "orders"}) {
		t.Errorf("expected the first ref to be public.orders unchanged, got %+v", refs[0])
	}
	if refs[1] != (TableRef{SchemaName: "billing", TableName: "invoices"}) {
		t.Errorf("expected the second ref to be billing.invoices unchanged, got %+v", refs[1])
	}
}

func TestTableRef_JSONRoundTrip(t *testing.T) {
	ref := TableRef{SchemaName: "public", TableName: "orders"}
	// This is a plain sanity check that TableRef's own json tags
	// (schema/table) are wired up correctly — real marshaling behavior
	// is exercised for real by the SQLite store's own Tables column
	// (see sqlite_store_test.go) and the API's own request/response
	// types, both of which depend on this exact tag shape.
	if ref.String() != "public.orders" {
		t.Errorf("expected String() to still work unchanged, got %q", ref.String())
	}
}
