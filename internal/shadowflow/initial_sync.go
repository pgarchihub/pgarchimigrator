// initial_sync.go implements Architecture Doc Section 4.1 step 2 "Initial
// Sync": copying the existing rows of the source table into the shadow
// table in the background, in batches, so a single long-running COPY never
// holds locks or resources for an extended period (Section 8 "Long-Running
// Transaction" risk — same rationale as internal/ddlflow's backfillLoop).
package shadowflow

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/monitor"
)

// defaultInitialSyncBatchSize matches internal/ddlflow's default for
// consistency (Architecture Doc Section 8: "commit every 10k rows").
const defaultInitialSyncBatchSize = 10000

// initialSyncLockWaitBackoff mirrors internal/ddlflow's backoff constant.
const initialSyncLockWaitBackoff = 2 * time.Second

// InitialSyncConfig groups the parameters needed to copy a table's
// existing rows into its shadow table.
type InitialSyncConfig struct {
	Pool         *pgxpool.Pool
	Watcher      monitor.Watcher // optional; FR-05/FR-06 awareness, nil skips throttling
	BatchSize    int
	SourceSchema string
	SourceTable  string
	ShadowTable  string

	// CastColumn/CastType: see the identical fields on ApplyEngine — the
	// column undergoing an explicit type change needs `::<CastType>` in the
	// SELECT list so PostgreSQL performs the conversion during the copy
	// rather than failing on an assignment-incompatible cast.
	CastColumn string
	CastType   string

	// PKColumns is the shadow table's primary key column name(s) — see
	// runBatch's doc comment for why this is required, not optional, and
	// what silently omitting it used to cause.
	PKColumns []string

	// OnBatchComplete, if set, is called after each successful batch with
	// the number of rows actually copied in that batch (post ON CONFLICT
	// — see runBatch's doc comment for why a batch can copy fewer rows
	// than it scanned). Optional — nil means no progress reporting,
	// matching this package's existing nil-is-fine conventions (see e.g.
	// Watcher above). Wired by runInitialSync (shadowflow.go) to the
	// job's persisted RowsProcessed counter, the same way
	// internal/ddlflow's backfillLoop already does for EXPAND_BACKFILL —
	// added specifically because SHADOW_TABLE migrations previously gave
	// ZERO progress visibility while running: a genuinely large table
	// taking many minutes (or, under heavy concurrent write load,
	// potentially much longer) looked, from the outside — via the API,
	// the web dashboard, or a direct database query — completely
	// identical to one that was permanently stuck. A real, load-testing-
	// found gap: diagnosing a real multi-hour SYNCING phase took direct
	// pg_stat_activity/pg_replication_slots inspection because nothing
	// in this project's own observability surfaced it.
	OnBatchComplete func(rowsCopiedThisBatch int64)
}

// Run copies all rows from the source table into the shadow table in
// resumable batches ordered by ctid. PostgreSQL 12+ provides a btree
// operator class for the `tid` type (see TR-11's minimum version
// requirement), which is what makes `WHERE ctid > $1 ORDER BY ctid`
// efficient and correct as a resume cursor.
//
// PKColumns must be set — see runBatch's doc comment for why this
// package's own concurrent-replication-capture design makes ON CONFLICT
// handling required, not optional, on the batch INSERT.
//
// The columns copied are the intersection of the shadow table's and the
// source table's column names — this matches the flagship use case (the
// shadow table was created via `CREATE TABLE ... LIKE source ...` so the
// column sets are identical except for the one column undergoing a type
// change, which is still present by name on both sides).
func (cfg InitialSyncConfig) Run(ctx context.Context) error {
	columns, err := commonColumns(ctx, cfg.Pool, cfg.SourceSchema, cfg.SourceTable, cfg.ShadowTable)
	if err != nil {
		return fmt.Errorf("failed to determine columns to copy: %w", err)
	}
	if len(columns) == 0 {
		return fmt.Errorf("no common columns found between %s.%s and %s.%s",
			cfg.SourceSchema, cfg.SourceTable, cfg.SourceSchema, cfg.ShadowTable)
	}

	var lastCtid *string
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		if cfg.Watcher != nil {
			if waiting, err := cfg.Watcher.CheckLockWait(ctx); err == nil && waiting {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-time.After(initialSyncLockWaitBackoff):
				}
				continue
			}
		}

		rowsCopied, newLastCtid, err := cfg.runBatch(ctx, columns, lastCtid)
		if err != nil {
			return fmt.Errorf("initial sync batch failed: %w", err)
		}
		// Skip the callback entirely for a zero-row batch — this
		// includes the final "confirms there's nothing left to scan"
		// call the loop always makes right before newLastCtid==nil
		// terminates it (harmless to report — Store.IncrementRowsProcessed
		// with 0 is a no-op — but a pointless extra write, and it made
		// this exact behavior easy to get wrong in a test: an assertion
		// counting "one call per batch of real work" needs to know this
		// trailing zero-row call doesn't happen).
		if cfg.OnBatchComplete != nil && rowsCopied > 0 {
			cfg.OnBatchComplete(rowsCopied)
		}
		// Termination is based on newLastCtid (whether the batch found
		// ANY candidate ctids at all), not rowsCopied (how many were
		// actually inserted) — these used to always agree, but the
		// ON CONFLICT DO NOTHING fix (see runBatch's doc comment) means
		// they can now genuinely differ: a batch can find N candidate
		// rows but insert 0 of them (all N already present, e.g.
		// ApplyEngine got there first under heavy concurrent write
		// load) — that is NOT the same thing as "nothing left to scan".
		// rowsCopied == 0 as the loop's stop condition would have ended
		// the sync early in exactly that case, silently leaving
		// unscanned rows behind — a real regression this exact
		// distinction fixes.
		if newLastCtid == nil {
			return nil
		}
		lastCtid = newLastCtid
	}
}

