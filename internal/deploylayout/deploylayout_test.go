package deploylayout

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestValidateEcosystemRoot_RelativePath_Rejected(t *testing.T) {
	if err := ValidateEcosystemRoot("relative/ArchiOrbitLabs"); err == nil {
		t.Error("expected a relative path to be rejected")
	}
}

func TestValidateEcosystemRoot_WrongFinalDirName_Rejected(t *testing.T) {
	root := filepath.Join(t.TempDir(), "SomeOtherName")
	if err := ValidateEcosystemRoot(root); err == nil {
		t.Error("expected a root not ending in ArchiOrbitLabs to be rejected")
	}
}

// TestValidateEcosystemRoot_CaseInsensitiveMatch_Accepted is the direct
// regression test for AOL-STD-DEPLOY-001 Section 4.2's own "compared
// case-insensitively where the platform requires it" rule.
func TestValidateEcosystemRoot_CaseInsensitiveMatch_Accepted(t *testing.T) {
	root := filepath.Join(t.TempDir(), "archiorbitlabs")
	if err := ValidateEcosystemRoot(root); err != nil {
		t.Errorf("expected a case-insensitive match to be accepted, got: %v", err)
	}
}

func TestValidateEcosystemRoot_ValidPath_Accepted(t *testing.T) {
	root := filepath.Join(t.TempDir(), EcosystemRootDirName)
	if err := ValidateEcosystemRoot(root); err != nil {
		t.Errorf("expected a valid root to be accepted, got: %v", err)
	}
}

func TestValidateEcosystemRoot_UnixFilesystemRoot_Rejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix-specific")
	}
	if err := ValidateEcosystemRoot("/"); err == nil {
		t.Error("expected the filesystem root '/' to be rejected")
	}
}

func TestProductPath_JoinsExpectedSegments(t *testing.T) {
	root := filepath.Join(t.TempDir(), EcosystemRootDirName)
	got, err := ProductPath(root)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := filepath.Join(root, "Platforms", "PostgreSQL", "pgArchiMigrator")
	if got != want {
		t.Errorf("expected %q, got %q", want, got)
	}
}

// TestProductPath_InvalidRoot_PropagatesValidationError confirms
// ProductPath doesn't silently build a path from an invalid root — it
// must fail the same way ValidateEcosystemRoot itself would.
func TestProductPath_InvalidRoot_PropagatesValidationError(t *testing.T) {
	_, err := ProductPath("not-an-absolute-path")
	if err == nil {
		t.Error("expected an error for an invalid ecosystem root")
	}
}

func TestDefaultEcosystemRoot_ReturnsAValidRoot(t *testing.T) {
	root, err := DefaultEcosystemRoot()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := ValidateEcosystemRoot(root); err != nil {
		t.Errorf("expected DefaultEcosystemRoot's own output to itself pass ValidateEcosystemRoot, got: %v", err)
	}
}

func TestEnsureProductDirectories_CreatesExpectedSubdirectories(t *testing.T) {
	base := filepath.Join(t.TempDir(), EcosystemRootDirName, "Platforms", "PostgreSQL", "pgArchiMigrator")
	if err := EnsureProductDirectories(base); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, sub := range []string{"state", "logs", "migrations"} {
		info, err := os.Stat(filepath.Join(base, sub))
		if err != nil {
			t.Errorf("expected subdirectory %q to exist: %v", sub, err)
			continue
		}
		if !info.IsDir() {
			t.Errorf("expected %q to be a directory", sub)
		}
	}
}

// TestEnsureProductDirectories_DoesNotTouchExistingFiles is the direct
// regression test for AOL-STD-DEPLOY-001 Section 4.2's "Existing
// installations MUST NOT be silently overwritten" rule — creating the
// directory skeleton must never disturb a file already present.
func TestEnsureProductDirectories_DoesNotTouchExistingFiles(t *testing.T) {
	base := filepath.Join(t.TempDir(), EcosystemRootDirName, "Platforms", "PostgreSQL", "pgArchiMigrator")
	if err := os.MkdirAll(filepath.Join(base, "state"), 0o700); err != nil {
		t.Fatalf("test setup failed: %v", err)
	}
	marker := filepath.Join(base, "state", "existing-file.db")
	if err := os.WriteFile(marker, []byte("do-not-touch"), 0o600); err != nil {
		t.Fatalf("test setup failed: %v", err)
	}

	if err := EnsureProductDirectories(base); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	content, err := os.ReadFile(marker)
	if err != nil {
		t.Fatalf("expected the existing file to survive: %v", err)
	}
	if string(content) != "do-not-touch" {
		t.Error("expected the existing file's content to be unchanged")
	}
}
