package ddlflow

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// These are the fast, DB-independent counterpart to
// lock_timeout_integration_test.go's real-PostgreSQL tests — pure logic
// coverage for isLockTimeoutError that runs as part of `go test ./...`
// with no docker-compose dependency.
func TestIsLockTimeoutError_RecognizesPgErrorWithCode55P03(t *testing.T) {
	err := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	if !isLockTimeoutError(err) {
		t.Error("expected a PgError with Code 55P03 to be recognized as a lock timeout")
	}
}

func TestIsLockTimeoutError_RejectsOtherPgErrorCodes(t *testing.T) {
	cases := []string{
		"42P01", // undefined_table
		"23505", // unique_violation
		"57014", // query_canceled (a plain statement_timeout, NOT lock_timeout — a real, easy mix-up)
	}
	for _, code := range cases {
		t.Run(code, func(t *testing.T) {
			err := &pgconn.PgError{Code: code, Message: "some other failure"}
			if isLockTimeoutError(err) {
				t.Errorf("expected SQLSTATE %s to NOT be recognized as a lock timeout", code)
			}
		})
	}
}

func TestIsLockTimeoutError_RejectsNonPgErrors(t *testing.T) {
	if isLockTimeoutError(errors.New("some ordinary Go error")) {
		t.Error("expected a plain (non-pgconn.PgError) error to never be classified as a lock timeout")
	}
}

func TestIsLockTimeoutError_RejectsNilError(t *testing.T) {
	if isLockTimeoutError(nil) {
		t.Error("expected a nil error to never be classified as a lock timeout")
	}
}

// A wrapped PgError (fmt.Errorf("...: %w", pgErr)) must still be
// recognized — execOnceWithLockTimeout returns the raw driver error
// as-is today, but this guards against a future refactor that wraps it
// (matching this codebase's near-universal fmt.Errorf("%w", err)
// convention) without remembering errors.As needs to still be able to
// unwrap through to the underlying PgError.
func TestIsLockTimeoutError_RecognizesAWrappedPgError(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	wrapped := fmt.Errorf("failed to run DDL: %w", pgErr)
	if !isLockTimeoutError(wrapped) {
		t.Error("expected errors.As to see through a %w-wrapped error to the underlying PgError")
	}
}
