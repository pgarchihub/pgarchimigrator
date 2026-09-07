package entitlement

import "testing"

func TestNewConfigChecker_Empty_DefaultsToCommunity(t *testing.T) {
	c := NewConfigChecker("")
	if c.Edition() != EditionCommunity {
		t.Errorf("expected an empty value to default to community, got %s", c.Edition())
	}
}

// TestNewConfigChecker_Unrecognized_FailsToCommunity is the direct
// regression test for the safety property this whole package exists to
// guarantee: a typo'd or unrecognized edition value (e.g. "enterprize",
// a stray space, an old/renamed value) must NEVER silently unlock
// Enterprise functionality. Failing toward the LESS-featured tier is
// the only safe default here.
func TestNewConfigChecker_Unrecognized_FailsToCommunity(t *testing.T) {
	for _, bad := range []string{"enterprize", "Enterprise", " enterprise", "cloud", "premium"} {
		c := NewConfigChecker(bad)
		if c.Edition() != EditionCommunity {
			t.Errorf("expected unrecognized value %q to fail toward community, got %s", bad, c.Edition())
		}
	}
}

func TestNewConfigChecker_Enterprise_RecognizedExactly(t *testing.T) {
	c := NewConfigChecker("enterprise")
	if c.Edition() != EditionEnterprise {
		t.Errorf("expected the exact value 'enterprise' to be recognized, got %s", c.Edition())
	}
	if !c.IsEnterprise() {
		t.Error("expected IsEnterprise() to be true for the enterprise edition")
	}
}

func TestConfigChecker_Community_IsEnterpriseFalse(t *testing.T) {
	c := NewConfigChecker("community")
	if c.IsEnterprise() {
		t.Error("expected IsEnterprise() to be false for the community edition")
	}
}

// TestChecker_InterfaceSatisfaction is a compile-time-adjacent check —
// if ConfigChecker ever stops implementing Checker, this test file
// fails to compile, which is a more useful signal than a runtime test
// failure for an interface-satisfaction regression.
func TestChecker_InterfaceSatisfaction(t *testing.T) {
	var _ Checker = (*ConfigChecker)(nil)
}
