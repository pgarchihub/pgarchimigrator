package upgrade

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PublicationName and SubscriptionName are derived from a job's ID,
// which is itself already a safe PostgreSQL identifier fragment
// (letters, digits, and underscores only — see newID) so no additional
// quoting/sanitizing is needed to embed it in DDL text here.
func PublicationName(jobID string) string  { return "pgarchimigrator_upgrade_" + jobID }
func SubscriptionName(jobID string) string { return "pgarchimigrator_upgrade_" + jobID }

// CreatePublication issues CREATE PUBLICATION on the source instance,
// scoped to exactly refs — never FOR ALL TABLES, even when job.Schemas
// was empty (meaning Introspect discovered "every table") — see
// Introspect's own doc comment for how that discovery already happened;
// by the time this function runs, the scope is always a concrete,
// already-known table list, so the publication is built to match
// exactly what was actually introspected and created on the target,
// not "whatever exists on source right now," which could silently
// drift from what CreateTableOnTarget actually built if new tables
// appeared on source between introspection and this call.
func CreatePublication(ctx context.Context, sourcePool *pgxpool.Pool, jobID string, refs []TableRef) error {
	if len(refs) == 0 {
		return fmt.Errorf("cannot create a publication with no tables in scope")
	}
	var qualified []string
	for _, ref := range refs {
		qualified = append(qualified, quoteIdent(ref.SchemaName)+"."+quoteIdent(ref.TableName))
	}
	sql := fmt.Sprintf("CREATE PUBLICATION %s FOR TABLE %s", quoteIdent(PublicationName(jobID)), strings.Join(qualified, ", "))
	if _, err := sourcePool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("failed to create publication: %w", err)
	}
	return nil
}

// DropPublication removes the publication created by CreatePublication
// — used both by Rollback (see rollback.go) and, in the future, by
// whatever eventually implements post-cutover cleanup once this
// package's own scope extends that far (see the package doc comment's
// "cutover is out of scope" note — cleanup after a cutover that this
// package didn't perform is a separate concern this function doesn't
// try to solve today, it only undoes what CreatePublication itself
// did).
func DropPublication(ctx context.Context, sourcePool *pgxpool.Pool, jobID string) error {
	sql := fmt.Sprintf("DROP PUBLICATION IF EXISTS %s", quoteIdent(PublicationName(jobID)))
	if _, err := sourcePool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("failed to drop publication: %w", err)
	}
	return nil
}

// CreateSubscription issues CREATE SUBSCRIPTION on the target instance,
// pointing at the source via sourceConnString and subscribing to the
// publication CreatePublication already built. PostgreSQL's own
// subscription machinery takes it from here — an initial full copy of
// every in-scope table's existing rows (via per-table "tablesync"
// worker processes), then a live switch to streaming replication of
// ongoing changes, all without this package needing to hand-roll any
// of that itself. See SyncProgress for how this package observes that
// built-in progress rather than driving it.
//
// sourceConnString is a plain libpq connection string (not the "ref"
// used elsewhere in this package) since PostgreSQL's own CONNECTION
// clause requires the real DSN, already resolved via ConnectionProvider
// by this point — see flow.go for where that resolution happens.
func CreateSubscription(ctx context.Context, targetPool *pgxpool.Pool, jobID, sourceConnString string) error {
	sql := fmt.Sprintf(
		"CREATE SUBSCRIPTION %s CONNECTION %s PUBLICATION %s",
		quoteIdent(SubscriptionName(jobID)), quoteLiteral(sourceConnString), quoteIdent(PublicationName(jobID)),
	)
	if _, err := targetPool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("failed to create subscription: %w", err)
	}
	return nil
}

// DropSubscription removes the subscription created by CreateSubscription.
func DropSubscription(ctx context.Context, targetPool *pgxpool.Pool, jobID string) error {
	sql := fmt.Sprintf("DROP SUBSCRIPTION IF EXISTS %s", quoteIdent(SubscriptionName(jobID)))
	if _, err := targetPool.Exec(ctx, sql); err != nil {
		return fmt.Errorf("failed to drop subscription: %w", err)
	}
	return nil
}

// quoteLiteral quotes a string as a DDL string literal (as opposed to
// quoteIdent's identifier quoting) — this package's own copy of the
// exact same helper engines/postgresql/shadowflow already has (see that
// package's own quoteLiteral for the original), needed here because
// CONNECTION expects a string literal, not an identifier.
func quoteLiteral(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}

// rawSubstate is one row of pg_subscription_rel, before translation
// into this package's own Phase vocabulary — shared by SyncProgress and
// AllTablesReady (via querySubstates) so the underlying catalog query
// exists in exactly one place; SyncProgress translates every row via
// mapSubstate, AllTablesReady checks the raw 'r' value directly (see
// its own doc comment for why that distinction matters there but
// nowhere else).
type rawSubstate struct {
	Ref      TableRef
	Substate string
}

