package auth

import (
	"fmt"

	"golang.org/x/crypto/bcrypt"
)

// bcryptCost is intentionally the library default rather than a custom
// value — bcrypt.DefaultCost balances brute-force resistance against
// login latency. Go's bcrypt implementation also silently truncates
// passwords over 72 bytes (a documented limitation of the bcrypt
// algorithm itself, not a bug in this package).
const bcryptCost = bcrypt.DefaultCost

// HashPassword hashes a plaintext password for storage. The result is
// the ONLY thing that should ever be stored as User.PasswordHash.
func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return "", fmt.Errorf("failed to hash password: %w", err)
	}
	return string(hash), nil
}

// VerifyPassword reports whether the plaintext password matches the given
// bcrypt hash. Returns false (not an error) for any mismatch, including a
// malformed hash — callers should treat "wrong password" and "corrupted
// stored hash" identically rather than distinguishing them, to avoid
// leaking which case occurred.
func VerifyPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
