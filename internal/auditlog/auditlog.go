// Package auditlog implements the "Audit Log" from Architecture Doc Section
// 3.3 and the "Audit Trail" requirement from Section 6: every command and
// decision is recorded in JSON format with Who/When/What/Result fields
// (TR-07). Secrets are never logged (TR-05).
package auditlog

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Entry represents a single audit record.
type Entry struct {
	Timestamp time.Time      `json:"timestamp"`
	JobID     string         `json:"job_id"`
	Actor     string         `json:"actor"`  // "who" - the user or CI/CD pipeline identity
	Action    string         `json:"action"` // "what" - e.g. "SWAP_ATTEMPT", "ROLLBACK"
	Result    string         `json:"result"` // "SUCCESS" | "FAILURE" | "RETRY"
	Detail    map[string]any `json:"detail,omitempty"`
}

// Writer is the interface responsible for persisting audit records.
type Writer interface {
	Write(ctx context.Context, e Entry) error
}

// FileWriter is a Writer that appends each Entry as a single line of JSON
// (JSON Lines / .jsonl format) to a file — simple, greppable, and safe to
// tail in production without needing a database. Safe for concurrent use.
type FileWriter struct {
	mu   sync.Mutex
	file *os.File
}

var _ Writer = (*FileWriter)(nil)

// NewFileWriter opens (creating if necessary, including parent
// directories) the file at path for appending.
func NewFileWriter(path string) (*FileWriter, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("failed to create audit log directory (%s): %w", dir, err)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("failed to open audit log file (%s): %w", path, err)
	}
	return &FileWriter{file: f}, nil
}

// Close closes the underlying file.
func (w *FileWriter) Close() error {
	return w.file.Close()
}

// Write appends a single JSON line. Entry.Detail is sanitized
// automatically before being marshaled — callers do not need to call
// Sanitize themselves.
func (w *FileWriter) Write(ctx context.Context, e Entry) error {
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	e.Detail = Sanitize(e.Detail)

	line, err := json.Marshal(e)
	if err != nil {
		return fmt.Errorf("failed to marshal audit entry: %w", err)
	}
	line = append(line, '\n')

	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.file.Write(line); err != nil {
		return fmt.Errorf("failed to write audit entry: %w", err)
	}
	return nil
}

// sensitiveKeyFragments are matched case-insensitively as substrings
// against Detail map keys. Deliberately broad (e.g. "dsn" catches both
// "dsn" and "database_dsn") since a false-positive redaction just means a
// harmless field is hidden, while a false negative could leak a secret —
// TR-05 makes that an asymmetric risk worth erring on the side of caution.
var sensitiveKeyFragments = []string{
	"password", "passwd", "secret", "token", "apikey", "api_key",
	"pgpassword", "dsn", "connectionstring", "connection_string",
	"credential", "authorization", "private_key", "privatekey",
}

// redactedValue is what a sanitized field's value is replaced with.
const redactedValue = "[REDACTED]"

// Sanitize returns a copy of detail with any value whose key looks like it
// might hold a secret replaced by redactedValue, per TR-05 (secrets must
// never be written to logs). The input map is never mutated.
func Sanitize(detail map[string]any) map[string]any {
	if detail == nil {
		return nil
	}

	out := make(map[string]any, len(detail))
	for k, v := range detail {
		if isSensitiveKey(k) {
			out[k] = redactedValue
			continue
		}
		// Recurse into nested maps so a secret buried inside a structured
		// value (e.g. detail["config"]["password"]) is still caught.
		if nested, ok := v.(map[string]any); ok {
			out[k] = Sanitize(nested)
			continue
		}
		out[k] = v
	}
	return out
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, fragment := range sensitiveKeyFragments {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}
