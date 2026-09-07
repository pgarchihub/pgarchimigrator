#!/usr/bin/env bash
# generate-checksums.sh — writes checksums.sha256 for every artifact in
# a directory (default: ./dist, what build-artifact.sh produces into)
# — AOL-STD-DEPLOY-001 §5.2's "The package MUST include ... checksums".
#
# Usage: ./scripts/generate-checksums.sh [directory]
set -euo pipefail

TARGET_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/dist}"
if [ ! -d "$TARGET_DIR" ]; then
  echo "directory not found: $TARGET_DIR" >&2
  exit 1
fi

OUT_FILE="$TARGET_DIR/checksums.sha256"

# Only hash actual artifacts (archives) — never checksums.sha256 itself,
# which would otherwise make this script non-idempotent (each run's
# checksum file would include a hash of the PREVIOUS run's checksum
# file). find -maxdepth 1: this directory's own artifacts only, not
# whatever a nested platform subdirectory might also contain.
: > "$OUT_FILE"
find "$TARGET_DIR" -maxdepth 1 -type f \( -name "*.zip" -o -name "*.tar.gz" \) -print0 |
  sort -z |
  while IFS= read -r -d '' f; do
    if command -v sha256sum >/dev/null 2>&1; then
      sha256sum "$f" | sed "s|$TARGET_DIR/||" >> "$OUT_FILE"
    else
      # macOS's own shasum, for a dev machine without GNU coreutils —
      # -a 256 matches sha256sum's own algorithm and output format
      # closely enough that a downstream `sha256sum -c` still verifies
      # correctly against either tool's output.
      shasum -a 256 "$f" | sed "s|$TARGET_DIR/||" >> "$OUT_FILE"
    fi
  done

echo "Checksums written: $OUT_FILE"
cat "$OUT_FILE"