// runBatch copies a single batch and returns how many rows were copied and
// the last (highest) ctid seen, to resume from on the next call.
//
// The INSERT ends in `ON CONFLICT (pk...) DO NOTHING` — required, not
// optional, and not merely defensive. This package's own design
// deliberately starts logical replication capture (see shadowflow.go's
// startDeltaSync, launched as a background goroutine BEFORE runInitialSync
// even begins) and lets it run CONCURRENTLY with this batch copy — see
// shadowflow.go's own comment: "overlaps with it because ApplyEngine.Apply
// is idempotent" (ApplyEngine.upsert uses its own ON CONFLICT ... DO
// UPDATE). That comment's safety claim only held for ONE side of the
// overlap: if a row is concurrently written on the source table WHILE
// initial sync is scanning, ApplyEngine can process the replicated change
// and insert that row into the shadow table BEFORE initial sync's own
// ctid-ordered scan reaches it — and this INSERT, without ON CONFLICT,
// would then hit a real primary-key violation trying to insert the SAME
// row again. Found via load testing with genuinely concurrent write
// traffic during a real SHADOW_TABLE migration — not a theoretical
// edge case, an actual `duplicate key value violates unique constraint
// "..._pkey" (SQLSTATE 23505)` failure. DO NOTHING (not DO UPDATE) is the
// correct resolution: if ApplyEngine already wrote this row, it wrote the
// CURRENT value from the live replication stream — initial sync's
// snapshot-based copy of that same row is, by definition, no newer, so it
// must never clobber what Apply already got right.
func (cfg InitialSyncConfig) runBatch(ctx context.Context, columns []string, lastCtid *string) (int64, *string, error) {
	selectList := make([]string, len(columns))
	insertList := make([]string, len(columns))
	for i, col := range columns {
		insertList[i] = quoteIdent(col)
		if cfg.CastColumn != "" && col == cfg.CastColumn {
			selectList[i] = fmt.Sprintf("%s::%s", quoteIdent(col), cfg.CastType)
		} else {
			selectList[i] = quoteIdent(col)
		}
	}

	pkList := make([]string, len(cfg.PKColumns))
	for i, col := range cfg.PKColumns {
		pkList[i] = quoteIdent(col)
	}

	sourceQualified := quoteIdent(cfg.SourceSchema) + "." + quoteIdent(cfg.SourceTable)
	shadowQualified := quoteIdent(cfg.SourceSchema) + "." + quoteIdent(cfg.ShadowTable)

	sql := fmt.Sprintf(`
		WITH batch AS (
			SELECT ctid FROM %s
			WHERE $1::text IS NULL OR ctid > $1::tid
			ORDER BY ctid
			LIMIT $2
		),
		ins AS (
			INSERT INTO %s (%s)
			SELECT %s FROM %s
			WHERE ctid IN (SELECT ctid FROM batch)
			ON CONFLICT (%s) DO NOTHING
			RETURNING 1
		)
		SELECT
			(SELECT count(*) FROM ins),
			(SELECT max(ctid)::text FROM batch)
	`, sourceQualified, shadowQualified, strings.Join(insertList, ", "),
		strings.Join(selectList, ", "), sourceQualified, strings.Join(pkList, ", "))

	var rowsCopied int64
	var maxCtid *string
	err := cfg.Pool.QueryRow(ctx, sql, lastCtid, cfg.batchSize()).Scan(&rowsCopied, &maxCtid)
	if err != nil {
		return 0, nil, err
	}
	return rowsCopied, maxCtid, nil
}

func (cfg InitialSyncConfig) batchSize() int {
	if cfg.BatchSize <= 0 {
		return defaultInitialSyncBatchSize
	}
	return cfg.BatchSize
}

// commonColumns returns the column names present in both the source and
// shadow tables, ordered by the source table's column order.
func commonColumns(ctx context.Context, pool *pgxpool.Pool, schema, sourceTable, shadowTable string) ([]string, error) {
	query := `
		SELECT s.column_name
		FROM information_schema.columns s
		JOIN information_schema.columns d
		  ON d.table_schema = s.table_schema AND d.table_name = $3 AND d.column_name = s.column_name
		WHERE s.table_schema = $1 AND s.table_name = $2
		ORDER BY s.ordinal_position
	`
	rows, err := pool.Query(ctx, query, schema, sourceTable, shadowTable)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cols []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		cols = append(cols, name)
	}
	return cols, rows.Err()
}
