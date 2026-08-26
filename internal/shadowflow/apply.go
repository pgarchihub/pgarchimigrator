// apply.go implements the "Apply Engine" component from Architecture Doc
// Section 3.2/4.1: it takes a decoded ChangeEvent and applies it to the
// shadow table. Every apply is idempotent (UPSERT keyed by primary key for
// INSERT/UPDATE, DELETE by primary key) — this is what makes it safe for
// Initial Sync and Delta Sync to overlap (see the design note in
// replication.go's CreateReplicationSlotAndGetStartLSN).
//
// Type handling note: pgoutput always delivers column values as
// text-encoded strings (see decoder.go's decodeTuple). These are bound as
// plain Go string query parameters without an explicit cast (except for
// CastColumn, see below). This relies on PostgreSQL's standard behavior of
// inferring an "unknown"-typed parameter's actual type from its context in
// the SQL statement — here, the target column of the INSERT — which
// correctly parses the text value as whatever type that column is
// (integer, timestamptz, boolean, etc.) for any ordinary, assignment-
// compatible case.
package shadowflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ApplyEngine applies decoded changes to a shadow table.
type ApplyEngine struct {
	Pool              *pgxpool.Pool
	Schema            string
	ShadowTable       string
	PrimaryKeyColumns []string

	// CastColumn/CastType handle the flagship shadow-table scenario from
	// Architecture Doc Section 4.0 — an incompatible type conversion (e.g.
	// text -> integer). PostgreSQL's plain assignment (a bare INSERT/UPDATE
	// with a text-formatted parameter) only succeeds for casts that are
	// "assignment-compatible"; an explicit-only cast like text->integer
	// requires the `::type` cast operator. When CastColumn is non-empty,
	// ApplyEngine emits `$N::<CastType>` for that column instead of a bare
	// `$N`, for both INSERT and UPDATE statements. If the source data isn't
	// actually convertible, PostgreSQL raises an error here — which is the
	// correct, desired behavior (surfacing a real data problem) rather than
	// silently corrupting the shadow table.
	CastColumn string
	CastType   string
}

// PrimaryKeyColumns fetches the ordered list of primary key column names
// for a table, via pg_constraint + pg_attribute (the same reliable
// mechanism used by internal/db.PgxPreflighter.checkPrimaryKey — the
// existence of a PK is already guaranteed by preflight for the
// shadow-table strategy; this just fetches the column NAMES).
func PrimaryKeyColumns(ctx context.Context, pool *pgxpool.Pool, schema, table string) ([]string, error) {
	qualifiedTable := fmt.Sprintf("%s.%s", schema, table)

	query := `
		SELECT a.attname
		FROM pg_constraint c
		JOIN unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		JOIN pg_attribute a ON a.attnum = k.attnum AND a.attrelid = c.conrelid
		WHERE c.conrelid = $1::regclass
		  AND c.contype = 'p'
		ORDER BY k.ord
	`
	rows, err := pool.Query(ctx, query, qualifiedTable)
	if err != nil {
		return nil, fmt.Errorf("failed to query primary key columns for %s: %w", qualifiedTable, err)
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("failed to scan primary key column: %w", err)
		}
		cols = append(cols, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cols) == 0 {
		return nil, fmt.Errorf("table %s has no primary key columns (this should have been rejected by preflight)", qualifiedTable)
	}
	return cols, nil
}

// Apply applies a single ChangeEvent to the shadow table.
func (a *ApplyEngine) Apply(ctx context.Context, event ChangeEvent) error {
	switch event.Kind {
	case ChangeInsert, ChangeUpdate:
		return a.upsert(ctx, event)
	case ChangeDelete:
		return a.delete(ctx, event)
	default:
		return fmt.Errorf("apply: unsupported change kind %q", event.Kind)
	}
}

