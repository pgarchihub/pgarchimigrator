// Package preview implements dry-run/preview support: showing a user what
// a migration WOULD do — which strategy would be chosen, which DDL
// statements would run, and (where cheap and safe) the result of
// read-only sanity checks against the real data — without ever executing
// any DDL or creating a state.Job.
//
// "Dry run" here means "no writes, no schema changes" — it does NOT mean
// "never touch the database". Read-only SELECT queries (e.g. counting
// existing NULLs before a SET_NOT_NULL, or counting rows that would
// violate a proposed CHECK constraint) are exactly what makes a preview
// genuinely useful rather than just a static string dump, so this package
// runs them deliberately. Nothing in this package ever calls Exec with
// anything other than a SELECT.
package preview

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
)

// Report is the full dry-run result for a single requested migration.
type Report struct {
	SchemaName    string
	TableName     string
	Operation     string
	Strategy      string
	EstimatedRows int64
	// Statements are the DDL statements this migration would execute, in
	// order. For SHADOW_TABLE-strategy ALTER_COLUMN_TYPE requests this is
	// intentionally empty — see buildAlterTypePreview's doc comment for why
	// that multi-step flow isn't reducible to a short statement list.
	Statements []string
	// Warnings flag conditions, found via real read-only queries against
	// the actual data, that would make the migration FAIL if run as-is
	// (e.g. existing NULL values that would fail a SET_NOT_NULL).
	Warnings []string
	// Notes are informational context that isn't a warning — e.g.
	// confirming a pre-check passed, or explaining a multi-step mechanism.
	Notes []string
}

// Render produces a plain-text, CLI-friendly rendering of the report —
// mirrors internal/progress.Report.Render's style for a consistent feel
// between "here's what would happen" and "here's what's happening".
func (r *Report) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Dry run: %s on %s.%s\n", r.Operation, r.SchemaName, r.TableName)
	fmt.Fprintf(&b, "Strategy: %s (~%d estimated row(s))\n", r.Strategy, r.EstimatedRows)

	if len(r.Statements) > 0 {
		b.WriteString("\nStatements that would run:\n")
		for i, stmt := range r.Statements {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, stmt)
		}
	}

	if len(r.Warnings) > 0 {
		b.WriteString("\nWarnings:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  ! %s\n", w)
		}
	}

	if len(r.Notes) > 0 {
		b.WriteString("\nNotes:\n")
		for _, n := range r.Notes {
			fmt.Fprintf(&b, "  - %s\n", n)
		}
	}

	b.WriteString("\nNo changes were made — this was a dry run.\n")
	return b.String()
}

