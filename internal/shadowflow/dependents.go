// dependents.go implements Architecture Doc Section 4.3 "Dependent Object
// Migration Plan": objects that `CREATE TABLE ... LIKE ... INCLUDING ALL`
// does NOT copy.
//
// These fall into two categories with very different timing requirements:
//
//   - Category A — objects that live ON the shadow table itself and simply
//     don't exist there at all until created: triggers (kept DISABLED
//     until after swap, per Section 4.3.1), RLS policies, GRANTs, and
//     sequence ownership. These are created once, early, during
//     Preparation — see ApplyToShadowTable.
//   - Category B — objects defined on OTHER tables that reference this one
//     by OID (foreign keys, views): a rename-swap changes which physical
//     table owns the public name, so anything still pointing at the old
//     OID goes stale. These must be dropped and recreated (by name, so
//     they resolve to the new object) immediately AFTER a successful swap
//     — see ReattachAfterSwap.
package shadowflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DependentObjects is the inventory collected by Inventory.
type DependentObjects struct {
	ForeignKeys        []ForeignKeyDef
	Triggers           []TriggerDef
	SequenceOwnerships []SequenceOwnership
	DependentViews     []ViewDef
	RLSEnabled         bool
	RLSForced          bool
	RLSPolicies        []RLSPolicyDef
	Grants             []GrantDef
}

// ForeignKeyDef is a foreign key defined on ANOTHER table that references
// the table being migrated (Category B).
type ForeignKeyDef struct {
	ConstraintName string
	SourceSchema   string
	SourceTable    string
	Definition     string // pg_get_constraintdef output, e.g. "FOREIGN KEY (order_id) REFERENCES orders(id)"
}

// TriggerDef is a user-defined trigger on the table being migrated
// (Category A). Definition is the full pg_get_triggerdef output, which
// includes the "ON <table>" clause referencing the SOURCE table by
// name — recreateOnShadowTable rewrites that clause to target the shadow
// table instead of blindly replaying it.
type TriggerDef struct {
	Name       string
	Definition string
}

// SequenceOwnership represents a classic SERIAL-style column (IDENTITY
// columns are excluded — those are already handled by `INCLUDING ALL`).
type SequenceOwnership struct {
	SequenceSchema string
	SequenceName   string
	ColumnName     string
}

// ViewDef is a view whose query depends on the table being migrated
// (Category B).
type ViewDef struct {
	Schema         string
	Name           string
	IsMaterialized bool
	Definition     string // pg_get_viewdef output (the SELECT body only)
}

// RLSPolicyDef is a row-level security policy on the table being migrated
// (Category A).
type RLSPolicyDef struct {
	Name       string
	Permissive bool
	Roles      []string
	Command    string // ALL, SELECT, INSERT, UPDATE, DELETE
	UsingExpr  *string
	CheckExpr  *string
}

// GrantDef is a non-owner privilege grant on the table being migrated
// (Category A). `CREATE TABLE ... LIKE ...` does not copy grants at all —
// the new table starts with only the default privileges of its owner.
type GrantDef struct {
	Grantee   string
	Privilege string
}

// Inventory collects every dependent object for schema.table, per
// Architecture Doc Section 4.1 step 0 / Section 4.3. It should be called
// once, right after preflight passes and before Preparation creates the
// shadow table, so ShadowFlow.Execute has the full picture before doing
// any DDL.
func Inventory(ctx context.Context, pool *pgxpool.Pool, schema, table string) (*DependentObjects, error) {
	qualifiedTable := fmt.Sprintf("%s.%s", schema, table)
	deps := &DependentObjects{}

	fks, err := fetchForeignKeys(ctx, pool, qualifiedTable)
	if err != nil {
		return nil, fmt.Errorf("failed to inventory foreign keys: %w", err)
	}
	deps.ForeignKeys = fks

	triggers, err := fetchTriggers(ctx, pool, qualifiedTable)
	if err != nil {
		return nil, fmt.Errorf("failed to inventory triggers: %w", err)
	}
	deps.Triggers = triggers

	seqs, err := fetchSequenceOwnerships(ctx, pool, qualifiedTable)
	if err != nil {
		return nil, fmt.Errorf("failed to inventory sequence ownerships: %w", err)
	}
	deps.SequenceOwnerships = seqs

	views, err := fetchDependentViews(ctx, pool, qualifiedTable)
	if err != nil {
		return nil, fmt.Errorf("failed to inventory dependent views: %w", err)
	}
	deps.DependentViews = views

	rlsEnabled, rlsForced, err := fetchRLSStatus(ctx, pool, qualifiedTable)
	if err != nil {
		return nil, fmt.Errorf("failed to inventory RLS status: %w", err)
	}
	deps.RLSEnabled = rlsEnabled
	deps.RLSForced = rlsForced

	policies, err := fetchRLSPolicies(ctx, pool, schema, table)
	if err != nil {
		return nil, fmt.Errorf("failed to inventory RLS policies: %w", err)
	}
	deps.RLSPolicies = policies

	grants, err := fetchGrants(ctx, pool, schema, table)
	if err != nil {
		return nil, fmt.Errorf("failed to inventory grants: %w", err)
	}
	deps.Grants = grants

	return deps, nil
}

