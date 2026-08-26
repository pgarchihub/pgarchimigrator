package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// GenerateSessionToken creates a new random session token (returned to the
// client, e.g. in an httpOnly cookie) plus its SHA-256 hash (what's
// actually persisted via Store — see Session.TokenHash's doc comment for
// why the raw token is never stored).
func GenerateSessionToken() (rawToken, tokenHash string, err error) {
	buf := make([]byte, 32) // 256 bits of entropy
	if _, err := rand.Read(buf); err != nil {
		return "", "", fmt.Errorf("failed to generate session token: %w", err)
	}
	rawToken = hex.EncodeToString(buf)
	return rawToken, HashToken(rawToken), nil
}

// HashToken hashes a raw session token the same way GenerateSessionToken
// does, so a token presented by a client (e.g. read from a cookie) can be
// looked up by its hash via Store.GetSessionByTokenHash.
func HashToken(rawToken string) string {
	sum := sha256.Sum256([]byte(rawToken))
	return hex.EncodeToString(sum[:])
}