// Generate builds a dry-run Report for the given request. It fetches
// table statistics and runs strategy.Decide() exactly as a real migration
// would (so the previewed strategy is never a guess), then builds an
// operation-specific preview. tableStats is the SAME
// orchestrator.TableStatsFetcher a real Orchestrator uses, passed in
// directly rather than reimplemented here, so preview and execution are
// guaranteed to see identical table statistics.
func Generate(ctx context.Context, pool *pgxpool.Pool, tableStats orchestrator.TableStatsFetcher, req orchestrator.MigrationRequest) (*Report, error) {
	stats, err := tableStats(ctx, req.SchemaName, req.TableName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch table statistics: %w", err)
	}
	stats.SchemaName = req.SchemaName
	stats.TableName = req.TableName

	strat, err := strategy.Decide(stats, req.Change, req.StrategyOverride)
	if err != nil {
		return nil, fmt.Errorf("failed to decide migration strategy: %w", err)
	}

	report := &Report{
		SchemaName:    req.SchemaName,
		TableName:     req.TableName,
		Operation:     string(req.Change.Operation),
		Strategy:      string(strat),
		EstimatedRows: stats.EstimatedRowCount,
		// Explicitly non-nil, even though every buildXxxPreview function
		// below only ever appends to these (never assigns): a real bug,
		// found via manual testing, was that most operations leave
		// Warnings (and sometimes Notes) with ZERO entries appended —
		// e.g. SET_NOT_NULL on already-clean data never appends a
		// warning, only a note — which left that field as Go's nil slice
		// zero value. encoding/json marshals a nil slice as JSON `null`,
		// and the frontend's `preview.Warnings.length > 0` crashed with
		// "Cannot read properties of null" the instant a preview like
		// that rendered — with no React error boundary anywhere in the
		// app, that blanked the ENTIRE screen, not just the preview
		// panel. Exactly the same class of bug already fixed once in
		// internal/catalog; missed here until now.
		Statements: []string{},
		Warnings:   []string{},
		Notes:      []string{},
	}

	switch req.Change.Operation {
	case strategy.OpAddColumn:
		buildAddColumnPreview(report, req)
	case strategy.OpDropColumn:
		buildDropColumnPreview(report, req)
	case strategy.OpAddIndex:
		buildAddIndexPreview(report, req)
	case strategy.OpDropIndex:
		if err := buildDropIndexPreview(ctx, pool, report, req); err != nil {
			return nil, err
		}
	case strategy.OpSetNotNull:
		if err := buildSetNotNullPreview(ctx, pool, report, req); err != nil {
			return nil, err
		}
	case strategy.OpAddConstraint:
		if err := buildAddConstraintPreview(ctx, pool, report, req); err != nil {
			return nil, err
		}
	case strategy.OpRenameColumn:
		buildRenameColumnPreview(report, req)
	case strategy.OpAlterType:
		buildAlterTypePreview(report, req, strat)
	default:
		report.Warnings = append(report.Warnings, fmt.Sprintf("no preview builder for operation %q — the strategy decision above is still accurate, but no statement preview is available", req.Change.Operation))
	}

	return report, nil
}

func qualifiedTable(schema, table string) string {
	return quoteIdent(schema) + "." + quoteIdent(table)
}

func buildAddColumnPreview(report *Report, req orchestrator.MigrationRequest) {
	change := req.Change
	qualified := qualifiedTable(req.SchemaName, req.TableName)

	if change.IsVolatileDefault {
		report.Statements = append(report.Statements,
			fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", qualified, quoteIdent(change.ColumnName), change.NewType),
		)
		report.Notes = append(report.Notes, fmt.Sprintf(
			"Volatile default detected (%s): the column is added as NULL, then backfilled in batches with %s = %s. This uses the Expand & Backfill strategy regardless of table size.",
			change.DefaultValue, change.ColumnName, change.DefaultValue))
		return
	}

	ddl := fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s", qualified, quoteIdent(change.ColumnName), change.NewType)
	if change.DefaultValue != "" {
		ddl += " DEFAULT " + change.DefaultValue
	}
	report.Statements = append(report.Statements, ddl)
	report.Notes = append(report.Notes, "Fixed (non-volatile) default: metadata-only on PostgreSQL 11+, effectively instant regardless of table size.")
}

func buildDropColumnPreview(report *Report, req orchestrator.MigrationRequest) {
	change := req.Change
	qualified := qualifiedTable(req.SchemaName, req.TableName)

	report.Statements = append(report.Statements,
		fmt.Sprintf("ALTER TABLE %s RENAME COLUMN %s TO __pgam_dropped_%s_<job-id>", qualified, quoteIdent(change.ColumnName), change.ColumnName),
	)
	report.Notes = append(report.Notes, fmt.Sprintf(
		"Two-phase soft-drop: %q becomes unreachable under its ORIGINAL name immediately — any application code still querying it by that name will error right away. The data itself is preserved and fully reversible via rollback during the default 10-minute window. %q is only PERMANENTLY, irreversibly deleted once that window expires without a rollback.",
		change.ColumnName, change.ColumnName))
}

func buildAddIndexPreview(report *Report, req orchestrator.MigrationRequest) {
	change := req.Change
	qualified := qualifiedTable(req.SchemaName, req.TableName)

	indexName := change.IndexName
	if indexName == "" {
		indexName = fmt.Sprintf("idx_%s_%s", req.TableName, change.ColumnName)
	}
	report.Statements = append(report.Statements,
		fmt.Sprintf("CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON %s (%s)", quoteIdent(indexName), qualified, quoteIdent(change.ColumnName)),
	)
	report.Notes = append(report.Notes, fmt.Sprintf(
		"CONCURRENTLY builds without blocking reads or writes, but can take noticeably longer than a plain CREATE INDEX — expect this to scale with the table's ~%d estimated row(s).",
		report.EstimatedRows))
}

