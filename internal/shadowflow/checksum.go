// checksum.go implements the chunked checksum validation required by
// Requirements Doc TR-10: row-count parity alone (validate's first check)
// catches missing/extra rows but not a row whose non-key column was
// corrupted during Apply. This file adds a second, stronger check.
//
// SCOPE: works for any primary key — single-column or composite, any
// orderable type (integer, text, uuid, timestamp, ...). This is possible
// because pagination uses PostgreSQL's native ROW/tuple comparison
// operator, e.g. `WHERE (col1, col2) > ($1, $2)`, rather than casting key
// values to text for a cursor comparison. Row comparison evaluates each
// column with its OWN native comparison operator (numeric for integers,
// collation-aware for text, etc.) and short-circuits left-to-right exactly
// like `ORDER BY col1, col2` does — so it can never suffer the classic
// text-cast trap where `'9' > '10'` lexicographically but `9 < 10`
// numerically. An earlier version of this file restricted the fast path to
// single-column integer keys specifically to dodge that trap; row
// comparison sidesteps it entirely, so that restriction is gone.
//
// The only remaining requirement is that every PK column has SOME default
// btree ordering (true for essentially every real-world PK type). If a
// column's type genuinely has no ordering operator, PostgreSQL itself
// raises a clear error at query time rather than this code silently
// producing wrong results.
package shadowflow

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// defaultChecksumBatchSize matches the other batch-size defaults in this
// package (see initial_sync.go, internal/ddlflow) for consistency.
const defaultChecksumBatchSize = 10000

// nullSentinel is used by buildRowHashExpr — see its doc comment.
//
// IMPORTANT: this must be a plain printable ASCII string, not a Go escape
// like "\x00" — that produces an actual embedded NUL byte in the resulting
// Go string, which corrupts PostgreSQL's NUL-terminated wire protocol
// framing once sent (this was observed as a cryptic "SQLSTATE 08P01:
// insufficient data left in message" that had nothing to do with
// parameter binding — purely a raw control byte inside the SQL text).
const nullSentinel = "PGAM_NULL_SENTINEL_9f3a"

// validateChunkedChecksum compares the source and shadow tables chunk by
// chunk (ordered by primary key), so a single huge checksum operation
// never runs against the whole table at once (Architecture Doc Section 8
// "Long-Running Transaction" risk, and TR-08's 5% performance ceiling).
func validateChunkedChecksum(ctx context.Context, pool *pgxpool.Pool, schema, sourceTable, shadowTable string, pkCols []string, castColumn, castType string, batchSize int) error {
	if len(pkCols) == 0 {
		return nil // no primary key at all: row-count comparison (already done by the caller) is the only check possible
	}

	columns, err := commonColumns(ctx, pool, schema, sourceTable, shadowTable)
	if err != nil {
		return fmt.Errorf("failed to determine columns for checksum validation: %w", err)
	}
	if len(columns) == 0 {
		return fmt.Errorf("no common columns found between %s.%s and %s.%s", schema, sourceTable, schema, shadowTable)
	}

	sourceQualified := quoteIdent(schema) + "." + quoteIdent(sourceTable)
	shadowQualified := quoteIdent(schema) + "." + quoteIdent(shadowTable)
	sourceHashExpr := buildRowHashExpr(columns, castColumn, castType)
	shadowHashExpr := buildRowHashExpr(columns, "", "") // shadow table's column is already the target type

	// cursor is a tuple of primary-key values (one per pkCols entry), or
	// nil for "no lower bound yet" (the first batch). Using a Go slice of
	// `any` — rather than a fixed numeric type — is exactly what makes
	// composite and non-integer keys work: each element round-trips
	// through pgx as whatever Go type it natively maps to for that
	// column's Postgres type.
	var cursor []any
	for {
		maxPK, batchCount, err := findBatchBoundary(ctx, pool, sourceQualified, pkCols, cursor, batchSize)
		if err != nil {
			return fmt.Errorf("failed to determine the next checksum batch boundary: %w", err)
		}
		if batchCount == 0 {
			return nil // no more rows on the source side; done
		}

		sourceCount, sourceSum, err := rangeChecksum(ctx, pool, sourceQualified, pkCols, sourceHashExpr, cursor, maxPK)
		if err != nil {
			return fmt.Errorf("source checksum batch failed: %w", err)
		}

		shadowCount, shadowSum, err := rangeChecksum(ctx, pool, shadowQualified, pkCols, shadowHashExpr, cursor, maxPK)
		if err != nil {
			return fmt.Errorf("shadow checksum batch failed: %w", err)
		}

		if sourceCount != shadowCount || sourceSum != shadowSum {
			return fmt.Errorf("checksum mismatch in primary key range (%s, %s]: source has %d row(s) (checksum %d), shadow has %d row(s) (checksum %d)",
				tupleLabel(cursor), tupleLabel(maxPK), sourceCount, sourceSum, shadowCount, shadowSum)
		}
		cursor = maxPK
	}
}

