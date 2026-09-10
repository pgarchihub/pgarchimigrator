package upgrade

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/engines/postgresql/catalog"
)

// TableRef identifies one table in scope for an upgrade — the plain
// schema+name pair Introspect discovers and every other function in
// this package threads through.
type TableRef struct {
	SchemaName string `json:"schema"`
	TableName  string `json:"table"`
}

func (t TableRef) String() string {
	return t.SchemaName + "." + t.TableName
}

// Introspect enumerates every table in scope on the source instance —
// every table in job.Schemas, or every table in every non-system schema
// if job.Schemas is empty (see Job.Schemas' own doc comment for that
// default). System schemas (pg_catalog, information_schema, and
// PostgreSQL's own pg_toast) are always excluded regardless — an
// upgrade has no business touching PostgreSQL's own internal objects,
// and internal/engines/postgresql/catalog.ListSchemas already excludes them for the exact
// same reasoning this project's other introspection call sites rely on.
func Introspect(ctx context.Context, sourcePool *pgxpool.Pool, job *Job) ([]TableRef, error) {
	// job.Tables, when set, is used EXACTLY as given — no discovery at
	// all, and job.Schemas is ignored entirely. See Job.Tables' own doc
	// comment for why: this is the dashboard's own table-level scoping
	// path, where the caller (the checkbox picker, already expanded
	// client-side) already knows precisely which tables it wants.
	if len(job.Tables) > 0 {
		return job.Tables, nil
	}

	schemas := job.Schemas
	if len(schemas) == 0 {
		all, err := catalog.ListSchemas(ctx, sourcePool)
		if err != nil {
			return nil, fmt.Errorf("failed to list source schemas: %w", err)
		}
		schemas = all
	}

	var refs []TableRef
	for _, schema := range schemas {
		tables, err := catalog.ListTables(ctx, sourcePool, schema)
		if err != nil {
			return nil, fmt.Errorf("failed to list tables in schema %q: %w", schema, err)
		}
		for _, table := range tables {
			refs = append(refs, TableRef{SchemaName: schema, TableName: table})
		}
	}
	return refs, nil
}

// CreateTableOnTarget recreates one source table's structure on the
// target instance — introspects the EXACT column definitions via
// internal/engines/postgresql/catalog.ListColumns (the same introspection
// internal/engines/postgresql/shadowflow.createPartitionedShadowTable already established
// the pattern for, see that function's own doc comment) and issues an
// explicit CREATE TABLE, column by column.
//
// Deliberately does NOT use PostgreSQL's own CREATE TABLE ... (LIKE
// source INCLUDING ALL) the way internal/engines/postgresql/shadowflow's plain (non-
// partitioned) shadow tables do — LIKE only works within a single
// connection/instance; sourcePool and targetPool here are two entirely
// separate PostgreSQL instances (that's the whole point of an upgrade),
// so there is no single connection LIKE could even run against. Explicit
// column-by-column recreation is this package's only option, not a
// stylistic choice.
//
// Primary key and NOT NULL constraints are preserved (both needed for
// logical replication's own REPLICA IDENTITY requirements downstream —
// see CreatePublication's own doc comment); secondary indexes,
// non-PK constraints, and triggers are NOT recreated by this function —
// see the package doc comment's own scope notes for what's still ahead.
func CreateTableOnTarget(ctx context.Context, sourcePool, targetPool *pgxpool.Pool, ref TableRef) error {
	columns, err := catalog.ListColumns(ctx, sourcePool, ref.SchemaName, ref.TableName)
	if err != nil {
		return fmt.Errorf("failed to introspect %s: %w", ref, err)
	}
	if len(columns) == 0 {
		return fmt.Errorf("%s has no columns (or does not exist) on the source", ref)
	}

	var colDefs []string
	var pkColumns []string
	for _, c := range columns {
		def := quoteIdent(c.Name) + " " + c.Type
		if !c.Nullable {
			def += " NOT NULL"
		}
		if c.Default != "" {
			def += " DEFAULT " + c.Default
		}
		colDefs = append(colDefs, def)
		if c.IsPrimaryKey {
			pkColumns = append(pkColumns, quoteIdent(c.Name))
		}
	}
	if len(pkColumns) > 0 {
		colDefs = append(colDefs, fmt.Sprintf("PRIMARY KEY (%s)", strings.Join(pkColumns, ", ")))
	}

	schemaCreateSQL := fmt.Sprintf("CREATE SCHEMA IF NOT EXISTS %s", quoteIdent(ref.SchemaName))
	if _, err := targetPool.Exec(ctx, schemaCreateSQL); err != nil {
		return fmt.Errorf("failed to create schema %q on target: %w", ref.SchemaName, err)
	}

	tableCreateSQL := fmt.Sprintf("CREATE TABLE %s.%s (%s)",
		quoteIdent(ref.SchemaName), quoteIdent(ref.TableName), strings.Join(colDefs, ", "))
	if _, err := targetPool.Exec(ctx, tableCreateSQL); err != nil {
		if isDuplicateTableErr(err) {
			// A specific, actionable message for a specific, predictable
			// cause — see this function's own explanation below, rather
			// than surfacing PostgreSQL's own generic "already exists"
			// text unexplained. This is the single most likely failure
			// mode on a RETRY: an earlier attempt against the SAME
			// target got far enough to create this table before failing
			// at a LATER step (sync/validation) — Flow.Run has no way to
			// undo a table it already created on a genuinely separate
			// instance (there's no single cross-instance transaction
			// that could wrap "create the table" and "set up
			// replication" together), so a partial prior attempt
			// legitimately leaves this behind. A real bug report from
			// manual testing surfaced exactly this sequence.
			return fmt.Errorf(
				"%s already exists on the target — this usually means an EARLIER upgrade attempt against this same target got far enough to create it before failing at a later step. Drop it on the target (DROP TABLE %s.%s;) before retrying, or point this attempt at a fresh target instance: %w",
				ref, ref.SchemaName, ref.TableName, err,
			)
		}
		return fmt.Errorf("failed to create %s on target: %w", ref, err)
	}
	return nil
}

// isDuplicateTableErr reports whether err is PostgreSQL's own
// duplicate_table condition (SQLSTATE 42P07) — checked via pgconn's own
// structured PgError (errors.As), not a string match against the error
// text, so this stays correct regardless of PostgreSQL's own message
// wording across versions or locales.
func isDuplicateTableErr(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42P07"
	}
	return false
}

// quoteIdent applies simple escaping for identifiers in DDL statements
// — this package's own copy of the exact same helper
// internal/engines/postgresql/ddlflow/internal/engines/postgresql/shadowflow each already have; kept local
// rather than exported from one of those packages specifically because
// internal/engines/postgresql/upgrade has no other dependency on either (see the package
// doc comment for why this is a deliberately separate package), and a
// three-line identifier-quoting function isn't worth a cross-package
// dependency to avoid duplicating.
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
