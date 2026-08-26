// Package migrationfile implements "Migration as Code" — migrations
// defined as version-controlled JSON files that can be code-reviewed,
// diffed in a pull request, and applied idempotently via the CLI (see
// cmd/pgarchimigrator's apply-file/preview-file commands) or a CI
// pipeline (see .github/workflows/migration-preview.yml, which runs
// preview-file automatically against changed migration files on every
// PR and posts the result as a comment).
//
// Why this exists: without it, a migration only lives as one-off CLI
// flags or a web form submission — nothing about WHAT migrations a
// team has run, in what order, or why is captured anywhere reviewable
// alongside the application code changes that need them. A JSON file
// per migration, committed to the same repository as the schema
// changes it accompanies, closes that gap.
package migrationfile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
)

// MigrationFile is the JSON-file representation of a single migration.
// Field names deliberately mirror strategy.ColumnChange/
// orchestrator.MigrationRequest closely (snake_case JSON, matching the
// REST API's own request body — see internal/api's startMigrationRequest)
// so anyone already familiar with the web UI or API recognizes the
// shape immediately.
type MigrationFile struct {
	// ID uniquely identifies this migration for idempotency purposes —
	// see AlreadyApplied's doc comment for exactly how it's used. By
	// convention, prefix it with a sortable timestamp or number (e.g.
	// "20260826_add_status_column") so LoadDir's filename-based
	// ordering and a human skimming the ID both agree on sequence, but
	// this package does not enforce any particular format — it's
	// treated as an opaque, unique string.
	ID          string `json:"id"`
	Description string `json:"description,omitempty"`

	Schema    string `json:"schema"`
	Table     string `json:"table"`
	Operation string `json:"operation"`

	Column           string `json:"column,omitempty"`
	Type             string `json:"type,omitempty"`
	Default          string `json:"default,omitempty"`
	VolatileDefault  bool   `json:"volatile_default,omitempty"`
	IndexName        string `json:"index_name,omitempty"`
	ConstraintName   string `json:"constraint_name,omitempty"`
	CheckExpression  string `json:"check_expression,omitempty"`
	NewColumnName    string `json:"new_column_name,omitempty"`
	StrategyOverride string `json:"strategy_override,omitempty"`
}

// Validate reports whether m has the minimum fields required to attempt
// a migration at all. This is deliberately NOT a full check of every
// operation-specific required field (e.g. ADD_CONSTRAINT also needs
// ConstraintName/CheckExpression) — internal/strategy.Decide and the
// flows themselves already enforce those, and duplicating that logic
// here would just be a second place for the two to drift out of sync.
// This catches the class of error that would otherwise fail confusingly
// deep inside StartMigration: a missing ID (breaks idempotency
// tracking), or an empty Schema/Table/Operation (nothing to act on at
// all).
func (m MigrationFile) Validate() error {
	if strings.TrimSpace(m.ID) == "" {
		return fmt.Errorf("migration file is missing required field \"id\"")
	}
	if strings.TrimSpace(m.Table) == "" {
		return fmt.Errorf("migration %q is missing required field \"table\"", m.ID)
	}
	if strings.TrimSpace(m.Operation) == "" {
		return fmt.Errorf("migration %q is missing required field \"operation\"", m.ID)
	}
	return nil
}

// ToMigrationRequest converts m into the request shape
// orchestrator.StartMigration/preview.Generate actually consume. Schema
// defaults to "public" when omitted, matching the CLI's own --schema
// flag default. actor is passed through separately (see
// orchestrator.MigrationRequest.Actor's own doc comment) rather than
// being a file field — WHO applied a migration is a property of the
// applying context (a CI run, a specific operator), not something that
// belongs hardcoded into a version-controlled file everyone shares.
func (m MigrationFile) ToMigrationRequest(actor string) orchestrator.MigrationRequest {
	schema := m.Schema
	if schema == "" {
		schema = "public"
	}
	return orchestrator.MigrationRequest{
		SchemaName: schema,
		TableName:  m.Table,
		Change: strategy.ColumnChange{
			Operation:         strategy.Operation(m.Operation),
			ColumnName:        m.Column,
			NewType:           m.Type,
			DefaultValue:      m.Default,
			IsVolatileDefault: m.VolatileDefault,
			IndexName:         m.IndexName,
			ConstraintName:    m.ConstraintName,
			CheckExpression:   m.CheckExpression,
			NewColumnName:     m.NewColumnName,
		},
		StrategyOverride: strategy.Strategy(m.StrategyOverride),
		Actor:            actor,
		// The migration file's own ID becomes the job's Name — this is
		// the field AlreadyApplied (see cmd/pgarchimigrator) checks
		// against to decide whether this migration has already run.
		Name:        m.ID,
		Description: m.Description,
	}
}

// LoadFile reads and parses a single migration file.
func LoadFile(path string) (MigrationFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return MigrationFile{}, fmt.Errorf("failed to read %s: %w", path, err)
	}
	var m MigrationFile
	if err := json.Unmarshal(data, &m); err != nil {
		return MigrationFile{}, fmt.Errorf("failed to parse %s as a migration file: %w", path, err)
	}
	if err := m.Validate(); err != nil {
		return MigrationFile{}, fmt.Errorf("%s: %w", path, err)
	}
	return m, nil
}

// LoadDir reads every *.json file directly inside dir (not recursive —
// a flat directory of migration files is the convention, matching
// Flyway/golang-migrate and most other migration tools) and returns
// them sorted by FILENAME, so a numeric or timestamp prefix (e.g.
// "001_...", "20260826_...") determines apply order — the same
// principle those tools use, so a team already familiar with one of
// them needs no new mental model here.
func LoadDir(dir string) ([]MigrationFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations directory %s: %w", dir, err)
	}

	var filenames []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		filenames = append(filenames, e.Name())
	}
	sort.Strings(filenames)

	migrations := make([]MigrationFile, 0, len(filenames))
	for _, name := range filenames {
		m, err := LoadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		migrations = append(migrations, m)
	}
	return migrations, nil
}
