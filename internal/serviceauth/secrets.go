package serviceauth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// GenerateClientSecret creates a new random client secret (shown to
// whoever registers the client exactly once — see Client's own doc
// comment) plus its SHA-256 hash (what's actually persisted).
//
// Deliberately the same mechanism as auth.GenerateSessionToken (256
// bits of crypto/rand entropy, hex-encoded, SHA-256 hashed for storage)
// rather than a slower, intentionally-expensive hash like bcrypt: bcrypt
// exists to slow down brute-forcing a LOW-entropy, human-chosen secret
// (a password); a client secret is high-entropy and machine-generated,
// never guessable by brute force regardless of hash speed, so a fast
// hash is both sufficient and better suited to a token endpoint that
// needs to stay fast under real request volume.
func GenerateClientSecret() (rawSecret, secretHash string, err error) {
	rawSecret, err = generateRandomToken()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate client secret: %w", err)
	}
	return rawSecret, HashSecret(rawSecret), nil
}

// GenerateAccessToken creates a new random bearer access token plus its
// SHA-256 hash — same mechanism and same reasoning as
// GenerateClientSecret just above, applied to the token returned from
// the token endpoint rather than the long-lived client secret.
func GenerateAccessToken() (rawToken, tokenHash string, err error) {
	rawToken, err = generateRandomToken()
	if err != nil {
		return "", "", fmt.Errorf("failed to generate access token: %w", err)
	}
	return rawToken, HashSecret(rawToken), nil
}

// HashSecret hashes a raw secret (a client secret OR an access token —
// both use the same hash function, see GenerateClientSecret's own doc
// comment for why) the same way the corresponding Generate* function
// does, so a value presented by a caller can be looked up by its hash.
func HashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func generateRandomToken() (string, error) {
	buf := make([]byte, 32) // 256 bits of entropy — matches auth.GenerateSessionToken
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
