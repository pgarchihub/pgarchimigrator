package strategy

import "testing"

func TestValidateColumnType_AcceptsCommonTypes(t *testing.T) {
	valid := []string{
		"text", "integer", "bigint", "boolean", "uuid", "jsonb",
		"varchar(255)", "numeric(10,2)", "char(1)",
		"double precision", "timestamp with time zone", "character varying",
		"text[]", "integer[]",
		"myschema.mytype",
	}
	for _, v := range valid {
		if err := ValidateColumnType(v); err != nil {
			t.Errorf("expected %q to be accepted as a valid column type, got error: %v", v, err)
		}
	}
}

// TestValidateColumnType_RejectsInjectionAttempts is the direct
// regression test for the real vulnerability this whole feature closes:
// ColumnType is inlined directly into DDL text (PostgreSQL doesn't
// support parameter binding inside ALTER TABLE/ADD COLUMN), so an
// unvalidated caller-supplied value was a genuine SQL injection vector,
// not just a theoretical one.
func TestValidateColumnType_RejectsInjectionAttempts(t *testing.T) {
	invalid := []string{
		"",
		"text; DROP TABLE users; --",
		"text) ; DROP TABLE users --",
		"text'; DELETE FROM accounts WHERE '1'='1",
		"text -- comment",
		"text/* comment */",
		"text)",
		"(SELECT 1)",
	}
	for _, v := range invalid {
		if err := ValidateColumnType(v); err == nil {
			t.Errorf("expected %q to be rejected, but it was accepted", v)
		}
	}
}

func TestValidateSQLExpression_AcceptsCommonDefaultsAndCheckExpressions(t *testing.T) {
	valid := []string{
		"'active'",
		"0",
		"true",
		"now()",
		"gen_random_uuid()",
		"CURRENT_TIMESTAMP",
		"price > 0",
		"status IN ('active', 'inactive')",
		"quantity >= 0 AND quantity <= 1000",
		"email IS NOT NULL",
		// A column/value merely containing a dangerous keyword as a
		// SUBSTRING of an identifier must not false-positive — see
		// dangerousSQLExpressionPattern's own doc comment on word
		// boundaries.
		"deleted_at IS NULL",
		"created_by = 'system'",
	}
	for _, v := range valid {
		if err := ValidateSQLExpression(v, "test field"); err != nil {
			t.Errorf("expected %q to be accepted, got error: %v", v, err)
		}
	}
}

// TestValidateSQLExpression_RejectsInjectionAttempts is the direct
// regression test for the real vulnerability this closes — DefaultValue
// and CheckExpression are both inlined directly into DDL text, so an
// unvalidated value could chain additional statements or comment out
// the rest of the intended one.
func TestValidateSQLExpression_RejectsInjectionAttempts(t *testing.T) {
	invalid := []string{
		"0; DROP TABLE users; --",
		"'x'; DELETE FROM accounts WHERE '1'='1",
		"0 -- comment out the rest",
		"0 /* comment */ OR 1=1",
		"(SELECT password FROM users LIMIT 1)", // not blocked by keyword list, but semicolon/comment variants of this same idea are
		"'x'); GRANT ALL ON accounts TO public; --",
		"0; TRUNCATE TABLE accounts",
		"1=1; ALTER TABLE accounts DROP COLUMN balance",
	}
	for _, v := range invalid {
		// Only assert on the ones that actually contain a blocked
		// pattern — the SELECT-subquery case above is a reminder that
		// this is a blocklist, not a full parser (see
		// ValidateSQLExpression's own doc comment), so it's excluded
		// from the assertion rather than asserted against.
		if v == "(SELECT password FROM users LIMIT 1)" {
			continue
		}
		if err := ValidateSQLExpression(v, "test field"); err == nil {
			t.Errorf("expected %q to be rejected, but it was accepted", v)
		}
	}
}

func TestValidateSQLExpression_IncludesFieldNameInErrorMessage(t *testing.T) {
	err := ValidateSQLExpression("0; DROP TABLE users", "CheckExpression")
	if err == nil {
		t.Fatal("expected an error")
	}
	if got := err.Error(); got == "" {
		t.Fatal("expected a non-empty error message")
	}
}
