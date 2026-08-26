package strategy

import (
	"fmt"
	"regexp"
	"strings"
)

// validColumnTypePattern matches genuine PostgreSQL type syntax: a base
// name (letters/digits/underscores, optionally schema-qualified with a
// dot), optional additional space-separated words for PostgreSQL's
// multi-word type names ("double precision", "character varying",
// "timestamp with time zone"), an optional precision/scale in
// parentheses ("numeric(10,2)", "varchar(255)"), and an optional array
// suffix ("text[]", "integer[]"). Deliberately does NOT allow anything
// else — no semicolons, quotes, or arbitrary expressions — since a type
// name has a genuinely bounded grammar, unlike DefaultValue/
// CheckExpression below, which are inherently arbitrary SQL expressions
// and can only reasonably be checked for known-dangerous patterns, not
// validated against a positive grammar.
var validColumnTypePattern = regexp.MustCompile(
	`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z_][a-zA-Z0-9_]*)?( [a-zA-Z_][a-zA-Z0-9_]*)*(\(\s*\d+\s*(,\s*\d+\s*)?\))?(\[\])?$`,
)

// ValidateColumnType reports whether typeName looks like a genuine
// PostgreSQL type name, not a SQL injection attempt. See
// pgArchiMigrator's ddlflow/shadowflow packages, which — like
// DefaultValue and CheckExpression below — must inline this value
// directly into DDL text (PostgreSQL doesn't support parameter binding
// inside ALTER TABLE/ADD COLUMN statements), so an unvalidated caller-
// supplied type name is a real SQL injection vector, not just a
// theoretical one. This is enforced at TWO layers deliberately: early,
// in internal/orchestrator, so a malformed request is rejected before a
// job record even exists — and again at the point each flow actually
// builds DDL from it, so any direct caller of ddlflow/shadowflow (not
// just ones that went through the orchestrator) gets the same
// protection, matching this project's existing internal/strategy
// isValidStrategyFor precedent for the same "enforce it where the
// dangerous action actually happens, not just at one entry point" reasoning.
func ValidateColumnType(typeName string) error {
	trimmed := strings.TrimSpace(typeName)
	if trimmed == "" {
		return fmt.Errorf("column type cannot be empty")
	}
	if !validColumnTypePattern.MatchString(trimmed) {
		return fmt.Errorf("column type %q does not look like a valid PostgreSQL type name", typeName)
	}
	return nil
}

// dangerousSQLExpressionPattern flags the classic SQL injection vectors
// inside a value that's otherwise expected to be an arbitrary
// expression (a DEFAULT value or a CHECK constraint body) — a semicolon
// (statement termination/chaining), SQL comment sequences (which can be
// used to comment out the rest of the intended statement and splice in
// something else within the SAME apparent statement), or a standalone
// DML/DDL keyword that has no legitimate business appearing inside a
// value or boolean expression. Word-boundary matched so this doesn't
// false-positive on an identifier merely CONTAINING one of these words
// as a substring (e.g. "deleted_at" doesn't match \bDELETE\b, since
// underscore is itself a word character with no boundary before "_at").
//
// This is NOT a full SQL parser, and deliberately doesn't try to be — a
// DEFAULT value or CHECK expression is inherently an arbitrary SQL
// expression by design (a literal, a function call like now(), a
// comparison, a boolean combination), so a strict positive allow-list
// would either be far too restrictive for legitimate use or would need
// to reimplement a meaningful chunk of PostgreSQL's own expression
// grammar. This blocklist targets the specific patterns that make
// statement-level injection possible, which is the actual severe risk
// (arbitrary additional SQL running), not an attempt to guarantee the
// expression is semantically "safe" in some broader sense.
var dangerousSQLExpressionPattern = regexp.MustCompile(
	`(?i)(;|--|/\*|\*/|\bDROP\b|\bGRANT\b|\bREVOKE\b|\bTRUNCATE\b|\bALTER\b|\bCREATE\b|\bINSERT\b|\bUPDATE\b|\bDELETE\b|\bEXECUTE\b|\bCOPY\b|\bVACUUM\b)`,
)

// ValidateSQLExpression checks expr (a DEFAULT value or CHECK constraint
// body — fieldName is used only to make the returned error message
// specific) for the dangerous patterns described in
// dangerousSQLExpressionPattern's own doc comment. See ValidateColumnType's
// doc comment for why this is enforced at two layers (early rejection in
// internal/orchestrator, and again at the point internal/ddlflow
// actually builds DDL from it).
func ValidateSQLExpression(expr, fieldName string) error {
	if dangerousSQLExpressionPattern.MatchString(expr) {
		return fmt.Errorf("%s contains a pattern that isn't allowed in this context (statement terminators, comments, and DML/DDL keywords aren't permitted here)", fieldName)
	}
	return nil
}
