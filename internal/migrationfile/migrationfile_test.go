package migrationfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidate_RequiresID(t *testing.T) {
	m := MigrationFile{Table: "orders", Operation: "ADD_COLUMN"}
	if err := m.Validate(); err == nil {
		t.Error("expected an error for a missing id")
	}
}

func TestValidate_RequiresTable(t *testing.T) {
	m := MigrationFile{ID: "001", Operation: "ADD_COLUMN"}
	if err := m.Validate(); err == nil {
		t.Error("expected an error for a missing table")
	}
}

func TestValidate_RequiresOperation(t *testing.T) {
	m := MigrationFile{ID: "001", Table: "orders"}
	if err := m.Validate(); err == nil {
		t.Error("expected an error for a missing operation")
	}
}

func TestValidate_AcceptsAMinimalValidMigration(t *testing.T) {
	m := MigrationFile{ID: "001", Table: "orders", Operation: "ADD_COLUMN"}
	if err := m.Validate(); err != nil {
		t.Errorf("expected no error for a minimal valid migration, got: %v", err)
	}
}

func TestToMigrationRequest_DefaultsSchemaToPublic(t *testing.T) {
	m := MigrationFile{ID: "001", Table: "orders", Operation: "ADD_COLUMN", Column: "status"}
	req := m.ToMigrationRequest("test-actor")
	if req.SchemaName != "public" {
		t.Errorf("expected SchemaName to default to \"public\", got %q", req.SchemaName)
	}
}

func TestToMigrationRequest_RespectsExplicitSchema(t *testing.T) {
	m := MigrationFile{ID: "001", Schema: "billing", Table: "orders", Operation: "ADD_COLUMN"}
	req := m.ToMigrationRequest("test-actor")
	if req.SchemaName != "billing" {
		t.Errorf("expected SchemaName=\"billing\", got %q", req.SchemaName)
	}
}

// TestToMigrationRequest_UsesIDAsJobName is the direct regression test
// for the idempotency mechanism this whole package exists to support —
// see MigrationFile.ID's own doc comment: the file's ID becomes the
// resulting job's Name, which is what a caller (see
// cmd/pgarchimigrator's AlreadyApplied) checks against existing jobs to
// decide whether this migration has already run.
func TestToMigrationRequest_UsesIDAsJobName(t *testing.T) {
	m := MigrationFile{ID: "20260826_add_status_column", Table: "orders", Operation: "ADD_COLUMN"}
	req := m.ToMigrationRequest("test-actor")
	if req.Name != "20260826_add_status_column" {
		t.Errorf("expected the job Name to be the migration's ID, got %q", req.Name)
	}
}

func TestToMigrationRequest_CopiesEveryField(t *testing.T) {
	m := MigrationFile{
		ID: "001", Description: "test description", Schema: "public", Table: "orders",
		Operation: "ADD_CONSTRAINT", Column: "amount", Type: "numeric", Default: "0",
		VolatileDefault: true, IndexName: "idx_test", ConstraintName: "check_amount",
		CheckExpression: "amount > 0", NewColumnName: "new_amount", StrategyOverride: "DIRECT_DDL",
	}
	req := m.ToMigrationRequest("test-actor")

	checks := []struct {
		name, want, got string
	}{
		{"Operation", "ADD_CONSTRAINT", string(req.Change.Operation)},
		{"ColumnName", "amount", req.Change.ColumnName},
		{"NewType", "numeric", req.Change.NewType},
		{"DefaultValue", "0", req.Change.DefaultValue},
		{"IndexName", "idx_test", req.Change.IndexName},
		{"ConstraintName", "check_amount", req.Change.ConstraintName},
		{"CheckExpression", "amount > 0", req.Change.CheckExpression},
		{"NewColumnName", "new_amount", req.Change.NewColumnName},
		{"StrategyOverride", "DIRECT_DDL", string(req.StrategyOverride)},
		{"Description", "test description", req.Description},
		{"Actor", "test-actor", req.Actor},
	}
	for _, c := range checks {
		if c.want != c.got {
			t.Errorf("%s: expected %q, got %q", c.name, c.want, c.got)
		}
	}
	if !req.Change.IsVolatileDefault {
		t.Error("expected IsVolatileDefault=true to be copied through")
	}
}

func writeTestFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatalf("could not write test fixture %s: %v", name, err)
	}
}

func TestLoadFile_ParsesAValidMigrationFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "001.json", `{
		"id": "001_add_status",
		"schema": "public",
		"table": "orders",
		"operation": "ADD_COLUMN",
		"column": "status",
		"type": "text",
		"default": "'active'"
	}`)

	m, err := LoadFile(filepath.Join(dir, "001.json"))
	if err != nil {
		t.Fatalf("LoadFile failed: %v", err)
	}
	if m.ID != "001_add_status" || m.Table != "orders" || m.Operation != "ADD_COLUMN" {
		t.Errorf("unexpected parsed migration: %+v", m)
	}
}

func TestLoadFile_RejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "broken.json", `{ not valid json`)

	if _, err := LoadFile(filepath.Join(dir, "broken.json")); err == nil {
		t.Error("expected an error for invalid JSON")
	}
}

func TestLoadFile_RejectsAMigrationMissingRequiredFields(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "incomplete.json", `{"description": "missing everything else"}`)

	if _, err := LoadFile(filepath.Join(dir, "incomplete.json")); err == nil {
		t.Error("expected a validation error for a migration missing required fields")
	}
}

func TestLoadFile_NonexistentFile_ReturnsError(t *testing.T) {
	if _, err := LoadFile("/does/not/exist.json"); err == nil {
		t.Error("expected an error for a nonexistent file")
	}
}

// TestLoadDir_SortsByFilename is the direct regression test for the
// ordering guarantee this whole feature depends on: migrations must
// apply in a predictable, filename-determined order (matching Flyway/
// golang-migrate's identical convention), not directory-listing order
// (which most filesystems don't guarantee is alphabetical).
func TestLoadDir_SortsByFilename(t *testing.T) {
	dir := t.TempDir()
	// Written deliberately out of order — LoadDir must still return
	// them sorted.
	writeTestFile(t, dir, "003_third.json", `{"id":"003","table":"t","operation":"ADD_COLUMN"}`)
	writeTestFile(t, dir, "001_first.json", `{"id":"001","table":"t","operation":"ADD_COLUMN"}`)
	writeTestFile(t, dir, "002_second.json", `{"id":"002","table":"t","operation":"ADD_COLUMN"}`)

	migrations, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir failed: %v", err)
	}
	if len(migrations) != 3 {
		t.Fatalf("expected 3 migrations, got %d", len(migrations))
	}
	if migrations[0].ID != "001" || migrations[1].ID != "002" || migrations[2].ID != "003" {
		t.Errorf("expected migrations sorted 001,002,003 by filename, got %s,%s,%s",
			migrations[0].ID, migrations[1].ID, migrations[2].ID)
	}
}

func TestLoadDir_IgnoresNonJSONFiles(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "001.json", `{"id":"001","table":"t","operation":"ADD_COLUMN"}`)
	writeTestFile(t, dir, "README.md", `# Migrations directory`)
	writeTestFile(t, dir, ".gitkeep", ``)

	migrations, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir failed: %v", err)
	}
	if len(migrations) != 1 {
		t.Fatalf("expected only the .json file to be loaded, got %d migrations", len(migrations))
	}
}

func TestLoadDir_EmptyDirectory_ReturnsEmptySliceNotError(t *testing.T) {
	dir := t.TempDir()
	migrations, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("expected no error for an empty directory, got: %v", err)
	}
	if len(migrations) != 0 {
		t.Errorf("expected 0 migrations, got %d", len(migrations))
	}
}

func TestLoadDir_PropagatesAParseErrorFromAnyFile(t *testing.T) {
	dir := t.TempDir()
	writeTestFile(t, dir, "001_good.json", `{"id":"001","table":"t","operation":"ADD_COLUMN"}`)
	writeTestFile(t, dir, "002_bad.json", `{ not valid json`)

	if _, err := LoadDir(dir); err == nil {
		t.Error("expected LoadDir to propagate the parse error from the broken file")
	}
}