func buildDropIndexPreview(ctx context.Context, pool *pgxpool.Pool, report *Report, req orchestrator.MigrationRequest) error {
	change := req.Change
	if change.IndexName == "" {
		return fmt.Errorf("DROP_INDEX requires an index name")
	}

	var def string
	query := `
		SELECT pg_get_indexdef(c.oid)
		FROM pg_class c
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2
	`
	err := pool.QueryRow(ctx, query, req.SchemaName, change.IndexName).Scan(&def)
	if err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("could not find index %q in %s.%s — it may not exist: %v", change.IndexName, req.SchemaName, req.TableName, err))
	} else {
		report.Notes = append(report.Notes, "Current definition (this is what would be captured for rollback, and is always safely recreatable at any time afterward): "+def)
	}

	report.Statements = append(report.Statements,
		fmt.Sprintf("DROP INDEX CONCURRENTLY IF EXISTS %s.%s", quoteIdent(req.SchemaName), quoteIdent(change.IndexName)),
	)
	return nil
}

func buildSetNotNullPreview(ctx context.Context, pool *pgxpool.Pool, report *Report, req orchestrator.MigrationRequest) error {
	change := req.Change
	qualified := qualifiedTable(req.SchemaName, req.TableName)

	constraintName := change.ConstraintName
	if constraintName == "" {
		constraintName = fmt.Sprintf("%s_%s_not_null_check", req.TableName, change.ColumnName)
	}

	report.Statements = append(report.Statements,
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s IS NOT NULL) NOT VALID", qualified, quoteIdent(constraintName), quoteIdent(change.ColumnName)),
		fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", qualified, quoteIdent(constraintName)),
		fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s SET NOT NULL", qualified, quoteIdent(change.ColumnName)),
		fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", qualified, quoteIdent(constraintName)),
	)

	var nullCount int64
	countQuery := fmt.Sprintf("SELECT count(*) FROM %s WHERE %s IS NULL", qualified, quoteIdent(change.ColumnName))
	if err := pool.QueryRow(ctx, countQuery).Scan(&nullCount); err != nil {
		return fmt.Errorf("failed to check for existing NULL values in %s: %w", change.ColumnName, err)
	}
	if nullCount > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"%d existing row(s) have NULL in %q — this migration WILL FAIL at the VALIDATE CONSTRAINT step unless those rows are fixed first",
			nullCount, change.ColumnName))
	} else {
		report.Notes = append(report.Notes, "No existing NULL values found in this column — validation should succeed.")
	}
	return nil
}

func buildAddConstraintPreview(ctx context.Context, pool *pgxpool.Pool, report *Report, req orchestrator.MigrationRequest) error {
	change := req.Change
	if change.ConstraintName == "" || change.CheckExpression == "" {
		return fmt.Errorf("ADD_CONSTRAINT requires both a constraint name and a check expression")
	}
	qualified := qualifiedTable(req.SchemaName, req.TableName)

	report.Statements = append(report.Statements,
		fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s CHECK (%s) NOT VALID", qualified, quoteIdent(change.ConstraintName), change.CheckExpression),
		fmt.Sprintf("ALTER TABLE %s VALIDATE CONSTRAINT %s", qualified, quoteIdent(change.ConstraintName)),
	)

	// WHERE NOT (<expr>) mirrors real CHECK constraint semantics exactly:
	// PostgreSQL only fails a row when the expression evaluates to FALSE
	// (a NULL result, e.g. from a NULL-able column, is treated as
	// satisfying the constraint) — and SQL's WHERE clause already excludes
	// NULL results the same way, so this count matches what
	// VALIDATE CONSTRAINT would actually reject.
	var violations int64
	checkQuery := fmt.Sprintf("SELECT count(*) FROM %s WHERE NOT (%s)", qualified, change.CheckExpression)
	if err := pool.QueryRow(ctx, checkQuery).Scan(&violations); err != nil {
		report.Warnings = append(report.Warnings, fmt.Sprintf("could not pre-check the expression against existing data (it may be invalid SQL): %v", err))
	} else if violations > 0 {
		report.Warnings = append(report.Warnings, fmt.Sprintf(
			"%d existing row(s) violate this check — this migration WILL FAIL at the VALIDATE CONSTRAINT step unless those rows are fixed first",
			violations))
	} else {
		report.Notes = append(report.Notes, "No existing rows violate this check — validation should succeed.")
	}
	return nil
}