// buildRowHashExpr builds an MD5-based expression hashing every column's
// text representation, in a stable (source-order) sequence. When
// castColumn is non-empty, that one column is cast to castType first —
// this normalizes the SOURCE side's value to what it will look like on
// the shadow side post-migration (e.g. the text '100' becomes the same
// representation as the shadow table's already-integer 100), so the two
// hashes are comparable at all. NULLs are mapped to a printable sentinel
// string (not the empty string) so NULL and ” never hash identically.
func buildRowHashExpr(columns []string, castColumn, castType string) string {
	parts := make([]string, len(columns))
	for i, col := range columns {
		expr := quoteIdent(col)
		if castColumn != "" && col == castColumn {
			expr = fmt.Sprintf("(%s::%s)", expr, castType)
		}
		parts[i] = fmt.Sprintf("coalesce(%s::text, '%s')", expr, nullSentinel)
	}
	return "md5(" + strings.Join(parts, " || '|' || ") + ")"
}

// findBatchBoundary determines the next batch's upper primary-key bound
// (as a tuple, one value per pkCols entry) and how many rows fall in it.
//
// The boundary tuple is found via N correlated scalar subqueries — one per
// PK column — each doing `ORDER BY <all pk cols> DESC LIMIT 1` against the
// same small `batch` CTE, which is the row-comparison-friendly equivalent
// of `MAX(pk)` for a single column (there's no built-in MAX aggregate for
// arbitrary multi-column tuples in PostgreSQL, so this is the standard
// workaround). When batch is empty, every scalar subquery correctly
// yields NULL and count(*) yields 0 — the caller checks count == 0 before
// using the (meaningless) NULL boundary values, so that's never an issue.
func findBatchBoundary(ctx context.Context, pool *pgxpool.Pool, qualifiedTable string, pkCols []string, cursor []any, batchSize int) ([]any, int64, error) {
	quotedCols := make([]string, len(pkCols))
	for i, c := range pkCols {
		quotedCols[i] = quoteIdent(c)
	}
	colList := strings.Join(quotedCols, ", ")

	descList := make([]string, len(quotedCols))
	for i, c := range quotedCols {
		descList[i] = c + " DESC"
	}
	descOrder := strings.Join(descList, ", ")

	var whereClause string
	args := make([]any, 0, len(pkCols)+1)
	if cursor != nil {
		placeholders := make([]string, len(pkCols))
		for i := range pkCols {
			args = append(args, cursor[i])
			placeholders[i] = fmt.Sprintf("$%d", len(args))
		}
		whereClause = fmt.Sprintf("WHERE (%s) > (%s)", colList, strings.Join(placeholders, ", "))
	}
	args = append(args, batchSize)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))

	boundaryExprs := make([]string, len(pkCols))
	for i, c := range quotedCols {
		boundaryExprs[i] = fmt.Sprintf("(SELECT %s FROM batch ORDER BY %s LIMIT 1)", c, descOrder)
	}

	sql := fmt.Sprintf(`
		WITH batch AS (
			SELECT %s FROM %s
			%s
			ORDER BY %s
			LIMIT %s
		)
		SELECT (SELECT count(*) FROM batch), %s
	`, colList, qualifiedTable, whereClause, colList, limitPlaceholder, strings.Join(boundaryExprs, ", "))

	dest := make([]any, len(pkCols)+1)
	var count int64
	dest[0] = &count
	values := make([]any, len(pkCols))
	for i := range values {
		dest[i+1] = &values[i]
	}

	if err := pool.QueryRow(ctx, sql, args...).Scan(dest...); err != nil {
		return nil, 0, err
	}
	if count == 0 {
		return nil, 0, nil
	}
	return values, count, nil
}

