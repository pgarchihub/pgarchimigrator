// Package runtimeverify implements the two checks AOL-STD-DEPLOY-001
// §9 requires between "files were staged" and "the runtime may be
// activated":
//
//   - VerifyInventory (§9 step 5, "verify staged inventory and
//     permissions"): confirms every file the package's own manifest
//     expects is present with a matching SHA-256, and that no
//     unexpected extra file has appeared in the installed tree.
//   - ResolveEntrypoint (§9 step 7, "activate only the exact
//     allow-listed runtime entrypoint"): returns the ONE binary path a
//     caller may execute — never a path the caller constructs itself.
//
// Both exist for the same reason AOL-STD-DEPLOY-001 §7.2 states
// directly: "The adapter MUST NOT accept raw command text, shell
// fragments, arbitrary executable paths." A certified installer
// adapter's own code should have no path anywhere that runs
// "whatever binary happens to be in bin/" — it calls ResolveEntrypoint
// and executes exactly what that returns, nothing else, and only after
// VerifyInventory has already confirmed the installed tree matches
// what was actually shipped.
package runtimeverify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// ErrMissingFile is returned when a manifest-listed file is absent from
// the installed tree.
var ErrMissingFile = errors.New("expected file is missing from the installation")

// ErrChecksumMismatch is returned when an installed file's SHA-256
// doesn't match the manifest — AOL-STD-DEPLOY-001 §9's "drifted
// runtime/configuration files" scenario (also relevant to repair, §10),
// or, in the worst case, tampering.
var ErrChecksumMismatch = errors.New("installed file's checksum does not match the manifest")

// ErrUnexpectedFile is returned when a file exists in the installed
// tree that the manifest doesn't account for — see VerifyInventory's
// own doc comment for why this is treated as a failure, not a warning.
var ErrUnexpectedFile = errors.New("unexpected file found in the installation that is not in the manifest")

// ErrEntrypointEscapesInstallation is returned by ResolveEntrypoint if
// the manifest's own AllowedEntrypoint value would resolve outside
// installPath — see that function's own doc comment.
var ErrEntrypointEscapesInstallation = errors.New("allowed entrypoint path escapes the installation directory")

// FileEntry is one file a package's manifest expects to exist, with the
// SHA-256 it was shipped with.
type FileEntry struct {
	RelativePath string // relative to the installation root, forward-slash separated
	SHA256       string
}

// InventoryManifest is what a specific package release actually shipped
// — built once, at packaging time (see scripts/build-artifact.sh),
// alongside the artifact itself; a real caller loads this from the
// package's own recorded manifest rather than constructing it by hand.
type InventoryManifest struct {
	Files []FileEntry
	// AllowedEntrypoint is the ONE relative path (e.g. "bin/pgarchimigrator")
	// ResolveEntrypoint will ever return — see deploy/platforms/*.json's
	// own binaryName field for where this value comes from in practice.
	AllowedEntrypoint string
}

// VerifyInventory confirms installPath's actual contents match manifest
// exactly: every listed file present with a matching checksum, AND no
// file exists that the manifest doesn't list.
//
// The "no unexpected file" direction matters as much as the checksum
// check: AOL-STD-DEPLOY-001 §5.2's own "Executable files MUST be
// explicitly enumerated; installers MUST NOT execute files discovered
// dynamically from the package" only has teeth if something actually
// confirms nothing WAS dynamically added after packaging — a
// checksum-only check would happily approve an installation with every
// original file untouched PLUS a newly-planted extra executable,
// since none of the expected files' own checksums would have changed.
func VerifyInventory(installPath string, manifest InventoryManifest) error {
	expected := make(map[string]string, len(manifest.Files))
	for _, f := range manifest.Files {
		expected[filepath.ToSlash(f.RelativePath)] = f.SHA256
	}

	seen := make(map[string]bool, len(expected))
	err := filepath.WalkDir(installPath, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(installPath, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)

		wantSum, ok := expected[rel]
		if !ok {
			return fmt.Errorf("%w: %s", ErrUnexpectedFile, rel)
		}
		seen[rel] = true

		gotSum, err := sha256File(path)
		if err != nil {
			return fmt.Errorf("failed to hash %s: %w", rel, err)
		}
		if gotSum != wantSum {
			return fmt.Errorf("%w: %s (expected %s, got %s)", ErrChecksumMismatch, rel, wantSum, gotSum)
		}
		return nil
	})
	if err != nil {
		return err
	}

	for rel := range expected {
		if !seen[rel] {
			return fmt.Errorf("%w: %s", ErrMissingFile, rel)
		}
	}
	return nil
}

// ResolveEntrypoint returns the absolute path to manifest's
// AllowedEntrypoint within installPath — the ONLY binary a certified
// installer adapter should ever execute for this installation. Rejects
// any AllowedEntrypoint value that would resolve outside installPath
// (a "../" escape, an absolute path override, or similar) — a manifest
// is expected to have already been signature-verified (see
// cmd/pgarchisign) before reaching this function, but this check exists
// as defense in depth regardless, matching this project's own
// established "validate again at the point of actual use, don't rely
// solely on an earlier check" pattern (see e.g.
// internal/orchestrator.prepareJob's own DDL-injection validation,
// re-enforced again in internal/ddlflow/internal/shadowflow).
func ResolveEntrypoint(installPath string, manifest InventoryManifest) (string, error) {
	if manifest.AllowedEntrypoint == "" {
		return "", fmt.Errorf("manifest does not declare an AllowedEntrypoint")
	}
	cleanInstall := filepath.Clean(installPath)
	candidate := filepath.Join(cleanInstall, filepath.FromSlash(manifest.AllowedEntrypoint))
	candidate = filepath.Clean(candidate)

	if candidate != cleanInstall && !strings.HasPrefix(candidate, cleanInstall+string(filepath.Separator)) {
		return "", ErrEntrypointEscapesInstallation
	}

	if info, err := os.Stat(candidate); err != nil {
		return "", fmt.Errorf("allowed entrypoint does not exist: %w", err)
	} else if info.IsDir() {
		return "", fmt.Errorf("allowed entrypoint %s is a directory, not a file", manifest.AllowedEntrypoint)
	}
	return candidate, nil
}

func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