func buildRenameColumnPreview(report *Report, req orchestrator.MigrationRequest) {
	change := req.Change
	qualified := qualifiedTable(req.SchemaName, req.TableName)

	report.Statements = append(report.Statements,
		fmt.Sprintf("ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s <same type as %s>", qualified, quoteIdent(change.NewColumnName), change.ColumnName),
		"CREATE OR REPLACE FUNCTION <dual-write sync trigger function>(...) — keeps both columns in sync",
		fmt.Sprintf("CREATE TRIGGER <sync trigger> BEFORE INSERT OR UPDATE ON %s FOR EACH ROW EXECUTE FUNCTION <...>()", qualified),
		fmt.Sprintf("UPDATE %s SET %s = %s WHERE %s IS NULL AND %s IS NOT NULL (batched)", qualified, quoteIdent(change.NewColumnName), quoteIdent(change.ColumnName), quoteIdent(change.NewColumnName), quoteIdent(change.ColumnName)),
	)
	report.Notes = append(report.Notes, fmt.Sprintf(
		"NOT a plain rename: this is a real expand & contract migration. It results in a \"dual-write\" state where BOTH %q and %q work simultaneously — legacy application code (using %q) and newly-deployed code (using %q) both keep working. Finishing the rename (permanently dropping %q) is a deliberate, separate, LATER DROP_COLUMN migration, not something this operation does automatically.",
		change.ColumnName, change.NewColumnName, change.ColumnName, change.NewColumnName, change.ColumnName))
}

// buildAlterTypePreview intentionally does NOT populate report.Statements
// for the SHADOW_TABLE case: that flow is a genuine multi-step pipeline
// (create shadow table, logical replication, atomic swap — Architecture
// Doc Section 4.1's 8 steps) with no single short statement list that
// honestly represents what will happen, unlike every other operation this
// package previews. Showing a truncated or misleading "preview SQL" would
// be worse than clearly explaining the mechanism instead.
func buildAlterTypePreview(report *Report, req orchestrator.MigrationRequest, strat strategy.Strategy) {
	change := req.Change
	qualified := qualifiedTable(req.SchemaName, req.TableName)

	if strat == strategy.StrategyDirectDDL {
		report.Statements = append(report.Statements,
			fmt.Sprintf("ALTER TABLE %s ALTER COLUMN %s TYPE %s USING %s::%s",
				qualified, quoteIdent(change.ColumnName), change.NewType, quoteIdent(change.ColumnName), change.NewType),
		)
		report.Notes = append(report.Notes, "Direct DDL: applied directly since the table is small and/or the type conversion was marked compatible.")
		return
	}

	report.Notes = append(report.Notes, strings.TrimSpace(`
Shadow Table strategy: creates a shadow copy of the table with the new
column type, replicates ongoing changes via PostgreSQL logical
replication, validates row-for-row via checksum, then performs an atomic
rename-based swap with automatic retry on lock timeout. No single short
statement list honestly represents this multi-step process — see
Architecture Doc Section 4.1 for the full sequence.`))
}

// quoteIdent is a simple SQL-injection guard for DDL statements where
// identifiers cannot be bound as parameters — same logic duplicated
// across internal/ddlflow, internal/reaper, internal/shadowflow; moving
// all of them to a shared internal/dbutil package is recommended later
// (TODO, noted in those packages too).
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
