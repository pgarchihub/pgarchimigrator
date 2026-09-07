// Package ecosystemmanifest embeds this product's Archi Product
// Manifest (see AC-PF-003 Section 6) directly into the compiled
// binary — the same reasoning internal/api's own webappFS follows for
// the React SPA (see that variable's doc comment): a binary that
// depends on a file existing at some expected filesystem path,
// separate from the binary itself, is fragile the moment someone moves
// or copies just the binary. This package exists specifically because
// go:embed can only reach files inside its OWN package directory (or a
// subdirectory of it) — never a parent directory — which is why the
// manifest lives here rather than at the repo root; see
// docs/ecosystem/ARCHITECTURE.md for the full reasoning and this
// package's own single source of truth for the manifest's actual
// content.
package ecosystemmanifest

import _ "embed"

//go:embed archi-product-manifest.yaml
var raw string

// Raw returns the exact bytes of this product's Archi Product Manifest
// YAML file, unmodified — internal/api's manifest handler serves this
// directly, byte for byte, so what a consumer fetches over HTTP is
// always identical to what's reviewable in this repository.
func Raw() string {
	return raw
}
