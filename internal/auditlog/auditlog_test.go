package auditlog

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitize_RedactsSensitiveKeys(t *testing.T) {
	detail := map[string]any{
		"schema_name":  "public",
		"password":     "hunter2",
		"PGPASSWORD":   "hunter2",
		"database_dsn": "postgresql://user:pass@host/db",
		"api_key":      "sk-abc123",
		"safe_field":   "keep-me",
		"row_count":    100,
	}

	got := Sanitize(detail)

	for _, key := range []string{"password", "PGPASSWORD", "database_dsn", "api_key"} {
		if got[key] != redactedValue {
			t.Errorf("expected %q to be redacted, got %v", key, got[key])
		}
	}
	if got["safe_field"] != "keep-me" {
		t.Errorf("expected safe_field to be preserved, got %v", got["safe_field"])
	}
	if got["schema_name"] != "public" {
		t.Errorf("expected schema_name to be preserved, got %v", got["schema_name"])
	}
	if got["row_count"] != 100 {
		t.Errorf("expected row_count to be preserved, got %v", got["row_count"])
	}
}

func TestSanitize_RedactsNestedMaps(t *testing.T) {
	detail := map[string]any{
		"config": map[string]any{
			"host":     "localhost",
			"password": "hunter2",
		},
	}

	got := Sanitize(detail)

	nested, ok := got["config"].(map[string]any)
	if !ok {
		t.Fatalf("expected config to still be a map, got %T", got["config"])
	}
	if nested["password"] != redactedValue {
		t.Errorf("expected nested password to be redacted, got %v", nested["password"])
	}
	if nested["host"] != "localhost" {
		t.Errorf("expected nested host to be preserved, got %v", nested["host"])
	}
}

func TestSanitize_DoesNotMutateInput(t *testing.T) {
	original := map[string]any{"password": "hunter2"}
	_ = Sanitize(original)

	if original["password"] != "hunter2" {
		t.Error("Sanitize must not mutate its input map")
	}
}

func TestSanitize_NilInput(t *testing.T) {
	if got := Sanitize(nil); got != nil {
		t.Errorf("expected nil for nil input, got %v", got)
	}
}

func TestFileWriter_WritesValidJSONLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := NewFileWriter(path)
	if err != nil {
		t.Fatalf("NewFileWriter failed: %v", err)
	}
	defer w.Close()

	entries := []Entry{
		{JobID: "job-1", Actor: "alice", Action: "MIGRATE_START", Result: "SUCCESS"},
		{JobID: "job-1", Actor: "alice", Action: "SWAP_ATTEMPT", Result: "SUCCESS", Detail: map[string]any{"attempt": 1}},
	}
	for _, e := range entries {
		if err := w.Write(context.Background(), e); err != nil {
			t.Fatalf("Write failed: %v", err)
		}
	}
	w.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read audit log file: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d", len(lines))
	}

	for i, line := range lines {
		var decoded Entry
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("line %d is not valid JSON: %v", i, err)
		}
		if decoded.JobID != "job-1" {
			t.Errorf("line %d: expected job_id='job-1', got %q", i, decoded.JobID)
		}
		if decoded.Timestamp.IsZero() {
			t.Errorf("line %d: expected a non-zero timestamp to be auto-populated", i)
		}
	}
}

func TestFileWriter_RedactsSecretsAutomatically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	w, err := NewFileWriter(path)
	if err != nil {
		t.Fatalf("NewFileWriter failed: %v", err)
	}
	defer w.Close()

	err = w.Write(context.Background(), Entry{
		JobID: "job-1", Action: "CONNECT", Result: "SUCCESS",
		Detail: map[string]any{"dsn": "postgresql://user:hunter2@host/db"},
	})
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	w.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read audit log file: %v", err)
	}
	if strings.Contains(string(data), "hunter2") {
		t.Error("the secret value leaked into the audit log file")
	}
	if !strings.Contains(string(data), redactedValue) {
		t.Error("expected the redacted placeholder to appear in the file")
	}
}

func TestFileWriter_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "dir", "audit.jsonl")
	w, err := NewFileWriter(path)
	if err != nil {
		t.Fatalf("NewFileWriter should have created the parent directory: %v", err)
	}
	defer w.Close()

	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected the audit log file to exist: %v", err)
	}
}

func TestFileWriter_AppendsAcrossMultipleWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")

	w1, err := NewFileWriter(path)
	if err != nil {
		t.Fatalf("NewFileWriter failed: %v", err)
	}
	if err := w1.Write(context.Background(), Entry{JobID: "job-1", Action: "A", Result: "SUCCESS"}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	w1.Close()

	w2, err := NewFileWriter(path) // simulates a new process starting later
	if err != nil {
		t.Fatalf("NewFileWriter failed: %v", err)
	}
	if err := w2.Write(context.Background(), Entry{JobID: "job-2", Action: "B", Result: "SUCCESS"}); err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	w2.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("could not read audit log file: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected entries from both writers to be present (2 lines), got %d", len(lines))
	}
}