// querySubstates is SyncProgress and AllTablesReady's shared query
// against pg_subscription_rel — PostgreSQL's own built-in record of
// per-table replication progress for a subscription.
func querySubstates(ctx context.Context, targetPool *pgxpool.Pool, jobID string) ([]rawSubstate, error) {
	rows, err := targetPool.Query(ctx, `
		SELECT n.nspname, c.relname, sr.srsubstate::text
		FROM pg_subscription_rel sr
		JOIN pg_class c ON c.oid = sr.srrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		JOIN pg_subscription s ON s.oid = sr.srsubid
		WHERE s.subname = $1
	`, SubscriptionName(jobID))
	if err != nil {
		return nil, fmt.Errorf("failed to query subscription sync progress: %w", err)
	}
	defer rows.Close()

	var states []rawSubstate
	for rows.Next() {
		var schema, table, substate string
		if err := rows.Scan(&schema, &table, &substate); err != nil {
			return nil, fmt.Errorf("failed to scan subscription sync progress: %w", err)
		}
		states = append(states, rawSubstate{Ref: TableRef{SchemaName: schema, TableName: table}, Substate: substate})
	}
	return states, rows.Err()
}

// TableSyncState is one table's replication progress, as PostgreSQL's
// own pg_subscription_rel catalog reports it — see SyncProgress.
type TableSyncState struct {
	Ref   TableRef
	Phase Phase // mapped from PostgreSQL's own srsubstate — see mapSubstate
}

// SyncProgress queries the target instance's pg_subscription_rel
// catalog — PostgreSQL's own built-in record of per-table replication
// progress for a subscription — and returns it translated into this
// package's own Phase vocabulary (see mapSubstate). This is read-only
// observation of state PostgreSQL is already tracking on its own; this
// package drives nothing about the actual per-table copy progress,
// only reports what PostgreSQL itself has already done.
func SyncProgress(ctx context.Context, targetPool *pgxpool.Pool, jobID string) ([]TableSyncState, error) {
	raw, err := querySubstates(ctx, targetPool, jobID)
	if err != nil {
		return nil, err
	}
	states := make([]TableSyncState, len(raw))
	for i, r := range raw {
		states[i] = TableSyncState{Ref: r.Ref, Phase: mapSubstate(r.Substate)}
	}
	return states, nil
}

// mapSubstate translates PostgreSQL's own single-character
// pg_subscription_rel.srsubstate values (documented under "Subscription
// Tables" in the PostgreSQL catalog reference) into this package's
// Phase vocabulary:
//
//	'i' (initialize)        -> PhaseSchemaCreated: not yet started copying
//	'd' (data is being copied) -> PhaseSyncing: initial bulk copy in progress
//	's' (synchronized)      -> PhaseSyncing: initial copy done, catching up to the live stream
//	'r' (ready)              -> PhaseSyncing: caught up, now streaming ongoing changes live
//
// Deliberately maps BOTH 's' and 'r' to PhaseSyncing rather than giving
// 'r' its own Phase value — see AllTablesReady's own doc comment for
// where "every table reached 'r'" actually gets acted on (moving the
// whole JOB to PhaseValidating); a per-table Phase distinguishing 'r'
// specifically isn't needed anywhere else in this package, and
// inventing one would be state with no reader.
func mapSubstate(substate string) Phase {
	switch substate {
	case "i":
		return PhaseSchemaCreated
	case "d", "s", "r":
		return PhaseSyncing
	default:
		return PhaseSyncing // unrecognized/future PostgreSQL state — fail toward "still in progress," never toward a false PhaseReady
	}
}

// AllTablesReady reports whether every table in refs has reached
// PostgreSQL's own 'r' (ready/streaming) state — the signal this
// package's flow (see flow.go) uses to decide it's safe to move the Job
// from PhaseSyncing to PhaseValidating. Checks the raw 'r' value
// (via querySubstates) rather than the translated Phase specifically
// because 'r' needs to be distinguished from 's' here even though
// mapSubstate deliberately collapses that distinction for every other
// caller — see mapSubstate's own doc comment for why that collapse is
// fine everywhere else but not here.
func AllTablesReady(ctx context.Context, targetPool *pgxpool.Pool, jobID string, refs []TableRef) (bool, error) {
	raw, err := querySubstates(ctx, targetPool, jobID)
	if err != nil {
		return false, err
	}

	ready := make(map[string]bool, len(raw))
	for _, r := range raw {
		if r.Substate == "r" {
			ready[r.Ref.String()] = true
		}
	}

	for _, ref := range refs {
		if !ready[ref.String()] {
			return false, nil
		}
	}
	return true, nil
}