func fetchForeignKeys(ctx context.Context, pool *pgxpool.Pool, qualifiedTable string) ([]ForeignKeyDef, error) {
	rows, err := pool.Query(ctx, `
		SELECT con.conname, ns.nspname, cls.relname, pg_get_constraintdef(con.oid)
		FROM pg_constraint con
		JOIN pg_class cls ON cls.oid = con.conrelid
		JOIN pg_namespace ns ON ns.oid = cls.relnamespace
		WHERE con.contype = 'f' AND con.confrelid = $1::regclass
	`, qualifiedTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var fks []ForeignKeyDef
	for rows.Next() {
		var fk ForeignKeyDef
		if err := rows.Scan(&fk.ConstraintName, &fk.SourceSchema, &fk.SourceTable, &fk.Definition); err != nil {
			return nil, err
		}
		fks = append(fks, fk)
	}
	return fks, rows.Err()
}

func fetchTriggers(ctx context.Context, pool *pgxpool.Pool, qualifiedTable string) ([]TriggerDef, error) {
	rows, err := pool.Query(ctx, `
		SELECT tgname, pg_get_triggerdef(oid)
		FROM pg_trigger
		WHERE tgrelid = $1::regclass AND NOT tgisinternal
	`, qualifiedTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var triggers []TriggerDef
	for rows.Next() {
		var t TriggerDef
		if err := rows.Scan(&t.Name, &t.Definition); err != nil {
			return nil, err
		}
		triggers = append(triggers, t)
	}
	return triggers, rows.Err()
}

// fetchSequenceOwnerships deliberately excludes IDENTITY columns
// (attidentity != ”): those are already copied correctly by
// `CREATE TABLE ... LIKE ... INCLUDING ALL` (which includes
// INCLUDING IDENTITY). Only classic SERIAL-style columns — a plain integer
// column with a `nextval('seq')` default, where the sequence is OWNED BY
// that column — are missed by LIKE and need to be explicitly re-pointed.
func fetchSequenceOwnerships(ctx context.Context, pool *pgxpool.Pool, qualifiedTable string) ([]SequenceOwnership, error) {
	rows, err := pool.Query(ctx, `
		SELECT seqns.nspname, seq.relname, att.attname
		FROM pg_depend d
		JOIN pg_class seq ON seq.oid = d.objid AND seq.relkind = 'S'
		JOIN pg_namespace seqns ON seqns.oid = seq.relnamespace
		JOIN pg_attribute att ON att.attrelid = d.refobjid AND att.attnum = d.refobjsubid
		WHERE d.refobjid = $1::regclass
		  AND d.deptype = 'a'
		  AND att.attidentity = ''
	`, qualifiedTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var seqs []SequenceOwnership
	for rows.Next() {
		var s SequenceOwnership
		if err := rows.Scan(&s.SequenceSchema, &s.SequenceName, &s.ColumnName); err != nil {
			return nil, err
		}
		seqs = append(seqs, s)
	}
	return seqs, rows.Err()
}

func fetchDependentViews(ctx context.Context, pool *pgxpool.Pool, qualifiedTable string) ([]ViewDef, error) {
	rows, err := pool.Query(ctx, `
		SELECT DISTINCT vns.nspname, v.relname, v.relkind = 'm', pg_get_viewdef(v.oid)
		FROM pg_depend d
		JOIN pg_rewrite r ON r.oid = d.objid
		JOIN pg_class v ON v.oid = r.ev_class AND v.relkind IN ('v', 'm')
		JOIN pg_namespace vns ON vns.oid = v.relnamespace
		WHERE d.refobjid = $1::regclass AND v.oid != $1::regclass
	`, qualifiedTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var views []ViewDef
	for rows.Next() {
		var v ViewDef
		if err := rows.Scan(&v.Schema, &v.Name, &v.IsMaterialized, &v.Definition); err != nil {
			return nil, err
		}
		views = append(views, v)
	}
	return views, rows.Err()
}

func fetchRLSStatus(ctx context.Context, pool *pgxpool.Pool, qualifiedTable string) (enabled, forced bool, err error) {
	err = pool.QueryRow(ctx, `
		SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE oid = $1::regclass
	`, qualifiedTable).Scan(&enabled, &forced)
	return enabled, forced, err
}

func fetchRLSPolicies(ctx context.Context, pool *pgxpool.Pool, schema, table string) ([]RLSPolicyDef, error) {
	rows, err := pool.Query(ctx, `
		SELECT policyname, permissive = 'PERMISSIVE', roles::text[], cmd, qual, with_check
		FROM pg_policies
		WHERE schemaname = $1 AND tablename = $2
	`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var policies []RLSPolicyDef
	for rows.Next() {
		var p RLSPolicyDef
		if err := rows.Scan(&p.Name, &p.Permissive, &p.Roles, &p.Command, &p.UsingExpr, &p.CheckExpr); err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

// fetchGrants excludes the connecting role: re-granting a role its own
// implicit owner privileges is redundant and, depending on how the shadow
// table's ownership ends up being assigned, can sometimes error.
func fetchGrants(ctx context.Context, pool *pgxpool.Pool, schema, table string) ([]GrantDef, error) {
	rows, err := pool.Query(ctx, `
		SELECT grantee, privilege_type
		FROM information_schema.table_privileges
		WHERE table_schema = $1 AND table_name = $2 AND grantee != current_user
	`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var grants []GrantDef
	for rows.Next() {
		var g GrantDef
		if err := rows.Scan(&g.Grantee, &g.Privilege); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	return grants, rows.Err()
}

// ApplyToShadowTable creates every Category A object (triggers, RLS,
// grants, sequence ownership) directly on the shadow table. Called during
// Preparation, before Initial Sync starts.
//
// Triggers are created but left DISABLED (Architecture Doc Section 4.3.1):
// re-enabling them is the caller's responsibility, immediately after a
// successful swap (see EnableUserTriggers).
func ApplyToShadowTable(ctx context.Context, pool *pgxpool.Pool, schema, shadowTable string, deps *DependentObjects) error {
	shadowQualified := quoteIdent(schema) + "." + quoteIdent(shadowTable)

	for _, trig := range deps.Triggers {
		createSQL := retargetTriggerDef(trig.Definition, shadowQualified)
		if _, err := pool.Exec(ctx, createSQL); err != nil {
			return fmt.Errorf("failed to create trigger %q on shadow table: %w", trig.Name, err)
		}
		disableSQL := fmt.Sprintf("ALTER TABLE %s DISABLE TRIGGER %s", shadowQualified, quoteIdent(trig.Name))
		if _, err := pool.Exec(ctx, disableSQL); err != nil {
			return fmt.Errorf("failed to disable trigger %q on shadow table: %w", trig.Name, err)
		}
	}

	for _, seq := range deps.SequenceOwnerships {
		alterSQL := fmt.Sprintf("ALTER SEQUENCE %s.%s OWNED BY %s.%s",
			quoteIdent(seq.SequenceSchema), quoteIdent(seq.SequenceName), shadowQualified, quoteIdent(seq.ColumnName))
		if _, err := pool.Exec(ctx, alterSQL); err != nil {
			return fmt.Errorf("failed to re-point sequence ownership for %q: %w", seq.ColumnName, err)
		}
	}

	if deps.RLSEnabled {
		if _, err := pool.Exec(ctx, fmt.Sprintf("ALTER TABLE %s ENABLE ROW LEVEL SECURITY", shadowQualified)); err != nil {
			return fmt.Errorf("failed to enable row level security on shadow table: %w", err)
		}
	}
	if deps.RLSForced {
		if _, err := pool.Exec(ctx, fmt.Sprintf("ALTER TABLE %s FORCE ROW LEVEL SECURITY", shadowQualified)); err != nil {
			return fmt.Errorf("failed to force row level security on shadow table: %w", err)
		}
	}
	for _, policy := range deps.RLSPolicies {
		createSQL := buildCreatePolicySQL(policy, shadowQualified)
		if _, err := pool.Exec(ctx, createSQL); err != nil {
			return fmt.Errorf("failed to create RLS policy %q on shadow table: %w", policy.Name, err)
		}
	}

	for _, grant := range deps.Grants {
		grantSQL := fmt.Sprintf("GRANT %s ON %s TO %s", grant.Privilege, shadowQualified, quoteIdent(grant.Grantee))
		if _, err := pool.Exec(ctx, grantSQL); err != nil {
			return fmt.Errorf("failed to grant %s to %q on shadow table: %w", grant.Privilege, grant.Grantee, err)
		}
	}

	return nil
}

// RevertSequenceOwnership points every sequence ApplyToShadowTable
// re-pointed at the shadow table (via ALTER SEQUENCE ... OWNED BY) back at
// the SOURCE table instead — the mirror-image operation, for cleanup after
// a failed/aborted migration.
//
// Why this exists — found the hard way, via manual investigation of an
// orphaned shadow table `internal/reaper` could never actually clean up:
// ApplyToShadowTable's sequence-ownership transfer runs during
// Preparation, well before Initial Sync/Validation/Swap — so ANY failure
// after that point (initial sync failing, validation failing, the process
// crashing) leaves the sequence permanently owned by a shadow table that's
// about to be dropped. `failAndCleanup`'s plain `DROP TABLE IF EXISTS`
// (with no CASCADE, deliberately — see its own comment) then always fails
// with a real PostgreSQL error: "cannot drop table ... because other
// objects depend on it / DETAIL: default value for column id of table
// <source> depends on sequence <seq>" — because the LIVE source table's
// own id column DEFAULT still references that same sequence object,
// PostgreSQL won't let it be dropped alongside the shadow table without
// CASCADE, and CASCADE here would be actively dangerous: it would drop the
// sequence entirely, stripping the auto-increment default off the LIVE,
// still-in-production source table.
//
// Calling this before the DROP TABLE re-points ownership back to the
// source table first, so the shadow table is no longer "responsible" for
// a sequence something else still depends on, and can be dropped cleanly.
// Safe to call unconditionally (even if the transfer never actually
// happened, e.g. prepare() failed before reaching that step) — pointing a
// sequence's ownership at a column it may already belong to is a
// harmless no-op, not an error.
func RevertSequenceOwnership(ctx context.Context, pool *pgxpool.Pool, schema, sourceTable string, deps *DependentObjects) error {
	sourceQualified := quoteIdent(schema) + "." + quoteIdent(sourceTable)
	var firstErr error
	for _, seq := range deps.SequenceOwnerships {
		alterSQL := fmt.Sprintf("ALTER SEQUENCE %s.%s OWNED BY %s.%s",
			quoteIdent(seq.SequenceSchema), quoteIdent(seq.SequenceName), sourceQualified, quoteIdent(seq.ColumnName))
		if _, err := pool.Exec(ctx, alterSQL); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("failed to revert sequence ownership for %q: %w", seq.ColumnName, err)
			}
			continue // best-effort: still try to revert the rest even if one fails
		}
	}
	return firstErr
}

// EnableUserTriggers re-enables the triggers ApplyToShadowTable created in
// a disabled state. Called immediately after a successful swap — the table
// is now live under its final (original) name.
func EnableUserTriggers(ctx context.Context, pool *pgxpool.Pool, schema, table string, deps *DependentObjects) error {
	qualifiedTable := quoteIdent(schema) + "." + quoteIdent(table)
	for _, trig := range deps.Triggers {
		sql := fmt.Sprintf("ALTER TABLE %s ENABLE TRIGGER %s", qualifiedTable, quoteIdent(trig.Name))
		if _, err := pool.Exec(ctx, sql); err != nil {
			return fmt.Errorf("failed to enable trigger %q: %w", trig.Name, err)
		}
	}
	return nil
}

// ReattachAfterSwap recreates every Category B object (foreign keys,
// views) that pointed at the pre-swap table by OID. Called immediately
// after a successful swap.
//
// KNOWN LIMITATION: there is a brief window between the swap committing
// and this function running where a foreign key on another table still
// points at the renamed-away (tempTable) OID rather than the newly-live
// table. For the ALTER_COLUMN_TYPE scenario this package currently
// supports, no rows are added or removed during the migration (only
// retyped), so this window carries no practical data-integrity risk in
// practice — but it is not a fully atomic operation, and a future version
// could close this gap by folding the FK re-pointing into the same
// transaction as the swap itself.
func ReattachAfterSwap(ctx context.Context, pool *pgxpool.Pool, deps *DependentObjects) error {
	for _, fk := range deps.ForeignKeys {
		sourceQualified := quoteIdent(fk.SourceSchema) + "." + quoteIdent(fk.SourceTable)

		dropSQL := fmt.Sprintf("ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s", sourceQualified, quoteIdent(fk.ConstraintName))
		if _, err := pool.Exec(ctx, dropSQL); err != nil {
			return fmt.Errorf("failed to drop stale foreign key %q: %w", fk.ConstraintName, err)
		}

		addSQL := fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s",
			sourceQualified, quoteIdent(fk.ConstraintName), fk.Definition)
		if _, err := pool.Exec(ctx, addSQL); err != nil {
			return fmt.Errorf("failed to recreate foreign key %q: %w", fk.ConstraintName, err)
		}
	}

	for _, view := range deps.DependentViews {
		viewQualified := quoteIdent(view.Schema) + "." + quoteIdent(view.Name)
		kind := "VIEW"
		if view.IsMaterialized {
			kind = "MATERIALIZED VIEW"
		}
		// DROP + CREATE is used uniformly for both plain and materialized
		// views: materialized views don't support CREATE OR REPLACE at
		// all, and for our case (only a column's TYPE changes, never its
		// name or the table's column list) this behaves identically to
		// REPLACE for plain views too.
		dropSQL := fmt.Sprintf("DROP %s IF EXISTS %s", kind, viewQualified)
		if _, err := pool.Exec(ctx, dropSQL); err != nil {
			return fmt.Errorf("failed to drop stale view %q: %w", view.Name, err)
		}
		createSQL := fmt.Sprintf("CREATE %s %s AS %s", kind, viewQualified, view.Definition)
		if _, err := pool.Exec(ctx, createSQL); err != nil {
			return fmt.Errorf("failed to recreate view %q: %w", view.Name, err)
		}
	}

	return nil
}

// retargetTriggerDef rewrites a pg_get_triggerdef statement's "ON <table>"
// clause to point at the shadow table instead of the source table it was
// captured from. pg_get_triggerdef's output has a stable, well-known
// format: "CREATE TRIGGER name ... ON schema.table ...", making a single
// targeted replacement reliable.
func retargetTriggerDef(definition, newQualifiedTable string) string {
	idx := strings.Index(definition, " ON ")
	if idx == -1 {
		return definition // unexpected format; the caller's Exec will surface a clear error instead of silently mis-targeting
	}
	rest := definition[idx+len(" ON "):]
	spaceIdx := strings.IndexAny(rest, " \n\t")
	if spaceIdx == -1 {
		return definition
	}
	return definition[:idx+len(" ON ")] + newQualifiedTable + rest[spaceIdx:]
}

func buildCreatePolicySQL(policy RLSPolicyDef, qualifiedTable string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "CREATE POLICY %s ON %s", quoteIdent(policy.Name), qualifiedTable)
	if !policy.Permissive {
		b.WriteString(" AS RESTRICTIVE")
	}
	if policy.Command != "" && policy.Command != "ALL" {
		fmt.Fprintf(&b, " FOR %s", policy.Command)
	}
	if len(policy.Roles) > 0 {
		quotedRoles := make([]string, len(policy.Roles))
		for i, r := range policy.Roles {
			quotedRoles[i] = quoteIdent(r)
		}
		fmt.Fprintf(&b, " TO %s", strings.Join(quotedRoles, ", "))
	}
	if policy.UsingExpr != nil {
		fmt.Fprintf(&b, " USING (%s)", *policy.UsingExpr)
	}
	if policy.CheckExpr != nil {
		fmt.Fprintf(&b, " WITH CHECK (%s)", *policy.CheckExpr)
	}
	return b.String()
}
