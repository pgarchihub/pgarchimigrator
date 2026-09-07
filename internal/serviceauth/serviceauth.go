// Package serviceauth implements OAuth2 client-credentials
// authentication for machine/service callers — as an ADDITION alongside
// internal/auth's existing cookie-based human authentication, never a
// replacement for it. See docs/ecosystem/ARCHITECTURE.md and AC-PF-003
// Section 13 for why this exists: the Archi ecosystem (ArchiConsole and
// other products acting on its behalf) needs to call this product's API
// as itself, not as any particular human user, and a session cookie
// obtained by logging in as a person is the wrong credential for that.
//
// Deliberately mirrors internal/auth's own shape (Client instead of
// User, AccessToken instead of Session, the same
// generate-random-secret-then-store-only-its-hash discipline — see
// GenerateClientSecret/GenerateAccessToken) rather than introducing a
// different pattern for what is, structurally, the same problem
// (long-lived credential -> short-lived proof-of-possession token) with
// a different audience (services, not people). Kept as its own package
// rather than folded into internal/auth specifically because
// internal/auth's own package doc comment describes it as deliberately
// generic and "nothing... references migrations, jobs, or any other
// pgArchiMigrator-specific concept" — OAuth2 client-credentials support
// is a genuinely different authentication MODEL (bearer tokens issued
// from a secret, not cookies issued from a password), not a variation
// on the same one, and conflating the two would make internal/auth's
// own "could be extracted into a shared library" claim less true, not
// more.
//
// This package does NOT implement a general-purpose OAuth2 authorization
// server (no authorization_code grant, no PKCE, no refresh tokens,
// no OIDC discovery) — only the client_credentials grant AC-PF-003
// Section 13's service-to-service model actually calls for. See
// Service.IssueToken's own doc comment for the exact grant this
// implements.
package serviceauth

import (
	"context"
	"errors"
	"time"
)

// ErrNotFound is returned by every Store lookup method when the
// requested row doesn't exist — mirrors internal/auth.ErrNotFound
// exactly, kept as this package's own distinct value (not an alias) so
// this package has zero import-time dependency on internal/auth, per
// this file's own package doc comment.
var ErrNotFound = errors.New("not found")

// Client is a registered service caller — e.g. one ArchiConsole
// deployment, or any other ecosystem product/automation invoking this
// product's API as itself. ClientSecretHash is a SHA-256 hash of the
// raw client secret (see GenerateClientSecret): the raw secret is shown
// to whoever registers the client exactly once, at creation time, and
// is never persisted or retrievable again — same discipline as
// auth.Session.TokenHash, for the same reason (a database leak alone
// must not be enough to impersonate a client).
type Client struct {
	ID   string
	Name string
	// ClientID is the public identifier presented alongside the secret
	// during token issuance (the OAuth2 "client_id" parameter) — safe
	// to log and display, unlike the secret itself.
	ClientID         string
	ClientSecretHash string
	// Scopes this client is allowed to request — see AC-PF-003 Section
	// 13.1's requiredScopes on an action descriptor for the ecosystem
	// side of this same idea. A token issued to this client can never
	// carry a scope outside this list, regardless of what's requested
	// at token-issuance time — see Service.IssueToken.
	Scopes    []string
	CreatedAt time.Time
}

// AccessToken represents one issued, still-potentially-valid bearer
// token. TokenHash follows the exact same "never store the raw value"
// discipline as Client.ClientSecretHash/auth.Session.TokenHash.
type AccessToken struct {
	ID        string
	ClientID  string // Client.ID (the internal row ID, not Client.ClientID) this token was issued to
	TokenHash string
	// Scopes actually granted to this specific token — see
	// Service.IssueToken's own doc comment for why this can be a
	// SUBSET of Client.Scopes, not always the full set.
	Scopes    []string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Store persists service clients and the access tokens issued to them.
type Store interface {
	CreateClient(ctx context.Context, client *Client) error
	GetClientByClientID(ctx context.Context, clientID string) (*Client, error)
	ListClients(ctx context.Context) ([]*Client, error)
	DeleteClient(ctx context.Context, id string) error

	CreateAccessToken(ctx context.Context, token *AccessToken) error
	GetAccessTokenByHash(ctx context.Context, tokenHash string) (*AccessToken, error)
	// DeleteExpiredAccessTokens removes tokens past their ExpiresAt and
	// returns how many were deleted — same "periodic cleanup" role as
	// auth.Store.DeleteExpiredSessions, intended to be called from the
	// same background loop.
	DeleteExpiredAccessTokens(ctx context.Context) (int64, error)
}
