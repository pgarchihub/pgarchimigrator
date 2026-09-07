package main

import (
	"os"
	"path/filepath"
	"testing"
)

func setupArtifact(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "test-artifact.tar.gz")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("failed to write test artifact: %v", err)
	}
	return path
}

// TestSignAndVerify_RoundTrip is the direct end-to-end proof this whole
// tool exists to provide: genkey -> sign -> verify succeeds for an
// untampered artifact against the matching public key.
func TestSignAndVerify_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "keys")

	if err := runGenkey([]string{"--out-dir", keyDir}); err != nil {
		t.Fatalf("genkey failed: %v", err)
	}

	artifact := setupArtifact(t, dir, "pretend artifact bytes")
	if err := runSign([]string{
		"--artifact", artifact,
		"--key", filepath.Join(keyDir, "ed25519-private.key"),
		"--key-id", "archiorbitlabs-release-test",
		"--product-id", "pgarchimigrator",
		"--version", "2.0.0",
	}); err != nil {
		t.Fatalf("sign failed: %v", err)
	}

	if err := runVerify([]string{
		"--descriptor", artifact + ".descriptor.json",
		"--pubkey", filepath.Join(keyDir, "ed25519-public.key"),
		"--artifact", artifact,
	}); err != nil {
		t.Fatalf("expected verify to succeed for an untampered artifact, got: %v", err)
	}
}

// TestVerify_TamperedArtifact_Rejected is THE critical negative test —
// AOL-STD-DEPLOY-001 §5.1's own "mutated bytes ... MUST be rejected"
// requirement. A signature-verification tool that fails to catch this
// is worse than useless: it would give false confidence in a corrupted
// or maliciously modified artifact.
func TestVerify_TamperedArtifact_Rejected(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "keys")
	_ = runGenkey([]string{"--out-dir", keyDir})

	artifact := setupArtifact(t, dir, "original bytes")
	_ = runSign([]string{
		"--artifact", artifact, "--key", filepath.Join(keyDir, "ed25519-private.key"),
		"--key-id", "test-key", "--version", "2.0.0",
	})

	// Tamper with the artifact AFTER signing — the descriptor's own
	// sha256 field still reflects the original content.
	if err := os.WriteFile(artifact, []byte("TAMPERED bytes"), 0o644); err != nil {
		t.Fatalf("test setup failed: %v", err)
	}

	err := runVerify([]string{
		"--descriptor", artifact + ".descriptor.json",
		"--pubkey", filepath.Join(keyDir, "ed25519-public.key"),
		"--artifact", artifact,
	})
	if err == nil {
		t.Fatal("expected verify to REJECT a tampered artifact, but it succeeded")
	}
}

// TestVerify_WrongPublicKey_Rejected is the second critical negative
// test — a descriptor signed with one key must not verify against a
// DIFFERENT key, even a validly-generated one (guards against a
// confused-deputy scenario: accepting any well-formed signature
// regardless of which key actually produced it).
func TestVerify_WrongPublicKey_Rejected(t *testing.T) {
	dir := t.TempDir()
	realKeyDir := filepath.Join(dir, "real-keys")
	wrongKeyDir := filepath.Join(dir, "wrong-keys")
	_ = runGenkey([]string{"--out-dir", realKeyDir})
	_ = runGenkey([]string{"--out-dir", wrongKeyDir})

	artifact := setupArtifact(t, dir, "artifact bytes")
	_ = runSign([]string{
		"--artifact", artifact, "--key", filepath.Join(realKeyDir, "ed25519-private.key"),
		"--key-id", "test-key", "--version", "2.0.0",
	})

	err := runVerify([]string{
		"--descriptor", artifact + ".descriptor.json",
		"--pubkey", filepath.Join(wrongKeyDir, "ed25519-public.key"), // wrong key
	})
	if err == nil {
		t.Fatal("expected verify to REJECT a signature checked against the wrong public key, but it succeeded")
	}
}

// TestVerify_TamperedDescriptorField_Rejected confirms the signature
// covers every field signingBytes says it does — not just the sha256.
// Changing productId or version after signing (without re-signing) must
// also fail, since the signature is over the whole descriptor, not just
// the digest.
func TestVerify_TamperedDescriptorField_Rejected(t *testing.T) {
	dir := t.TempDir()
	keyDir := filepath.Join(dir, "keys")
	_ = runGenkey([]string{"--out-dir", keyDir})

	artifact := setupArtifact(t, dir, "artifact bytes")
	_ = runSign([]string{
		"--artifact", artifact, "--key", filepath.Join(keyDir, "ed25519-private.key"),
		"--key-id", "test-key", "--product-id", "pgarchimigrator", "--version", "2.0.0",
	})

	descriptorPath := artifact + ".descriptor.json"
	raw, err := os.ReadFile(descriptorPath)
	if err != nil {
		t.Fatalf("test setup failed: %v", err)
	}
	// Swap the version field to a different value, leaving the
	// signature (computed over the ORIGINAL version) unchanged.
	tampered := []byte(replaceOnce(string(raw), `"version": "2.0.0"`, `"version": "9.9.9"`))
	if err := os.WriteFile(descriptorPath, tampered, 0o644); err != nil {
		t.Fatalf("test setup failed: %v", err)
	}

	err = runVerify([]string{
		"--descriptor", descriptorPath,
		"--pubkey", filepath.Join(keyDir, "ed25519-public.key"),
	})
	if err == nil {
		t.Fatal("expected verify to REJECT a descriptor whose version field was changed after signing, but it succeeded")
	}
}

func replaceOnce(s, old, new string) string {
	i := indexOf(s, old)
	if i < 0 {
		return s
	}
	return s[:i] + new + s[i+len(old):]
}

func indexOf(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}
