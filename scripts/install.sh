#!/usr/bin/env sh
# install.sh — one-line installer for pgArchiMigrator's portable
# packages (see scripts/build-artifact.sh's own "portable" mode and
# deploy/platforms/*.json for exactly what this downloads).
#
# Usage:
#   curl -fsSL https://raw.githubusercontent.com/pgarchihub/pgarchimigrator/main/scripts/install.sh | sh
#
# Override the version (default: latest GitHub release):
#   curl -fsSL .../install.sh | PGARCHIMIGRATOR_VERSION=v2.0.0 sh
#
# What this does, in order:
#   1. Detects OS/architecture and picks the matching portable package
#      from deploy/platforms/*.json's own build matrix.
#   2. Downloads that package plus its checksums.sha256 from the
#      matching GitHub Release.
#   3. Verifies the download's SHA-256 against checksums.sha256 — this
#      catches a corrupted or truncated download. It does NOT verify
#      the Ed25519 artifact-descriptor signature (see cmd/pgarchisign)
#      — that's a SEPARATE, stronger guarantee (the artifact was
#      genuinely built and signed by this project, not just "whatever
#      bytes happened to be at this URL"), deliberately left as an
#      OPTIONAL manual step below rather than embedded here: verifying
#      an Ed25519 signature from a POSIX sh script would mean either
#      shelling out to a tool that isn't on every machine (a modern
#      OpenSSL build) or bundling a signature-verification binary
#      that itself has no signature to verify ITS OWN integrity against
#      — a bootstrapping problem with no clean answer inside a single
#      curl-then-run script. HTTPS transport integrity + a checksum
#      check is the realistic, honest floor this format can offer;
#      run `pgarchisign verify` yourself afterward (see the printed
#      instructions at the end) for the stronger guarantee.
#   4. Extracts the binary into this project's own standard install
#      directory (matching internal/deploylayout's own
#      DefaultEcosystemRoot() + ProductPathSegments exactly, so a
#      script-installed binary lives in the SAME place a manually
#      unpacked one would).
#   5. Prints how to add it to PATH (never modifies shell rc files
#      automatically — a person's shell config is theirs to own, and
#      silently editing it is a common source of installer complaints).
set -eu

REPO="pgarchihub/pgarchimigrator"
VERSION="${PGARCHIMIGRATOR_VERSION:-}"

log() { printf '%s\n' "$*" >&2; }
die() {
	log "error: $*"
	exit 1
}

# --- 1. Detect OS/architecture, matching deploy/platforms/*.json's own
#     os/arch naming exactly (linux|macos|windows, x64|arm64) — this
#     script only ever handles linux/macos; Windows has its own
#     install.ps1 (PowerShell, not POSIX sh).
os_raw="$(uname -s)"
case "$os_raw" in
Linux) os_name="linux" ;;
Darwin) os_name="macos" ;;
*) die "unsupported OS: $os_raw (this script covers Linux and macOS only — see install.ps1 for Windows)" ;;
esac

arch_raw="$(uname -m)"
case "$arch_raw" in
x86_64 | amd64) arch_name="x64" ;;
arm64 | aarch64) arch_name="arm64" ;;
*) die "unsupported architecture: $arch_raw" ;;
esac

# macOS x64 (Intel) has no published platform definition yet (see
# deploy/platforms/ — only macos-arm64.json exists) — fail clearly
# rather than attempting a download that 404s with no explanation.
if [ "$os_name" = "macos" ] && [ "$arch_name" = "x64" ]; then
	die "macOS on Intel (x64) isn't published yet — only Apple Silicon (arm64) is currently built. Building from source is the only option on Intel Macs for now."
fi

log "Detected platform: ${os_name}-${arch_name}"

# --- 2. Resolve the version and download URL.
if [ -z "$VERSION" ]; then
	log "Resolving the latest release..."
	# GitHub's own releases/latest endpoint — grep/sed rather than jq,
	# since jq isn't guaranteed to be on every machine this runs on,
	# while grep/sed are POSIX-baseline. Extracts "tag_name": "vX.Y.Z".
	VERSION="$(curl -fsSL "https://api.github.com/repos/${REPO}/releases/latest" |
		grep '"tag_name"' | head -n1 | sed -E 's/.*"tag_name": *"([^"]+)".*/\1/')"
	[ -n "$VERSION" ] || die "could not resolve the latest release version — set PGARCHIMIGRATOR_VERSION explicitly"
