package reaper

import (
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// The fast, DB-independent counterpart to internal/ddlflow's identical
// tests — internal/reaper duplicates the same execDDLWithLockTimeout /
// isLockTimeoutError pattern (see that package's doc comment for why:
// the reaper's own DDL, finalizing a DROP_COLUMN or cleaning up an
// orphaned shadow table, targets the same live tables and needs the same
// protection).
func TestIsLockTimeoutError_RecognizesPgErrorWithCode55P03(t *testing.T) {
	err := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	if !isLockTimeoutError(err) {
		t.Error("expected a PgError with Code 55P03 to be recognized as a lock timeout")
	}
}

func TestIsLockTimeoutError_RejectsOtherPgErrorCodes(t *testing.T) {
	cases := []string{"42P01", "23505", "57014"}
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

func TestIsLockTimeoutError_RecognizesAWrappedPgError(t *testing.T) {
	pgErr := &pgconn.PgError{Code: "55P03", Message: "canceling statement due to lock timeout"}
	wrapped := fmt.Errorf("failed to run DDL: %w", pgErr)
	if !isLockTimeoutError(wrapped) {
		t.Error("expected errors.As to see through a %w-wrapped error to the underlying PgError")
	}
}
