// Package catalog implements read-only PostgreSQL catalog browsing —
// listing schemas, tables, columns, and a small sample of actual rows —
// used to populate the New Migration screen's schema/table/column
// dropdowns and table-overview panel (see internal/api's
// handleListSchemas/handleListTables/handleListColumns/handleSampleRows).
// Deliberately read-only: ListSchemas/ListTables/ListColumns never read a
// row of actual table DATA, only pg_catalog/information_schema. SampleRows
// is the one exception — see its own doc comment for the guardrails
// around that (LIMIT-bounded, read-only, identifier-escaped).
package catalog

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ListSchemas returns every non-system schema in the connected database,
// alphabetically. System schemas (pg_catalog, information_schema,
// pg_toast*, pg_temp*) are excluded — they're never valid migration
// targets and would just be noise in a schema picker.
//
// Always returns a non-nil slice (empty, not nil, when there are no
// results) — encoding/json marshals a nil Go slice as JSON `null`, which
// would crash a frontend doing `schemas.map(...)` on the response.
func ListSchemas(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	query := `
		SELECT schema_name FROM information_schema.schemata
		WHERE schema_name NOT IN ('pg_catalog', 'information_schema')
		  AND schema_name NOT LIKE 'pg_toast%'
		  AND schema_name NOT LIKE 'pg_temp%'
		ORDER BY schema_name
	`
	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to list schemas: %w", err)
	}
	defer rows.Close()

	schemas := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("failed to scan schema name: %w", err)
		}
		schemas = append(schemas, s)
	}
	return schemas, rows.Err()
}

// ListTables returns every ordinary table (BASE TABLE — not a view, not a
// system table) in the given schema, alphabetically. Always returns a
// non-nil slice — see ListSchemas' doc comment for why that matters.
func ListTables(ctx context.Context, pool *pgxpool.Pool, schema string) ([]string, error) {
	query := `
		SELECT table_name FROM information_schema.tables
		WHERE table_schema = $1 AND table_type = 'BASE TABLE'
		ORDER BY table_name
	`
	rows, err := pool.Query(ctx, query, schema)
	if err != nil {
		return nil, fmt.Errorf("failed to list tables in schema %q: %w", schema, err)
	}
	defer rows.Close()

	tables := []string{}
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, fmt.Errorf("failed to scan table name: %w", err)
		}
		tables = append(tables, t)
	}
	return tables, rows.Err()
}

// ColumnInfo is a single column's full definition, everything the New
// Migration screen's table-overview panel shows: its exact,
// fully-specified type (e.g. "character varying(50)", "numeric(10,2)" —
// the same format_type()-based technique used elsewhere in this project,
// see internal/ddlflow's columnType / internal/typecompat's
// CurrentColumnType), nullability, primary-key membership, and default
// expression.
type ColumnInfo struct {
	Name         string
	Type         string
	Nullable     bool
	IsPrimaryKey bool
	// Default is the column's default expression exactly as
	// pg_get_expr() renders it (e.g. "now()", "'active'::text"), or "" if
	// the column has no default at all — never a Go nil, so the frontend
	// can check it with a plain truthiness test.
	Default string
}

// ListColumns returns every live (non-dropped) column of the given table,
// in physical (attnum) order — matching the order `\d tablename` shows in
// psql, the order most familiar to a DBA. Always returns a non-nil
// slice — see ListSchemas' doc comment for why that matters.
func ListColumns(ctx context.Context, pool *pgxpool.Pool, schema, table string) ([]ColumnInfo, error) {
	query := `
		SELECT
			a.attname,
			format_type(a.atttypid, a.atttypmod),
			NOT a.attnotnull AS nullable,
			COALESCE(pg_get_expr(ad.adbin, ad.adrelid), '') AS default_expr,
			EXISTS (
				SELECT 1 FROM pg_index i
				WHERE i.indrelid = a.attrelid AND i.indisprimary AND a.attnum = ANY(i.indkey)
			) AS is_primary_key
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		LEFT JOIN pg_attrdef ad ON ad.adrelid = a.attrelid AND ad.adnum = a.attnum
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum
	`
	rows, err := pool.Query(ctx, query, schema, table)
	if err != nil {
		return nil, fmt.Errorf("failed to list columns of %s.%s: %w", schema, table, err)
	}
	defer rows.Close()

	columns := []ColumnInfo{}
	for rows.Next() {
		var c ColumnInfo
		if err := rows.Scan(&c.Name, &c.Type, &c.Nullable, &c.Default, &c.IsPrimaryKey); err != nil {
			return nil, fmt.Errorf("failed to scan column info: %w", err)
		}
		columns = append(columns, c)
	}
	return columns, rows.Err()
}

// SampleRowsResult is a small, read-only preview of a table's actual
// data — Columns gives the display order (matching SELECT *'s column
// order, which is usually but not guaranteed to match ListColumns'
// attnum order if the table has dropped columns in between), Rows is a
// list of rows, each cell already stringified (see SampleRows' doc
// comment for why).
type SampleRowsResult struct {
	Columns []string
	Rows    [][]string
}

// SampleRows returns up to `limit` rows from schema.table, exactly as
// `SELECT * ... LIMIT n` would — used by the New Migration screen so an
// operator can see real data shape before committing to a schema change,
// not just column names and types.
//
// Two things distinguish this from an ordinary read: (1) schema/table
// cannot be parameterized with $1/$2 the way ListTables/ListColumns' WHERE
// clauses are, because here they appear as an IDENTIFIER in the FROM
// clause, not as a compared VALUE — pgx.Identifier{}.Sanitize() is used
// instead, which quotes and escapes exactly like a well-behaved SQL
// client would, closing the SQL-injection risk that string-concatenating
// schema/table directly into the query would otherwise open (schema/table
// here are supplied by the API request, and while today's frontend only
// ever sends values it got back from ListSchemas/ListTables, the API
// itself doesn't and shouldn't assume every caller goes through that UI).
// (2) every cell is converted to its string representation rather than
// preserved as a native Go/JSON type — pgx's rows.Values() returns
// driver-native types (pgtype.Numeric, time.Time, []byte for bytea,
// etc.), several of which either don't round-trip cleanly through
// encoding/json or would render as something unhelpful for a human to
// glance at (bytea as base64, for instance) — a human-readable sample
// preview has no need to preserve exact types, only to look like what
// psql would show.
func SampleRows(ctx context.Context, pool *pgxpool.Pool, schema, table string, limit int) (*SampleRowsResult, error) {
	qualified := pgx.Identifier{schema, table}.Sanitize()
	query := fmt.Sprintf("SELECT * FROM %s LIMIT %d", qualified, limit)

	rows, err := pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to sample rows from %s.%s: %w", schema, table, err)
	}
	defer rows.Close()

	fieldDescs := rows.FieldDescriptions()
	columns := make([]string, len(fieldDescs))
	for i, fd := range fieldDescs {
		columns[i] = string(fd.Name)
	}

	result := &SampleRowsResult{Columns: columns, Rows: [][]string{}}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, fmt.Errorf("failed to read sample row from %s.%s: %w", schema, table, err)
		}
		strRow := make([]string, len(vals))
		for i, v := range vals {
			if v == nil {
				strRow[i] = "NULL"
			} else {
				strRow[i] = fmt.Sprintf("%v", v)
			}
		}
		result.Rows = append(result.Rows, strRow)
	}
	return result, rows.Err()
}
