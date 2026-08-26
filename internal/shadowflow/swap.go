// swap.go implements Architecture Doc Section 4.1 step 6 "Swap". This file
// handles the most fragile point of the "zero downtime" promise (lock queue
// pile-up) — see the Section 8 risk table.
package shadowflow

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// lockTimeoutSQLState is the SQLSTATE PostgreSQL returns when a statement
// is aborted by lock_timeout.
const lockTimeoutSQLState = "55P03"

// SwapConfig matches the default values from Requirements Doc FR-06.
type SwapConfig struct {
	LockTimeout time.Duration // default: 2 * time.Second
	MaxRetries  int           // default: 5
	BackoffBase time.Duration // base duration for exponential backoff
}

func DefaultSwapConfig() SwapConfig {
	return SwapConfig{
		LockTimeout: 2 * time.Second,
		MaxRetries:  5,
		BackoffBase: 500 * time.Millisecond,
	}
}

// SwapExecutor wraps the atomic RENAME TABLE swap with SET LOCAL
// lock_timeout and retries with exponential backoff if needed.
type SwapExecutor struct {
	Pool *pgxpool.Pool
}

// NewSwapExecutor creates a SwapExecutor with the given connection pool.
func NewSwapExecutor(pool *pgxpool.Pool) *SwapExecutor {
	return &SwapExecutor{Pool: pool}
}

// Swap performs the atomic table swap described in Architecture Doc Section
// 4.1 step 6:
//
//	BEGIN;
//	SET LOCAL lock_timeout = '<cfg.LockTimeout>';
//	ALTER TABLE <old> RENAME TO <temp>;
//	ALTER TABLE <new> RENAME TO <old>;
//	COMMIT;
//
// On a lock_timeout error (SQLSTATE 55P03), the transaction is rolled back,
// a backoff is applied, and the swap is retried up to MaxRetries times.
// Errors that are NOT caused by lock_timeout are returned immediately
// without retrying, since retrying would not help (e.g. the table doesn't
// exist). If all lock_timeout-related retries are exhausted, the swap is
// considered to have never happened — the original tables are left
// untouched (each attempt runs in its own transaction, so a partial
// rename can never be left in place), and the caller should surface this
// to the user per FR-06.
func (s *SwapExecutor) Swap(ctx context.Context, schema, oldTable, newTable, tempName string, cfg SwapConfig) error {
	var lastErr error
	for attempt := 0; attempt <= cfg.MaxRetries; attempt++ {
		if attempt > 0 {
			backoff := cfg.BackoffBase * time.Duration(1<<uint(attempt-1)) // 500ms, 1s, 2s, 4s, 8s
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
		}

		err := s.attemptSwap(ctx, schema, oldTable, newTable, tempName, cfg.LockTimeout)
		if err == nil {
			return nil
		}

		lastErr = err
		if !isLockTimeoutError(err) {
			return fmt.Errorf("swap failed with a non-retryable error: %w", err)
		}
	}
	return fmt.Errorf("swap failed after %d attempt(s) (lock_timeout=%s): %w", cfg.MaxRetries+1, cfg.LockTimeout, lastErr)
}

// attemptSwap runs a single swap attempt inside one transaction. Each
// attempt is fully self-contained: if it fails at any point, the deferred
// Rollback guarantees neither rename took effect, so a failed attempt can
// never leave the schema half-swapped.
func (s *SwapExecutor) attemptSwap(ctx context.Context, schema, oldTable, newTable, tempName string, lockTimeout time.Duration) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin swap transaction: %w", err)
	}
	defer tx.Rollback(ctx) // no-op once Commit has succeeded

	lockTimeoutSQL := fmt.Sprintf("SET LOCAL lock_timeout = '%dms'", lockTimeout.Milliseconds())
	if _, err := tx.Exec(ctx, lockTimeoutSQL); err != nil {
		return fmt.Errorf("failed to set lock_timeout: %w", err)
	}

	renameOldSQL := fmt.Sprintf("ALTER TABLE %s.%s RENAME TO %s",
		quoteIdent(schema), quoteIdent(oldTable), quoteIdent(tempName))
	if _, err := tx.Exec(ctx, renameOldSQL); err != nil {
		return err // returned as-is so isLockTimeoutError can inspect the SQLSTATE
	}

	renameNewSQL := fmt.Sprintf("ALTER TABLE %s.%s RENAME TO %s",
		quoteIdent(schema), quoteIdent(newTable), quoteIdent(oldTable))
	if _, err := tx.Exec(ctx, renameNewSQL); err != nil {
		return err
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit swap transaction: %w", err)
	}
	return nil
}

// isLockTimeoutError reports whether err is a PostgreSQL error caused by
// lock_timeout (SQLSTATE 55P03) — the only condition under which Swap retries.
func isLockTimeoutError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == lockTimeoutSQLState
	}
	return false
}

// quoteIdent applies simple escaping for identifiers in DDL statements
// (where parameters cannot be bound). Same logic as
// internal/reaper.quoteIdent and internal/ddlflow.quoteIdent; moving all
// three to a shared internal/dbutil package is recommended later (TODO).
func quoteIdent(ident string) string {
	return `"` + strings.ReplaceAll(ident, `"`, `""`) + `"`
}
