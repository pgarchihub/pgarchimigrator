package typecompat

import "testing"

func TestIsCompatible_VarcharWidening(t *testing.T) {
	cases := []struct {
		old, new string
		want     bool
	}{
		{"character varying(50)", "varchar(100)", true},
		{"character varying(50)", "VARCHAR(100)", true},  // case-insensitive
		{"varchar(50)", "varchar(50)", true},             // equal length is still >= , safe
		{"character varying(100)", "varchar(50)", false}, // SHRINKING — never safe
		{"character varying(50)", "text", true},          // dropping the limit entirely
		{"character varying", "varchar(50)", false},      // old unlimited -> new limited: could truncate
		{"character varying(50)", "text", true},
	}
	for _, c := range cases {
		got := IsCompatible(c.old, c.new)
		if got != c.want {
			t.Errorf("IsCompatible(%q, %q) = %v, want %v", c.old, c.new, got, c.want)
		}
	}
}

func TestIsCompatible_CharWidening(t *testing.T) {
	cases := []struct {
		old, new string
		want     bool
	}{
		{"character(10)", "character(20)", true},
		{"character(10)", "char(5)", false}, // shrinking
		{"character(10)", "char(10)", true}, // equal
		// Unspecified char precision implicitly means length 1 in
		// PostgreSQL, NOT "unlimited" — this must be rejected, not
		// silently treated as always-compatible the way varchar's
		// unspecified form is handled.
		{"character", "character(10)", false},
		{"character(10)", "character", false},
	}
	for _, c := range cases {
		got := IsCompatible(c.old, c.new)
		if got != c.want {
			t.Errorf("IsCompatible(%q, %q) = %v, want %v", c.old, c.new, got, c.want)
		}
	}
}

func TestIsCompatible_NumericWidening(t *testing.T) {
	cases := []struct {
		old, new string
		want     bool
	}{
		{"numeric(10,2)", "numeric(12,2)", true},  // widen precision, same scale
		{"numeric(10,2)", "numeric(8,2)", false},  // shrinking precision
		{"numeric(10,2)", "numeric(12,4)", false}, // scale change always needs validation
		{"numeric(10,2)", "decimal(12,2)", true},  // decimal is an alias for numeric
		{"numeric(10,2)", "numeric(10,2)", true},  // equal
		{"numeric", "numeric(10,2)", false},       // old unlimited -> new constrained: could reject existing values
	}
	for _, c := range cases {
		got := IsCompatible(c.old, c.new)
		if got != c.want {
			t.Errorf("IsCompatible(%q, %q) = %v, want %v", c.old, c.new, got, c.want)
		}
	}
}

func TestIsCompatible_CrossKind_AlwaysFalse(t *testing.T) {
	cases := []struct{ old, new string }{
		{"text", "integer"},
		{"integer", "bigint"}, // widely believed "safe", but is NOT free in vanilla PostgreSQL — must stay false
		{"integer", "text"},
		{"varchar(50)", "numeric(10,2)"},
		{"numeric(10,2)", "varchar(50)"},
		{"text", "varchar(50)"}, // text -> varchar always needs a length-fit validation scan
	}
	for _, c := range cases {
		if IsCompatible(c.old, c.new) {
			t.Errorf("IsCompatible(%q, %q) = true, want false (cross-kind changes always need real validation)", c.old, c.new)
		}
	}
}

func TestIsCompatible_UnrecognizedTypes_DefaultsFalse(t *testing.T) {
	// Anything this package doesn't explicitly recognize must default to
	// false — the existing, already-safe behavior (route through Shadow
	// Table) — never silently assumed compatible.
	cases := []struct{ old, new string }{
		{"jsonb", "jsonb"},
		{"timestamp", "timestamptz"},
		{"uuid", "uuid"},
		{"", ""},
		{"not a real type", "also not real"},
	}
	for _, c := range cases {
		if IsCompatible(c.old, c.new) {
			t.Errorf("IsCompatible(%q, %q) = true, want false (unrecognized types must default to incompatible)", c.old, c.new)
		}
	}
}