// rangeChecksum computes an order-independent checksum (a SUM of per-row
// hash fragments, so exact row ordering within the range never matters)
// for all rows in the (cursor, maxPK] primary-key range — using tuple
// comparison, so this works identically for single-column and composite
// keys. Used symmetrically for BOTH the source and shadow table with the
// exact same boundary values, guaranteeing the two calls cover the
// identical logical set of primary key values even though they're two
// independent physical tables.
//
// Each row's 64-bit hash fragment is reduced modulo 2147483647 (a Mersenne
// prime comfortably under 2^31) BEFORE summing: PostgreSQL's sum(bigint)
// aggregate always returns numeric (specifically to avoid overflow when
// accumulating many large bigint values), and once that numeric total
// exceeds int64's range, scanning it into a Go int64 fails outright
// ("... is out of range for int64"). Bounding each term to roughly ±2^31
// keeps the accumulated sum for any realistic batch size well within
// int64's range, so the final `::bigint` cast below always succeeds.
func rangeChecksum(ctx context.Context, pool *pgxpool.Pool, qualifiedTable string, pkCols []string, hashExpr string, cursor, maxPK []any) (count int64, checksum int64, err error) {
	quotedCols := make([]string, len(pkCols))
	for i, c := range pkCols {
		quotedCols[i] = quoteIdent(c)
	}
	colList := strings.Join(quotedCols, ", ")

	var conditions []string
	args := make([]any, 0, len(pkCols)*2)

	if cursor != nil {
		placeholders := make([]string, len(pkCols))
		for i := range pkCols {
			args = append(args, cursor[i])
			placeholders[i] = fmt.Sprintf("$%d", len(args))
		}
		conditions = append(conditions, fmt.Sprintf("(%s) > (%s)", colList, strings.Join(placeholders, ", ")))
	}
	{
		placeholders := make([]string, len(pkCols))
		for i := range pkCols {
			args = append(args, maxPK[i])
			placeholders[i] = fmt.Sprintf("$%d", len(args))
		}
		conditions = append(conditions, fmt.Sprintf("(%s) <= (%s)", colList, strings.Join(placeholders, ", ")))
	}

	sql := fmt.Sprintf(`
		SELECT count(*), COALESCE(sum((('x' || substr(%s, 1, 16))::bit(64)::bigint) %% 2147483647), 0)::bigint
		FROM %s
		WHERE %s
	`, hashExpr, qualifiedTable, strings.Join(conditions, " AND "))

	err = pool.QueryRow(ctx, sql, args...).Scan(&count, &checksum)
	return count, checksum, err
}

// tupleLabel renders a primary-key tuple for error messages, e.g. "(42,
// 7)" for a composite key or "42" for a single-column one. nil (the "no
// lower bound yet" cursor state) renders as "start".
func tupleLabel(values []any) string {
	if values == nil {
		return "start"
	}
	if len(values) == 1 {
		return fmt.Sprintf("%v", values[0])
	}
	parts := make([]string, len(values))
	for i, v := range values {
		parts[i] = fmt.Sprintf("%v", v)
	}
	return "(" + strings.Join(parts, ", ") + ")"
}