fi
log "Installing pgArchiMigrator ${VERSION}"

archive_ext="tar.gz"
pkg_name="pgarchimigrator-${VERSION}-${os_name}-${arch_name}"
archive_name="${pkg_name}.${archive_ext}"
release_base="https://github.com/${REPO}/releases/download/${VERSION}"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "$tmp_dir"' EXIT

log "Downloading ${archive_name}..."
curl -fsSL -o "$tmp_dir/$archive_name" "$release_base/$archive_name" ||
	die "failed to download $archive_name — check that $VERSION was actually released for ${os_name}-${arch_name}"

log "Downloading checksums.sha256..."
curl -fsSL -o "$tmp_dir/checksums.sha256" "$release_base/checksums.sha256" ||
	die "failed to download checksums.sha256"

# --- 3. Verify the download's integrity — see this file's own header
#     comment for exactly what this does and does not guarantee.
log "Verifying checksum..."
(
	cd "$tmp_dir"
	if command -v sha256sum >/dev/null 2>&1; then
		grep " ${archive_name}\$" checksums.sha256 | sha256sum -c - ||
			die "CHECKSUM MISMATCH — the downloaded archive does not match checksums.sha256. Do not run this build; try again or report this."
	else
		# macOS's own shasum, when GNU coreutils isn't installed.
		grep " ${archive_name}\$" checksums.sha256 | shasum -a 256 -c - ||
			die "CHECKSUM MISMATCH — the downloaded archive does not match checksums.sha256. Do not run this build; try again or report this."
	fi
)
log "Checksum OK."

# --- 4. Extract into this project's own standard install directory —
#     matching internal/deploylayout's own DefaultEcosystemRoot() +
#     ProductPathSegments (ArchiOrbitLabs/Platforms/PostgreSQL/
#     pgArchiMigrator) exactly, per-OS, so a script install lands in
#     the SAME place a manual unpack would.
case "$os_name" in
macos)
	ecosystem_root="${HOME}/Library/Application Support/ArchiOrbitLabs"
	;;
linux)
	ecosystem_root="${XDG_DATA_HOME:-${HOME}/.local/share}/ArchiOrbitLabs"
	;;
esac
install_dir="${ecosystem_root}/Platforms/PostgreSQL/pgArchiMigrator"
mkdir -p "$install_dir"

log "Extracting to ${install_dir}..."
tar -xzf "$tmp_dir/$archive_name" -C "$tmp_dir"
# build-artifact.sh's own portable-package layout places the binary at
# the archive's own top level, named per deploy/platforms/*.json's
# binaryName field ("pgarchimigrator" on linux/macos) — copy just the
# binary itself into install_dir rather than the whole extracted tree,
# so a re-run of this script doesn't accumulate old package metadata.
find "$tmp_dir" -maxdepth 2 -type f -name "pgarchimigrator" -exec cp {} "$install_dir/pgarchimigrator" \;
chmod +x "$install_dir/pgarchimigrator"

[ -x "$install_dir/pgarchimigrator" ] || die "extraction succeeded but the pgarchimigrator binary wasn't found where expected — the portable package's own layout may have changed"

log ""
log "pgArchiMigrator ${VERSION} installed to:"
log "  $install_dir/pgarchimigrator"
log ""
log "Add it to your PATH (this script never edits shell config files for you):"
log "  export PATH=\"$install_dir:\$PATH\""
log ""
log "For the stronger integrity guarantee beyond the checksum check above"
log "(that this artifact was genuinely built and signed by this project,"
log "not just whatever bytes were at this URL), verify its Ed25519"
log "artifact descriptor separately:"
log "  curl -fsSL -o pgarchimigrator.descriptor.json $release_base/${archive_name}.descriptor.json"
log "  pgarchisign verify --descriptor pgarchimigrator.descriptor.json --pubkey <the project's published public key>"
log ""
log "Get started:"
log "  $install_dir/pgarchimigrator --help"
