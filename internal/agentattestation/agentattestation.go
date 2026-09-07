// Package agentattestation verifies the nonce-bound Ed25519 agent
// attestations AOL-STD-API-001 §6.3 and AOL-STD-DEPLOY-001 §7.1 require
// before a certified installer adapter acts on ArchiConsole's behalf.
//
// This package deliberately implements ONLY the verification side —
// checking a signature and enforcing single-use — never the enrollment,
// challenge-issuance, or nonce-generation side. Those are ArchiConsole's
// own Experience API responsibility (AOL-STD-API-001 §9.4:
// POST /api/v1/agents/enrollment, POST /api/v1/agents/challenges,
// POST /api/v1/agents/attestations, POST /api/v1/agents/embedded/attest
// are all listed as ArchiConsole routes, not a per-product contract).
// pgArchiMigrator's own installer adapter receives an attestation that
// ArchiConsole already produced through that flow and must confirm it's
// genuine before treating it as authorization for a local install
// action — this package is that confirmation step, nothing more.
package agentattestation

import (
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidSignature is returned when an attestation's signature
// doesn't verify against the given public key.
var ErrInvalidSignature = errors.New("attestation signature is invalid")

// ErrExpired is returned when an attestation's IssuedAt is older than
// the caller's own maxAge tolerance — see Verify.
var ErrExpired = errors.New("attestation has expired")

// ErrNonceReused is returned when a nonce has already been consumed —
// AOL-STD-API-001 §6.3's "nonce-bound" requirement exists specifically
// to prevent an attestation from being captured and replayed; this is
// the enforcement of that property.
var ErrNonceReused = errors.New("attestation nonce has already been used")

// Attestation is one agent's proof of possession of its enrolled
// private key over a specific, ArchiConsole-issued nonce.
type Attestation struct {
	AgentID   string
	Nonce     string
	IssuedAt  time.Time
	Signature string // hex-encoded Ed25519 signature over signingBytes
}

// signingBytes returns the exact, deterministic bytes an attestation's
// signature covers — AgentID, Nonce, and IssuedAt (RFC3339Nano, UTC),
// in a fixed order. Mirrors cmd/pgarchisign's own signingBytes function
// in spirit (a fixed field order and delimiter, never "sign the JSON
// encoding") for the identical reasoning: a signature's input bytes
// must never depend on an incidental, unguaranteed detail like
// encoding/json's field ordering.
func signingBytes(a Attestation) []byte {
	return []byte(fmt.Sprintf("%s\n%s\n%s", a.AgentID, a.Nonce, a.IssuedAt.UTC().Format(time.RFC3339Nano)))
}

// VerifySignature checks ONLY the Ed25519 signature — no freshness or
// nonce-reuse check, see Verify for the complete check a caller should
// actually use. Exported separately because a caller occasionally needs
// signature validity alone (e.g. an audit tool re-checking a historical,
// already-consumed attestation, where re-enforcing "not yet used" would
// be nonsensical).
func VerifySignature(pubKey ed25519.PublicKey, a Attestation) error {
	sig, err := hex.DecodeString(a.Signature)
	if err != nil {
		return fmt.Errorf("attestation signature is not valid hex: %w", err)
	}
	if !ed25519.Verify(pubKey, signingBytes(a), sig) {
		return ErrInvalidSignature
	}
	return nil
}

// NonceStore tracks which nonces have already been consumed, enforcing
// single-use. A real deployment needs a PERSISTENT implementation (a
// process restart must not forget which nonces were already spent,
// or replay protection silently stops working across restarts) —
// MemoryNonceStore below is deliberately only for tests/development;
// see MemoryNonceStore's own doc comment.
type NonceStore interface {
	// MarkUsed atomically checks whether nonce was already recorded
	// and, if not, records it — implementations MUST make the
	// check-and-record a single atomic operation (e.g. a UNIQUE
	// constraint in SQL, not a separate SELECT then INSERT), or two
	// concurrent verification attempts for the same replayed
	// attestation could both observe "not yet used" and both succeed.
	MarkUsed(ctx context.Context, nonce string) error
}

// MemoryNonceStore is an in-process, non-persistent NonceStore — useful
// for tests and for a single short-lived process, but NOT a substitute
// for a real persistent store in production: restarting the process
// forgets every nonce it had already seen, silently reopening the
// replay window this whole package exists to close. A SQLite-backed
// implementation (mirroring internal/serviceauth.SQLiteStore's own
// pattern — its own separate database file, following this project's
// established "isolate this package's write path" precedent) is the
// natural next step once this is wired into a real installer adapter;
// not yet built.
type MemoryNonceStore struct {
	used map[string]bool
}

func NewMemoryNonceStore() *MemoryNonceStore {
	return &MemoryNonceStore{used: make(map[string]bool)}
}

func (m *MemoryNonceStore) MarkUsed(ctx context.Context, nonce string) error {
	if m.used == nil {
		m.used = make(map[string]bool)
	}
	if m.used[nonce] {
		return ErrNonceReused
	}
	m.used[nonce] = true
	return nil
}

// Verify performs the complete check AOL-STD-API-001 §6.3 requires
// before an attestation may be trusted: a valid signature, issued
// within maxAge of now, and a nonce that has never been consumed
// before (checked and recorded atomically via store).
//
// Order matters: nonce consumption happens LAST, only after signature
// and freshness both already passed — an attacker presenting a
// forged or expired attestation must never be able to burn a
// legitimate agent's real nonce as a side effect of a failed attempt.
func Verify(ctx context.Context, pubKey ed25519.PublicKey, a Attestation, store NonceStore, maxAge time.Duration) error {
	if err := VerifySignature(pubKey, a); err != nil {
		return err
	}
	if time.Since(a.IssuedAt) > maxAge {
		return ErrExpired
	}
	if err := store.MarkUsed(ctx, a.Nonce); err != nil {
		return err
	}
	return nil
}