// upsert builds and executes an `INSERT ... ON CONFLICT (pk) DO UPDATE SET
// ...` statement so that INSERT and UPDATE events are handled identically
// and idempotently — a change that has already been applied (e.g. via
// Initial Sync) is simply overwritten with the same or a newer value.
func (a *ApplyEngine) upsert(ctx context.Context, event ChangeEvent) error {
	if len(event.Columns) == 0 {
		return fmt.Errorf("apply: %s event for %s.%s has no columns to write", event.Kind, event.Schema, event.Table)
	}

	placeholders := make([]string, len(event.Columns))
	setClauses := make([]string, 0, len(event.Columns))
	pkSet := map[string]bool{}
	for _, pk := range a.PrimaryKeyColumns {
		pkSet[pk] = true
	}

	for i, col := range event.Columns {
		placeholders[i] = a.placeholderFor(col, i+1)
		if !pkSet[col] {
			setClauses = append(setClauses, fmt.Sprintf("%s = EXCLUDED.%s", quoteIdent(col), quoteIdent(col)))
		}
	}

	quotedCols := make([]string, len(event.Columns))
	for i, col := range event.Columns {
		quotedCols[i] = quoteIdent(col)
	}
	quotedPKs := make([]string, len(a.PrimaryKeyColumns))
	for i, pk := range a.PrimaryKeyColumns {
		quotedPKs[i] = quoteIdent(pk)
	}

	var sql string
	if len(setClauses) == 0 {
		// Every column in this event is part of the primary key — nothing to
		// update on conflict, so make the upsert a no-op on collision rather
		// than emitting invalid SQL with an empty SET clause.
		sql = fmt.Sprintf(
			"INSERT INTO %s.%s (%s) VALUES (%s) ON CONFLICT (%s) DO NOTHING",
			quoteIdent(a.Schema), quoteIdent(a.ShadowTable),
			strings.Join(quotedCols, ", "), strings.Join(placeholders, ", "),
			strings.Join(quotedPKs, ", "),
		)
	} else {
		sql = fmt.Sprintf(
			"INSERT INTO %s.%s (%s) VALUES (%s) ON CONFLICT (%s) DO UPDATE SET %s",
			quoteIdent(a.Schema), quoteIdent(a.ShadowTable),
			strings.Join(quotedCols, ", "), strings.Join(placeholders, ", "),
			strings.Join(quotedPKs, ", "), strings.Join(setClauses, ", "),
		)
	}

	if _, err := a.Pool.Exec(ctx, sql, event.Values...); err != nil {
		return fmt.Errorf("failed to apply %s for %s.%s: %w", event.Kind, event.Schema, event.Table, err)
	}
	return nil
}

// delete removes the row identified by the event's primary key columns.
func (a *ApplyEngine) delete(ctx context.Context, event ChangeEvent) error {
	pkValues := make([]any, 0, len(a.PrimaryKeyColumns))
	whereClauses := make([]string, 0, len(a.PrimaryKeyColumns))

	for _, pk := range a.PrimaryKeyColumns {
		idx := indexOfColumn(event.Columns, pk)
		if idx == -1 {
			return fmt.Errorf("apply: DELETE event for %s.%s is missing primary key column %q "+
				"(source table's REPLICA IDENTITY may not include the full primary key)", event.Schema, event.Table, pk)
		}
		pkValues = append(pkValues, event.Values[idx])
		whereClauses = append(whereClauses, fmt.Sprintf("%s = %s", quoteIdent(pk), a.placeholderFor(pk, len(pkValues))))
	}

	sql := fmt.Sprintf("DELETE FROM %s.%s WHERE %s",
		quoteIdent(a.Schema), quoteIdent(a.ShadowTable), strings.Join(whereClauses, " AND "))

	if _, err := a.Pool.Exec(ctx, sql, pkValues...); err != nil {
		return fmt.Errorf("failed to apply DELETE for %s.%s: %w", event.Schema, event.Table, err)
	}
	return nil
}

// placeholderFor returns "$N" normally, or "$N::<CastType>" for the one
// column undergoing an explicit type change (see the CastColumn/CastType
// doc comment above).
func (a *ApplyEngine) placeholderFor(column string, paramIndex int) string {
	if a.CastColumn != "" && column == a.CastColumn {
		return fmt.Sprintf("$%d::%s", paramIndex, a.CastType)
	}
	return fmt.Sprintf("$%d", paramIndex)
}

func indexOfColumn(columns []string, name string) int {
	for i, c := range columns {
		if c == name {
			return i
		}
	}
	return -1
}
