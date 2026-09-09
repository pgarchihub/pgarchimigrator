package upgrade

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TableValidationResult is one table's validation outcome — see
// ValidateTable.
type TableValidationResult struct {
	Ref            TableRef
	SourceRowCount int64
	TargetRowCount int64
	SourceChecksum int64
	TargetChecksum int64
	Matched        bool
}

// ValidateTable compares row count AND a simple aggregate checksum
// between the source and target instance for one table — the
// cross-instance equivalent of engines/postgresql/shadowflow's own validate step,
// necessarily reimplemented rather than reused: engines/postgresql/shadowflow's
// validateChunkedChecksum takes a single *pgxpool.Pool and compares two
// tables reachable through it (source and shadow table, same instance);
// here source and target are two ENTIRELY SEPARATE PostgreSQL instances
// with no single connection that could see both.
//
// Deliberately a SIMPLER checksum than engines/postgresql/shadowflow's own
// (a single whole-table aggregate rather than that package's chunked,
// primary-key-ordered range comparison) — a known, accepted
// limitation for this first version: a single unchunked aggregate
// query is straightforward and catches real data mismatches, but scales
// less gracefully to very large tables (one long-running query per
// table, rather than resumable, boundable chunks) and — unlike
// engines/postgresql/shadowflow's own chunked approach — can't narrow a mismatch
// down to which specific ROWS differ, only that the table as a whole
// doesn't match. Bringing engines/postgresql/shadowflow's own chunked technique
// here, generalized to two separate connections, is a reasonable
// follow-up once this package's core flow is otherwise working
// end to end.
func ValidateTable(ctx context.Context, sourcePool, targetPool *pgxpool.Pool, ref TableRef) (TableValidationResult, error) {
	result := TableValidationResult{Ref: ref}

	sourceCount, sourceChecksum, err := countAndChecksum(ctx, sourcePool, ref)
	if err != nil {
		return result, fmt.Errorf("failed to validate %s on source: %w", ref, err)
	}
	targetCount, targetChecksum, err := countAndChecksum(ctx, targetPool, ref)
	if err != nil {
		return result, fmt.Errorf("failed to validate %s on target: %w", ref, err)
	}

	result.SourceRowCount = sourceCount
	result.TargetRowCount = targetCount
	result.SourceChecksum = sourceChecksum
	result.TargetChecksum = targetChecksum
	result.Matched = sourceCount == targetCount && sourceChecksum == targetChecksum
	return result, nil
}

// countAndChecksum computes a row count and a simple order-independent
// aggregate checksum for one table on one instance —
// sum(hashtext(row::text)) cast through a bigint aggregate. Using the
// whole row (::text of the record itself) rather than naming individual
// columns means this automatically covers every column without this
// package needing its own column enumeration here (Introspect/
// CreateTableOnTarget already did that work when the table was
// created) — the trade-off is that this is sensitive to column ORDER
// matching between source and target, which CreateTableOnTarget already
// guarantees by construction (it creates target columns in the exact
// order engines/postgresql/catalog.ListColumns returned them from source).
func countAndChecksum(ctx context.Context, pool *pgxpool.Pool, ref TableRef) (count int64, checksum int64, err error) {
	sql := fmt.Sprintf(
		`SELECT count(*), coalesce(sum(hashtext(t::text)), 0) FROM %s.%s t`,
		quoteIdent(ref.SchemaName), quoteIdent(ref.TableName),
	)
	row := pool.QueryRow(ctx, sql)
	if err := row.Scan(&count, &checksum); err != nil {
		return 0, 0, err
	}
	return count, checksum, nil
}
