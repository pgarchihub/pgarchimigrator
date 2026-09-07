// Package deploylayout resolves and validates the canonical ArchiOrbit
// Labs ecosystem filesystem layout — AOL-STD-DEPLOY-001 Section 4. This
// is the ONE place in the codebase that knows the ecosystem root's
// default per-OS location and where pgArchiMigrator's own files must
// live within it; every other package that needs a path under the
// ecosystem root goes through this package rather than constructing one
// itself, so the layout rules (Section 4.2's MUST list) are enforced in
// exactly one place.
//
// This package does NOT decide when/whether to use the ecosystem
// layout at all — a self-hosted Community/Enterprise operator running
// pgArchiMigrator entirely on its own (outside any ArchiConsole-managed
// installation) has no reason to touch this package; its existing
// --state-db/--auth-db/--upgrade-db flags and PGARCHIMIGRATOR_*
// environment variables are untouched by anything here. This package
// exists for the desktop-managed local topology (AOL-STD-DEPLOY-001
// Section 3.2) specifically, where ArchiConsole itself resolves and
// owns the ecosystem root and hands pgArchiMigrator's installer adapter
// a path within it.
package deploylayout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// EcosystemRootDirName is the required final path component of any
// valid ecosystem root — AOL-STD-DEPLOY-001 Section 4.2: "The final
// root directory name MUST be ArchiOrbitLabs."
const EcosystemRootDirName = "ArchiOrbitLabs"

// ProductPathSegments is pgArchiMigrator's fixed location within the
// ecosystem root — Section 4: "<ArchiOrbitLabs Root>/Platforms/
// PostgreSQL/pgArchiMigrator", not configurable (Section 4.2: "Product
// paths MUST be resolved by the shared layout contract; user-supplied
// subpaths are forbidden").
var ProductPathSegments = []string{"Platforms", "PostgreSQL", "pgArchiMigrator"}

// DefaultEcosystemRoot returns this OS's default ecosystem root —
// AOL-STD-DEPLOY-001 Section 4.1's exact table:
//
//	Windows: %LOCALAPPDATA%\ArchiOrbitLabs
//	macOS:   ~/Library/Application Support/ArchiOrbitLabs
//	Linux:   ${XDG_DATA_HOME:-~/.local/share}/ArchiOrbitLabs
//
// This is only ever a DEFAULT — AOL-STD-DEPLOY-001 Section 8 describes
// ArchiConsole itself resolving "the selected ecosystem root" during
// installation planning, meaning the user (via ArchiConsole's own UI)
// may have chosen a different root; a caller that already knows the
// actual selected root should use it directly rather than calling this
// function, which is provided for the case a default genuinely applies
// (first-run detection, documentation, tests).
func DefaultEcosystemRoot() (string, error) {
	switch runtime.GOOS {
	case "windows":
		base := os.Getenv("LOCALAPPDATA")
		if base == "" {
			return "", errors.New("LOCALAPPDATA is not set")
		}
		return filepath.Join(base, EcosystemRootDirName), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("could not determine home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", EcosystemRootDirName), nil
	default: // linux and other unix-likes
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, EcosystemRootDirName), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("could not determine home directory: %w", err)
		}
		return filepath.Join(home, ".local", "share", EcosystemRootDirName), nil
	}
}

// ProductPath returns pgArchiMigrator's fixed installation target within
// ecosystemRoot — <ecosystemRoot>/Platforms/PostgreSQL/pgArchiMigrator —
// after validating ecosystemRoot itself via ValidateEcosystemRoot.
func ProductPath(ecosystemRoot string) (string, error) {
	if err := ValidateEcosystemRoot(ecosystemRoot); err != nil {
		return "", err
	}
	segments := append([]string{ecosystemRoot}, ProductPathSegments...)
	return filepath.Join(segments...), nil
}

// ValidateEcosystemRoot enforces AOL-STD-DEPLOY-001 Section 4.2's MUST
// rules on a candidate ecosystem root:
//
//   - MUST be absolute
//   - MUST NOT be a filesystem root (e.g. "/", "C:\")
//   - The final path component MUST be ArchiOrbitLabs, compared
//     case-insensitively (Section 4.2 requires this "where the platform
//     requires it" — Windows and macOS filesystems are commonly
//     case-insensitive; this package compares case-insensitively
//     unconditionally, since accepting a case-correct match everywhere
//     and rejecting nothing extra is the safe direction for a
//     validation rule to be imprecise in).
func ValidateEcosystemRoot(root string) error {
	if !filepath.IsAbs(root) {
		return fmt.Errorf("ecosystem root %q must be an absolute path", root)
	}
	clean := filepath.Clean(root)
	if isFilesystemRoot(clean) {
		return fmt.Errorf("ecosystem root %q must not be a filesystem root", root)
	}
	base := filepath.Base(clean)
	if !strings.EqualFold(base, EcosystemRootDirName) {
		return fmt.Errorf("ecosystem root %q must end in %q (got %q)", root, EcosystemRootDirName, base)
	}
	return nil
}

// isFilesystemRoot reports whether p is a filesystem root — "/" on
// Unix-likes, or a bare drive root such as "C:\" on Windows (checked via
// filepath.VolumeName rather than a hardcoded drive-letter pattern, so
// this is correct for any drive letter or UNC volume root).
func isFilesystemRoot(p string) bool {
	if p == string(filepath.Separator) {
		return true
	}
	vol := filepath.VolumeName(p)
	return vol != "" && (p == vol || p == vol+string(filepath.Separator))
}

// EnsureProductDirectories creates productPath and its expected
// subdirectories — AOL-STD-DEPLOY-001 Section 4.2: "Runtime state,
// installation files, and user migration data SHOULD be separated into
// explicit subdirectories." Directories are created with 0700
// (owner-only — Section 4.2's "state directories" requirement); this
// function does NOT touch or validate any existing installation beyond
// creating missing directories — Section 4.2's "Existing installations
// MUST NOT be silently overwritten or automatically moved" is enforced
// by NEVER writing product files here, only ensuring the directory
// skeleton exists.
func EnsureProductDirectories(productPath string) error {
	for _, sub := range []string{"", "state", "logs", "migrations"} {
		dir := filepath.Join(productPath, sub)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("failed to create %s: %w", dir, err)
		}
	}
	return nil
}
