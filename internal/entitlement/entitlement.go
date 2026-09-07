// Package entitlement decides which edition (Community, Enterprise, or
// Cloud) this running instance is, and therefore which features are
// unlocked. See docs/ecosystem/ARCHITECTURE.md for the full reasoning,
// but the short version: pgArchiMigrator is ONE codebase, not a forked
// Community/Enterprise pair — matching the Archi ecosystem's own
// AC-DEC-007 ("Community, Enterprise, and Cloud share the same core
// domain model") and the explicit warning against a "core domain fork" in
// ArchiConsole's own Product Definition (Section 10). Every feature
// exists in this one codebase; entitlement decides what's unlocked at
// runtime, not what's compiled in.
//
// Cloud is deliberately NOT a third code path — it's Enterprise's
// entitlement, running under a different deployment topology (managed,
// multi-tenant) that this package doesn't need to know about. See
// Edition's own doc comment.
package entitlement

// Edition is one of the three tiers this codebase can run as. There is
// no separate "cloud" build — Cloud is Enterprise-tier entitlement
// under a managed deployment; from this package's perspective (and
// every caller's) it checks IsEnterprise() and gets the same answer
// either way. A CloudChecker implementation, when one exists, is free
// to layer tenant/multi-tenancy concerns on top — that's a deployment
// concern, not a different entitlement tier as far as feature-gating
// goes.
type Edition string

const (
	EditionCommunity  Edition = "community"
	EditionEnterprise Edition = "enterprise"
)

// Checker is the one interface every feature-gating call site in this
// codebase depends on — never a raw config flag or env var read
// directly. This indirection is deliberate and cheap to add now,
// expensive to retrofit later: the ONLY implementation today
// (ConfigChecker, see below) reads a plain environment variable, but
// when a real license server/key exists, a new implementation goes
// behind this exact same interface and nothing else in the codebase
// changes — every call site already goes through Checker, not through
// "how is the edition currently determined."
type Checker interface {
	// Edition returns which tier this instance is running as.
	Edition() Edition
	// IsEnterprise is a convenience for the extremely common
	// "is this Enterprise-or-above" check — equivalent to
	// Edition() != EditionCommunity, but reads better at call sites
	// than a string comparison, and stays correct automatically if a
	// third tier is ever added above Enterprise.
	IsEnterprise() bool
}

// ConfigChecker is today's only Checker implementation — reads the
// PGARCHIMIGRATOR_EDITION environment variable once at startup. No
// license validation, no signature, no expiry: exactly what was asked
// for ("basit bir config flag'i"), on the explicit understanding that a
// real license-server-backed Checker will implement this same
// interface later, at which point ConfigChecker either goes away or
// stays as the self-hosted/offline fallback path — that decision is
// deferred, since it costs nothing to defer given the interface
// boundary already exists.
type ConfigChecker struct {
	edition Edition
}

// NewConfigChecker builds a ConfigChecker from a raw environment
// variable value (pass the result of os.Getenv("PGARCHIMIGRATOR_EDITION")
// directly — this function takes a plain string rather than reading the
// environment itself so it stays trivially testable without actually
// mutating process environment variables). An empty or unrecognized
// value defaults to EditionCommunity — this MUST fail toward the
// LESS-featured tier, never toward Enterprise, so a missing/misconfigured
// environment variable can never accidentally unlock gated
// functionality.
func NewConfigChecker(rawEditionEnvValue string) *ConfigChecker {
	switch Edition(rawEditionEnvValue) {
	case EditionEnterprise:
		return &ConfigChecker{edition: EditionEnterprise}
	default:
		return &ConfigChecker{edition: EditionCommunity}
	}
}

func (c *ConfigChecker) Edition() Edition {
	return c.edition
}

func (c *ConfigChecker) IsEnterprise() bool {
	return c.edition != EditionCommunity
}
