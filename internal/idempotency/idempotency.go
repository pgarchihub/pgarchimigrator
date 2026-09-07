// Package idempotency implements AC-PF-003 Section 12.3's own
// "Idempotency-Key" header — AOL-STD-API-001's Common API Contract also
// lists this among its recommended headers, and both agree on the same
// underlying semantics this package implements: "a retry for the same
// logical request uses the same idempotency key" (AC-PF-003 Section
// 12.2). Concretely: two requests carrying the same Idempotency-Key
// value must produce the SAME result — the second one returns whatever
// the first one already produced, rather than performing the action
// again (e.g. starting a second, duplicate migration or upgrade job).
//
// This is deliberately its own small package, not folded into
// internal/ecosystem — it's a generic HTTP-layer concern (works for any
// handler, not specifically migration/upgrade events), matching
// internal/auth's own "deliberately generic, no
// pgArchiMigrator-specific concept" precedent for the identical
// reasoning.
package idempotency

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by Store.Get when no record exists for a key.
var ErrNotFound = errors.New("not found")

// ErrAlreadyExists is returned by Store.Put when key was already
// recorded — see Put's own doc comment for the race this resolves.
var ErrAlreadyExists = errors.New("idempotency key already recorded")

// Record is what gets replayed for a repeated request — the complete
// HTTP response the FIRST request with this key produced, so a retry
// sees exactly what the original caller saw, not a re-derived
// approximation of it.
type Record struct {
	Key          string
	StatusCode   int
	ResponseBody []byte
	CreatedAt    time.Time
}

// Store persists idempotency records. Real deployment needs a
// PERSISTENT implementation — a process restart must not forget which
// keys were already used, or a retry after a restart could silently
// duplicate the original action, precisely what this package exists to
// prevent.
type Store interface {
	// Get retrieves a previously stored record for key, or ErrNotFound.
	Get(ctx context.Context, key string) (*Record, error)
	// Put atomically stores record — implementations MUST make this a
	// single atomic insert-if-absent operation (e.g. a UNIQUE
	// constraint, not a separate check-then-insert), so two concurrent
	// requests carrying the same brand-new key can't both observe "not
	// yet used" and both proceed to perform the action — the exact
	// same atomicity requirement agentattestation.NonceStore.MarkUsed
	// already documents, for the identical race.
	//
	// Put returns ErrAlreadyExists if key was already recorded by a
	// concurrent request that won the race — the caller (see
	// Middleware) is expected to re-fetch via Get in that case rather
	// than treating it as a hard failure.
	Put(ctx context.Context, record *Record) error
}
