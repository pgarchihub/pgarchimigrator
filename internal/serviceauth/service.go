package serviceauth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultAccessTokenDuration — one hour, a conventional OAuth2
// client-credentials default (short enough that a leaked token has a
// bounded blast radius; long enough that a caller doing a batch of
// requests isn't constantly re-authenticating). Override via
// Service.AccessTokenDuration.
const DefaultAccessTokenDuration = 1 * time.Hour

// ErrInvalidClient is returned by IssueToken for both "no such
// client_id" and "wrong client_secret" — deliberately the SAME error
// for both cases, for the exact same enumeration-resistance reasoning
// as auth.ErrInvalidCredentials. Maps to OAuth2's own "invalid_client"
// error (RFC 6749 Section 5.2) at the HTTP handler layer.
var ErrInvalidClient = errors.New("invalid client credentials")

// ErrInvalidScope is returned by IssueToken when the caller requests a
// scope the client isn't registered for — maps to OAuth2's own
// "invalid_scope" error (RFC 6749 Section 5.2).
var ErrInvalidScope = errors.New("requested scope exceeds what this client is registered for")

// Service wires Store together with the secret/token generation logic
// to provide client-credentials token issuance and bearer-token
// authentication — mirrors auth.Service's own role exactly, for the
// human-auth equivalent of the same two operations (Login/Authenticate
// there, IssueToken/Authenticate here).
type Service struct {
	Store               Store
	AccessTokenDuration time.Duration
}

func NewService(store Store) *Service {
	return &Service{Store: store, AccessTokenDuration: DefaultAccessTokenDuration}
}

// IssueToken implements the OAuth2 client_credentials grant (RFC 6749
// Section 4.4) — the ONE grant type this package supports (see the
// package doc comment for why not the others). Verifies clientID/
// clientSecret, then issues a token scoped to requestedScopes.
//
// requestedScopes, if non-empty, MUST be a subset of the client's own
// registered Scopes (Client.Scopes) — requesting anything outside that
// set fails with ErrInvalidScope rather than silently narrowing to what
// IS allowed, so a caller with a typo'd or overly broad scope request
// gets a clear signal, not a token quietly granting less than it asked
// for. An EMPTY requestedScopes is treated as "grant everything this
// client is registered for" (RFC 6749 Section 3.3's own "if the client
// omits the scope parameter... the … full set" behavior), not as
// "grant nothing."
func (s *Service) IssueToken(ctx context.Context, clientID, clientSecret string, requestedScopes []string) (rawToken string, grantedScopes []string, expiresAt time.Time, err error) {
	client, err := s.Store.GetClientByClientID(ctx, clientID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil, time.Time{}, ErrInvalidClient
		}
		return "", nil, time.Time{}, fmt.Errorf("failed to look up client: %w", err)
	}

	if HashSecret(clientSecret) != client.ClientSecretHash {
		return "", nil, time.Time{}, ErrInvalidClient
	}

	if len(requestedScopes) == 0 {
		grantedScopes = client.Scopes
	} else {
		if !scopesSubsetOf(requestedScopes, client.Scopes) {
			return "", nil, time.Time{}, ErrInvalidScope
		}
		grantedScopes = requestedScopes
	}

	rawToken, tokenHash, err := GenerateAccessToken()
	if err != nil {
		return "", nil, time.Time{}, err
	}

	expiresAt = time.Now().UTC().Add(s.duration())
	token := &AccessToken{
		ClientID:  client.ID,
		TokenHash: tokenHash,
		Scopes:    grantedScopes,
		ExpiresAt: expiresAt,
	}
	if err := s.Store.CreateAccessToken(ctx, token); err != nil {
		return "", nil, time.Time{}, fmt.Errorf("failed to persist access token: %w", err)
	}

	return rawToken, grantedScopes, expiresAt, nil
}

// Authenticate resolves a raw bearer token (from an Authorization:
// Bearer header) to the Client it was issued to and the scopes THAT
// TOKEN carries — which may be a subset of the client's full Scopes,
// see IssueToken's own doc comment — or an error if the token is
// missing, invalid, or expired. Mirrors auth.Service.Authenticate's
// exact shape and the same "delete an expired token as a side effect of
// finding it, rather than leaving it for the periodic cleanup pass"
// behavior.
func (s *Service) Authenticate(ctx context.Context, rawToken string) (*Client, []string, error) {
	if rawToken == "" {
		return nil, nil, fmt.Errorf("no access token provided")
	}

	token, err := s.Store.GetAccessTokenByHash(ctx, HashSecret(rawToken))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, nil, fmt.Errorf("invalid access token")
		}
		return nil, nil, fmt.Errorf("failed to look up access token: %w", err)
	}

	if time.Now().UTC().After(token.ExpiresAt) {
		// Best-effort cleanup, same reasoning as
		// auth.Service.Authenticate's identical expired-session handling
		// — a failure here doesn't change the outcome for this request.
		_, _ = s.Store.DeleteExpiredAccessTokens(ctx)
		return nil, nil, fmt.Errorf("access token expired")
	}

	client, err := clientByID(ctx, s.Store, token.ClientID)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to look up client for access token: %w", err)
	}
	return client, token.Scopes, nil
}

// clientByID is a small helper since Store's own interface only offers
// GetClientByClientID (looking up by the PUBLIC client_id, what a token
// request presents) — Authenticate needs to resolve the INTERNAL row ID
// an AccessToken references back to a full Client. Implemented as a
// linear scan over ListClients rather than adding a second Store lookup
// method just for this one internal call site — the number of
// registered service clients for a single self-hosted instance is
// expected to be small (a handful of ecosystem products, not
// thousands), so this is deliberately not optimized further.
func clientByID(ctx context.Context, store Store, id string) (*Client, error) {
	clients, err := store.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, ErrNotFound
}

// scopesSubsetOf reports whether every scope in requested also appears
// in allowed.
func scopesSubsetOf(requested, allowed []string) bool {
	allowedSet := make(map[string]bool, len(allowed))
	for _, s := range allowed {
		allowedSet[s] = true
	}
	for _, s := range requested {
		if !allowedSet[s] {
			return false
		}
	}
	return true
}

// duration returns AccessTokenDuration, falling back to
// DefaultAccessTokenDuration only when truly unset — same == 0 (not <=
// 0) reasoning as auth.Service.duration, so a deliberately-negative
// value (for testing expiry) isn't silently discarded.
func (s *Service) duration() time.Duration {
	if s.AccessTokenDuration == 0 {
		return DefaultAccessTokenDuration
	}
	return s.AccessTokenDuration
}
