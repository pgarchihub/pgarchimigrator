// Package typecompat implements automatic detection of "compatible"
// PostgreSQL column type changes — i.e. ALTER COLUMN TYPE requests that
// PostgreSQL itself can apply as a metadata-only operation, without a
// full table rewrite. When true, internal/strategy.Decide routes an
// ALTER_COLUMN_TYPE request through DIRECT_DDL even on a large table,
// skipping the Shadow Table strategy's overhead entirely.
//
// SCOPE — deliberately conservative: this recognizes only a curated set
// of well-documented, version-stable "free" cases (widening a
// varchar/character length limit or dropping it in favor of text,
// widening a char length, widening numeric precision at a fixed scale).
// It does NOT attempt to replicate PostgreSQL's full internal
// binary-coercibility logic for arbitrary type pairs — that logic is
// subtle and has shifted across major versions, and a wrong "yes" here
// could cause a genuinely damaging, unexpected ACCESS EXCLUSIVE table
// lock in production — exactly the failure mode this whole tool exists
// to prevent. Anything not explicitly recognized returns false, which is
// the existing, already-safe default (route through Shadow Table) — so
// this package can only ever make a migration MORE efficient, never less
// safe.
package typecompat

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	// Matches character varying/varchar/character/char/bpchar, with an
	// optional (n) length. Postgres normalizes "varchar" to
	// "character varying" and "char"/"bpchar" to "character" in
	// format_type() output, but user-supplied --type values commonly use
	// the short forms — both are accepted here.
	lengthTypeRe = regexp.MustCompile(`^(character varying|varchar|character|char|bpchar)\s*(?:\(\s*(\d+)\s*\))?$`)
	// Matches numeric/decimal with optional (precision) or (precision, scale).
	numericTypeRe = regexp.MustCompile(`^(numeric|decimal)\s*(?:\(\s*(\d+)\s*(?:,\s*(\d+)\s*)?\))?$`)
)

type parsedType struct {
	kind      string // "varchar", "char", "text", "numeric", or "" (unrecognized)
	precision int    // length or precision; -1 if unspecified
	scale     int    // numeric scale only; -1 if unspecified/not applicable
}

func parseType(raw string) parsedType {
	s := strings.ToLower(strings.TrimSpace(raw))

	if s == "text" {
		return parsedType{kind: "text", precision: -1, scale: -1}
	}

	if m := lengthTypeRe.FindStringSubmatch(s); m != nil {
		kind := "varchar"
		if m[1] == "character" || m[1] == "char" || m[1] == "bpchar" {
			kind = "char"
		}
		precision := -1
		if m[2] != "" {
			precision, _ = strconv.Atoi(m[2])
		}
		return parsedType{kind: kind, precision: precision, scale: -1}
	}

	if m := numericTypeRe.FindStringSubmatch(s); m != nil {
		precision, scale := -1, -1
		if m[2] != "" {
			precision, _ = strconv.Atoi(m[2])
		}
		if m[3] != "" {
			scale, _ = strconv.Atoi(m[3])
		}
		return parsedType{kind: "numeric", precision: precision, scale: scale}
	}

	return parsedType{kind: ""}
}

// IsCompatible reports whether changing a column from oldType to newType
// is one of the curated "free" cases this package recognizes — see the
// package doc comment for the exact scope and the safety reasoning behind
// keeping it narrow. oldType is typically the exact format_type() output
// captured via CurrentColumnType; newType is the user-supplied --type
// value, in whatever casing/spacing they wrote it.
func IsCompatible(oldType, newType string) bool {
	old := parseType(oldType)
	new_ := parseType(newType)
	if old.kind == "" || new_.kind == "" {
		return false
	}

	switch old.kind {
	case "varchar":
		switch new_.kind {
		case "varchar":
			return widensOrDropsLimit(old.precision, new_.precision)
		case "text":
			return true // dropping the limit entirely is always a widening
		}
		return false

	case "char":
		if new_.kind != "char" {
			return false
		}
		// Deliberately stricter than varchar: an unspecified "char"/
		// "character" implicitly means length 1 in PostgreSQL, NOT
		// "unlimited" — so -1 must never be treated as "wider than
		// everything" here the way it safely can be for varchar/numeric.
		// Only the explicit CHAR(n) -> CHAR(m) case is recognized.
		if old.precision == -1 || new_.precision == -1 {
			return false
		}
		return new_.precision >= old.precision

	case "numeric":
		if new_.kind != "numeric" {
			return false
		}
		if old.scale != new_.scale {
			return false // a scale change always needs real data validation
		}
		return widensOrDropsLimit(old.precision, new_.precision)

	default: // "text" -> anything, or any unrecognized kind
		return false
	}
}

// widensOrDropsLimit is shared by the varchar and numeric cases, where an
// unspecified precision (-1) genuinely means "unlimited" (unlike char's
// implicit length-1 default, handled separately above).
func widensOrDropsLimit(oldPrecision, newPrecision int) bool {
	if newPrecision == -1 {
		return true // dropping the limit entirely is always safe
	}
	if oldPrecision == -1 {
		return false // old was unlimited; adding an explicit limit could truncate existing data
	}
	return newPrecision >= oldPrecision
}

// CurrentColumnType reads the exact, fully-specified current type of an
// existing column (including modifiers like varchar(50) or numeric(10,2))
// via format_type() — the same technique internal/ddlflow's RENAME_COLUMN
// support uses, duplicated here rather than shared across packages to
// keep typecompat usable standalone without pulling in ddlflow.
func CurrentColumnType(ctx context.Context, pool *pgxpool.Pool, schema, table, column string) (string, error) {
	var typ string
	query := `
		SELECT format_type(a.atttypid, a.atttypmod)
		FROM pg_attribute a
		JOIN pg_class c ON c.oid = a.attrelid
		JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND a.attname = $3 AND NOT a.attisdropped
	`
	err := pool.QueryRow(ctx, query, schema, table, column).Scan(&typ)
	if err != nil {
		return "", fmt.Errorf("column %q not found on %s.%s: %w", column, schema, table, err)
	}
	return typ, nil
}
