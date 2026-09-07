package agentattestation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

func newTestKeyPair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("failed to generate test key pair: %v", err)
	}
	return pub, priv
}

func signedAttestation(priv ed25519.PrivateKey, agentID, nonce string, issuedAt time.Time) Attestation {
	a := Attestation{AgentID: agentID, Nonce: nonce, IssuedAt: issuedAt}
	sig := ed25519.Sign(priv, signingBytes(a))
	a.Signature = hex.EncodeToString(sig)
	return a
}

func TestVerifySignature_ValidAttestation_Succeeds(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	a := signedAttestation(priv, "agent-1", "nonce-abc", time.Now())

	if err := VerifySignature(pub, a); err != nil {
		t.Errorf("expected a valid signature to verify, got: %v", err)
	}
}

// TestVerifySignature_WrongPublicKey_Rejected mirrors
// cmd/pgarchisign's own identical negative test — a confused-deputy
// guard: an attestation signed by one agent's key must not verify
// against a DIFFERENT agent's public key.
func TestVerifySignature_WrongPublicKey_Rejected(t *testing.T) {
	_, priv := newTestKeyPair(t)
	wrongPub, _ := newTestKeyPair(t)
	a := signedAttestation(priv, "agent-1", "nonce-abc", time.Now())

	err := VerifySignature(wrongPub, a)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature, got: %v", err)
	}
}

// TestVerifySignature_TamperedField_Rejected confirms the signature
// covers every field signingBytes says it does — changing AgentID
// after signing (without re-signing) must invalidate the signature.
func TestVerifySignature_TamperedField_Rejected(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	a := signedAttestation(priv, "agent-1", "nonce-abc", time.Now())
	a.AgentID = "agent-2" // tampered after signing

	err := VerifySignature(pub, a)
	if !errors.Is(err, ErrInvalidSignature) {
		t.Errorf("expected ErrInvalidSignature for a tampered field, got: %v", err)
	}
}

func TestVerify_FullCheck_ValidAttestation_Succeeds(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	a := signedAttestation(priv, "agent-1", "nonce-abc", time.Now())
	store := NewMemoryNonceStore()

	if err := Verify(context.Background(), pub, a, store, 5*time.Minute); err != nil {
		t.Errorf("expected a fresh, validly-signed, unused-nonce attestation to succeed, got: %v", err)
	}
}

func TestVerify_ExpiredAttestation_Rejected(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	a := signedAttestation(priv, "agent-1", "nonce-abc", time.Now().Add(-10*time.Minute))
	store := NewMemoryNonceStore()

	err := Verify(context.Background(), pub, a, store, 5*time.Minute)
	if !errors.Is(err, ErrExpired) {
		t.Errorf("expected ErrExpired, got: %v", err)
	}
}

// TestVerify_ReplayedNonce_Rejected is the direct regression test for
// this whole package's core promise — AOL-STD-API-001 §6.3's
// "nonce-bound" requirement. A second attestation using the SAME nonce
// (even if otherwise perfectly valid — a genuine agent replaying its
// own prior, legitimate attestation) must be rejected.
func TestVerify_ReplayedNonce_Rejected(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	store := NewMemoryNonceStore()
	ctx := context.Background()

	first := signedAttestation(priv, "agent-1", "nonce-abc", time.Now())
	if err := Verify(ctx, pub, first, store, 5*time.Minute); err != nil {
		t.Fatalf("expected the first use of this nonce to succeed, got: %v", err)
	}

	// A second, freshly-signed attestation reusing the SAME nonce.
	second := signedAttestation(priv, "agent-1", "nonce-abc", time.Now())
	err := Verify(ctx, pub, second, store, 5*time.Minute)
	if !errors.Is(err, ErrNonceReused) {
		t.Errorf("expected ErrNonceReused on replay, got: %v", err)
	}
}

// TestVerify_ExpiredAttestation_DoesNotConsumeNonce is the direct
// regression test for Verify's own documented ordering guarantee: a
// FAILED verification (here, due to expiry) must never burn the nonce
// — otherwise an attacker could invalidate a legitimate agent's not-yet
// -used nonce merely by replaying an old, expired copy of it, denying
// the real agent the ability to use its own still-fresh credential.
func TestVerify_ExpiredAttestation_DoesNotConsumeNonce(t *testing.T) {
	pub, priv := newTestKeyPair(t)
	store := NewMemoryNonceStore()
	ctx := context.Background()

	expired := signedAttestation(priv, "agent-1", "nonce-abc", time.Now().Add(-10*time.Minute))
	if err := Verify(ctx, pub, expired, store, 5*time.Minute); !errors.Is(err, ErrExpired) {
		t.Fatalf("expected ErrExpired for the first (expired) attempt, got: %v", err)
	}

	// The SAME nonce, now presented fresh — must still succeed, proving
	// the expired attempt above never marked it used.
	fresh := signedAttestation(priv, "agent-1", "nonce-abc", time.Now())
	if err := Verify(ctx, pub, fresh, store, 5*time.Minute); err != nil {
		t.Errorf("expected the nonce to still be unused after a prior EXPIRED (not replayed) attempt, got: %v", err)
	}
}

func TestMemoryNonceStore_MarkUsed_SecondCallRejected(t *testing.T) {
	store := NewMemoryNonceStore()
	ctx := context.Background()

	if err := store.MarkUsed(ctx, "n1"); err != nil {
		t.Fatalf("unexpected error on first use: %v", err)
	}
	if err := store.MarkUsed(ctx, "n1"); !errors.Is(err, ErrNonceReused) {
		t.Errorf("expected ErrNonceReused on second use, got: %v", err)
	}
}

func TestMemoryNonceStore_DifferentNonces_BothSucceed(t *testing.T) {
	store := NewMemoryNonceStore()
	ctx := context.Background()

	if err := store.MarkUsed(ctx, "n1"); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
	if err := store.MarkUsed(ctx, "n2"); err != nil {
		t.Errorf("unexpected error for a distinct nonce: %v", err)
	}
}
