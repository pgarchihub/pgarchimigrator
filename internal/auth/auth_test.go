package auth

import (
	"strings"
	"testing"
)

func TestHashPassword_VerifyPassword_RoundTrip(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if hash == "correct-horse-battery-staple" {
		t.Fatal("the hash must not equal the plaintext password")
	}
	if !VerifyPassword(hash, "correct-horse-battery-staple") {
		t.Error("expected the correct password to verify successfully")
	}
}

func TestVerifyPassword_WrongPassword_Fails(t *testing.T) {
	hash, err := HashPassword("correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("HashPassword failed: %v", err)
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Error("expected an incorrect password to fail verification")
	}
}

func TestVerifyPassword_MalformedHash_FailsWithoutPanicking(t *testing.T) {
	if VerifyPassword("not-a-real-bcrypt-hash", "anything") {
		t.Error("expected a malformed hash to fail verification, not panic or succeed")
	}
}

func TestRole_Satisfies(t *testing.T) {
	cases := []struct {
		have, want Role
		expect     bool
	}{
		{RoleAdmin, RoleViewer, true},
		{RoleAdmin, RoleOperator, true},
		{RoleAdmin, RoleAdmin, true},
		{RoleOperator, RoleViewer, true},
		{RoleOperator, RoleOperator, true},
		{RoleOperator, RoleAdmin, false},
		{RoleViewer, RoleOperator, false},
		{RoleViewer, RoleAdmin, false},
		{RoleViewer, RoleViewer, true},
	}
	for _, c := range cases {
		if got := c.have.Satisfies(c.want); got != c.expect {
			t.Errorf("%s.Satisfies(%s) = %v, want %v", c.have, c.want, got, c.expect)
		}
	}
}

func TestRole_Satisfies_UnrecognizedRoleFailsClosed(t *testing.T) {
	var bogus Role = "NOT_A_REAL_ROLE"
	if bogus.Satisfies(RoleViewer) {
		t.Error("an unrecognized role must not satisfy even the lowest real role (fail closed)")
	}
}

func TestGenerateSessionToken_ProducesUniqueTokens(t *testing.T) {
	token1, hash1, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken failed: %v", err)
	}
	token2, hash2, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken failed: %v", err)
	}
	if token1 == token2 {
		t.Error("expected two calls to produce different raw tokens")
	}
	if hash1 == hash2 {
		t.Error("expected two calls to produce different hashes")
	}
	if strings.Contains(hash1, token1) {
		t.Error("the hash must not contain the raw token as a substring")
	}
}

func TestHashToken_Deterministic(t *testing.T) {
	rawToken, expectedHash, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken failed: %v", err)
	}
	if got := HashToken(rawToken); got != expectedHash {
		t.Errorf("HashToken(rawToken) = %q, want %q (must match what GenerateSessionToken computed)", got, expectedHash)
	}
}
