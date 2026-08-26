package api

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
)

// These tests are a direct regression guard for a real finding from a
// security self-audit: writeError used to send err.Error() to the client
// verbatim for EVERY status code, including 500 — meaning an internal
// failure (a raw pgx connection error, containing host/port/database
// details) was returned as-is to any authenticated caller, down to the
// lowest-privilege RoleViewer tier.
func TestWriteError_InternalServerError_DoesNotLeakTheRealMessage(t *testing.T) {
	rec := httptest.NewRecorder()
	sensitiveErr := errors.New("failed to connect to `host=10.0.4.17 port=5432 user=admin dbname=prod_orders`: connection refused")

	writeError(rec, 500, sensitiveErr)

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if body["error"] == sensitiveErr.Error() {
		t.Fatal("CRITICAL: the real internal error (with host/port/database details) was sent to the client")
	}
	if body["error"] != "an internal error occurred; check server logs for details" {
		t.Errorf("unexpected generic message: %q", body["error"])
	}
}

// Every OTHER status code is a deliberate exception to the above: these
// are caller-facing validation/business-logic errors that are both safe
// and genuinely useful to show in full ("email is required", "job not
// found") — the policy split is specifically by status code (500 vs
// everything else), not a blanket "never show error text" rule.
func TestWriteError_NonInternalStatusCodes_StillShowTheRealMessage(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"400 Bad Request", 400},
		{"401 Unauthorized", 401},
		{"403 Forbidden", 403},
		{"404 Not Found", 404},
		{"409 Conflict", 409},
		{"422 Unprocessable Entity", 422},
		{"429 Too Many Requests", 429},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			callerErr := errors.New("email is required")

			writeError(rec, tc.status, callerErr)

			var body map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("invalid JSON response: %v", err)
			}
			if body["error"] != "email is required" {
				t.Errorf("expected the real validation message to pass through unchanged, got %q", body["error"])
			}
		})
	}
}

func TestWriteError_NilError_DoesNotPanic(t *testing.T) {
	rec := httptest.NewRecorder()
	writeError(rec, 500, nil) // must not panic on a nil error

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if body["error"] == "" {
		t.Error("expected some non-empty error message even for a nil error")
	}
}
