package runtimeverify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, content string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("test setup failed: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("test setup failed: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func TestVerifyInventory_MatchingInstallation_Succeeds(t *testing.T) {
	dir := t.TempDir()
	binSum := writeFile(t, filepath.Join(dir, "bin", "pgarchimigrator"), "binary content")
	cfgSum := writeFile(t, filepath.Join(dir, "config", "defaults.json"), "{}")

	manifest := InventoryManifest{Files: []FileEntry{
		{RelativePath: "bin/pgarchimigrator", SHA256: binSum},
		{RelativePath: "config/defaults.json", SHA256: cfgSum},
	}}

	if err := VerifyInventory(dir, manifest); err != nil {
		t.Errorf("expected a matching installation to verify, got: %v", err)
	}
}

func TestVerifyInventory_MissingFile_Rejected(t *testing.T) {
	dir := t.TempDir()
	binSum := writeFile(t, filepath.Join(dir, "bin", "pgarchimigrator"), "binary content")

	manifest := InventoryManifest{Files: []FileEntry{
		{RelativePath: "bin/pgarchimigrator", SHA256: binSum},
		{RelativePath: "config/defaults.json", SHA256: "never-written"}, // never actually created
	}}

	err := VerifyInventory(dir, manifest)
	if !errors.Is(err, ErrMissingFile) {
		t.Errorf("expected ErrMissingFile, got: %v", err)
	}
}

// TestVerifyInventory_ChecksumMismatch_Rejected is the direct
// regression test for AOL-STD-DEPLOY-001 §9's "verify staged inventory"
// step actually catching drift/tampering — a file present at the right
// path but with different CONTENT than what was shipped.
func TestVerifyInventory_ChecksumMismatch_Rejected(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bin", "pgarchimigrator"), "original content")

	manifest := InventoryManifest{Files: []FileEntry{
		{RelativePath: "bin/pgarchimigrator", SHA256: "0000000000000000000000000000000000000000000000000000000000000"}, // wrong on purpose
	}}

	err := VerifyInventory(dir, manifest)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Errorf("expected ErrChecksumMismatch, got: %v", err)
	}
}

// TestVerifyInventory_UnexpectedExtraFile_Rejected is the direct
// regression test for this package's own doc comment on why "no
// unexpected file" matters as much as checksum matching — a maliciously
// (or accidentally) planted extra file, with every ORIGINAL file
// completely untouched, must still be caught.
func TestVerifyInventory_UnexpectedExtraFile_Rejected(t *testing.T) {
	dir := t.TempDir()
	binSum := writeFile(t, filepath.Join(dir, "bin", "pgarchimigrator"), "binary content")
	// An extra file the manifest never mentions.
	writeFile(t, filepath.Join(dir, "bin", "sneaky-extra-binary"), "not supposed to be here")

	manifest := InventoryManifest{Files: []FileEntry{
		{RelativePath: "bin/pgarchimigrator", SHA256: binSum},
	}}

	err := VerifyInventory(dir, manifest)
	if !errors.Is(err, ErrUnexpectedFile) {
		t.Errorf("expected ErrUnexpectedFile, got: %v", err)
	}
}

func TestResolveEntrypoint_ValidEntrypoint_ReturnsAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "bin", "pgarchimigrator"), "binary content")

	manifest := InventoryManifest{AllowedEntrypoint: "bin/pgarchimigrator"}
	got, err := ResolveEntrypoint(dir, manifest)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(dir, "bin", "pgarchimigrator")
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// TestResolveEntrypoint_PathTraversal_Rejected is THE critical negative
// test for this function — AOL-STD-DEPLOY-001 §7.2's "MUST NOT accept
// ... arbitrary executable paths" requirement, enforced directly: a
// manifest (however it got there) declaring an AllowedEntrypoint that
// escapes the installation directory must never resolve to a path
// outside it.
func TestResolveEntrypoint_PathTraversal_Rejected(t *testing.T) {
	dir := t.TempDir()
	// Something outside dir that a traversal might otherwise reach.
	outsideFile := filepath.Join(filepath.Dir(dir), "outside-target")
	writeFile(t, outsideFile, "should never be reachable")
	defer os.Remove(outsideFile)

	manifest := InventoryManifest{AllowedEntrypoint: "../" + filepath.Base(outsideFile)}
	_, err := ResolveEntrypoint(dir, manifest)
	if !errors.Is(err, ErrEntrypointEscapesInstallation) {
		t.Errorf("expected ErrEntrypointEscapesInstallation, got: %v", err)
	}
}

func TestResolveEntrypoint_NonexistentEntrypoint_Rejected(t *testing.T) {
	dir := t.TempDir()
	manifest := InventoryManifest{AllowedEntrypoint: "bin/does-not-exist"}
	_, err := ResolveEntrypoint(dir, manifest)
	if err == nil {
		t.Error("expected an error for a nonexistent entrypoint")
	}
}

func TestResolveEntrypoint_EntrypointIsADirectory_Rejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatalf("test setup failed: %v", err)
	}
	manifest := InventoryManifest{AllowedEntrypoint: "bin"}
	_, err := ResolveEntrypoint(dir, manifest)
	if err == nil {
		t.Error("expected an error when AllowedEntrypoint points at a directory")
	}
}

func TestResolveEntrypoint_EmptyAllowedEntrypoint_Rejected(t *testing.T) {
	dir := t.TempDir()
	_, err := ResolveEntrypoint(dir, InventoryManifest{})
	if err == nil {
		t.Error("expected an error for an empty AllowedEntrypoint")
	}
}
