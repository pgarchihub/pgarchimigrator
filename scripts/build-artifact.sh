#!/usr/bin/env bash
# build-artifact.sh — produces pgArchiMigrator's two AOL-STD-DEPLOY-001-
# defined artifact types:
#
#   ./scripts/build-artifact.sh source
#     A source code package — a plain, deterministic tarball of the
#     git-tracked tree (no build outputs, no .git), matching
#     AOL-STD-DEPLOY-001 §5.2's "package contents MUST be deterministic
#     for the same source revision" requirement via `git archive`, which
#     produces byte-identical output for the same commit regardless of
#     when/where it's run.
#
#   ./scripts/build-artifact.sh portable <platform-json>
#     A portable desktop package for ONE platform (see deploy/platforms/
#     *.json for the supported matrix) — AOL-STD-DEPLOY-001 §3.2's
#     "desktop-managed local" topology, the only one pgArchiMigrator 2.0
#     targets initially. Builds the frontend, cross-compiles the Go
#     binary for the target platform, and assembles §5.2's recommended
#     package layout (minus SBOM.cdx.json and a code-signed descriptor —
#     see the header comment in deploy/release-channels.json for why
#     those specific §5.2/§12 items are deferred to the Enterprise
#     release by product decision).
#
# Neither mode signs or checksums its own output — see
# generate-checksums.sh and sign-descriptor.sh, run afterward against
# whatever this script produces. Keeping "build" and "attest what was
# built" as separate scripts (matching AOL-STD-DEPLOY-001 §13's own
# reference pipeline stages) means a checksum/signature is always
# computed against a finished, unambiguous artifact, never against a
# tree this script might still be mutating.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
OUT_DIR="${OUT_DIR:-$REPO_ROOT/dist}"
PRODUCT_VERSION="${PRODUCT_VERSION:-2.0.0}"
mkdir -p "$OUT_DIR"

usage() {
  echo "Usage:"
  echo "  $0 source"
  echo "  $0 portable <path-to-deploy/platforms/*.json>"
  exit 1
}

build_source_package() {
  local out="$OUT_DIR/pgarchimigrator-${PRODUCT_VERSION}-source.tar.gz"
  # git archive: deterministic, and naturally excludes anything not
  # tracked (build outputs, .git itself, local-only files) without this
  # script needing its own exclude list to keep in sync with .gitignore.
  git -C "$REPO_ROOT" archive --format=tar.gz \
    --prefix="pgarchimigrator-${PRODUCT_VERSION}-source/" \
    -o "$out" HEAD
  echo "Source package: $out"
}

build_portable_package() {
  local platform_json="$1"
  if [ ! -f "$platform_json" ]; then
    echo "platform definition not found: $platform_json" >&2
    exit 1
  fi

  local os arch goos goarch archive_fmt binary_name
  os=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['os'])" "$platform_json")
  arch=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['arch'])" "$platform_json")
  goos=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['goos'])" "$platform_json")
  goarch=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['goarch'])" "$platform_json")
  archive_fmt=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['archive'])" "$platform_json")
  binary_name=$(python3 -c "import json,sys; print(json.load(open(sys.argv[1]))['binaryName'])" "$platform_json")

  local pkg_name="pgarchimigrator-${PRODUCT_VERSION}-${os}-${arch}"
  local work_dir
  work_dir=$(mktemp -d)
  local pkg_dir="$work_dir/$pkg_name"
  mkdir -p "$pkg_dir/bin" "$pkg_dir/config/schema" "$pkg_dir/contracts" "$pkg_dir/migrations" "$pkg_dir/licenses"

  echo "==> Building frontend"
  # npm ci (not npm install) — matches ci.yml's own frontend job
  # exactly, installing precisely what package-lock.json pins rather
  # than potentially resolving something new. This step was missing
  # entirely before — the build script went straight to `npm run
  # build` against whatever node_modules happened to already exist on
  # the runner (none, on a fresh one), which is why this failed with
  # "Cannot find module 'react'" and similar errors rather than an
  # actual build problem.
  (cd "$REPO_ROOT/web" && npm ci)
  # Not redirected to /dev/null (deliberately) — a silent frontend build
  # failure here previously produced nothing more diagnosable than
  # "exit code 1" in CI, with no visible npm error to work from.
  (cd "$REPO_ROOT/web" && npm run build)
  rm -rf "$REPO_ROOT/internal/api/webapp"/*
  cp -r "$REPO_ROOT/web/dist"/* "$REPO_ROOT/internal/api/webapp/"

  echo "==> Cross-compiling for $goos/$goarch"
  (cd "$REPO_ROOT" && GOOS="$goos" GOARCH="$goarch" go build \
    -ldflags="-s -w -X github.com/pgarchihub/pgarchimigrator/internal/version.Version=v${PRODUCT_VERSION}" \
    -o "$pkg_dir/bin/$binary_name" \
    ./cmd/pgarchimigrator)

  echo "==> Assembling package contents"
  cp "$REPO_ROOT/deploy/manifests/pgarchimigrator.json" "$pkg_dir/manifest.json"
  cp "$REPO_ROOT/internal/api/ecosystemmanifest/archi-product-manifest.yaml" "$pkg_dir/contracts/capabilities.yaml"
  cp "$REPO_ROOT/LICENSE" "$pkg_dir/licenses/" 2>/dev/null || true
  echo "pgArchiMigrator — see licenses/ for third-party attributions once generate-sbom.* exists (deferred, see deploy/release-channels.json)" > "$pkg_dir/NOTICE"

  # config/defaults.json — the subset of config.Default() genuinely safe
  # to ship as a starting point (no secrets: DatabaseURL is deliberately
  # excluded, matching internal/config.Config's own DatabaseURL doc
  # comment — it's read from an environment variable, never a file).
  python3 -c "
import json
print(json.dumps({
    'smallTableRowThreshold': 1000000,
    'rollbackWindowSeconds': 600,
    'minPostgresVersion': 12,
    'stateDbPath': './pgarchimigrator-state.db',
    'authDbPath': './pgarchimigrator-auth.db',
    'upgradeDbPath': './pgarchimigrator-upgrade.db'
}, indent=2))
" > "$pkg_dir/config/defaults.json"

  local archive_path
  case "$archive_fmt" in
    zip)
      archive_path="$OUT_DIR/${pkg_name}.zip"
      (cd "$work_dir" && zip -qr "$archive_path" "$pkg_name")
      ;;
    tar.gz)
      archive_path="$OUT_DIR/${pkg_name}.tar.gz"
      tar -czf "$archive_path" -C "$work_dir" "$pkg_name"
      ;;
    *)
      echo "unsupported archive format: $archive_fmt" >&2
      exit 1
      ;;
  esac

  rm -rf "$work_dir"
  echo "Portable package: $archive_path"
}

case "${1:-}" in
  source)
    build_source_package
    ;;
  portable)
    [ $# -eq 2 ] || usage
    build_portable_package "$2"
    ;;
  *)
    usage
    ;;
esac
