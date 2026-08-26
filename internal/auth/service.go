package auth

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DefaultSessionDuration matches a common "stay logged in for a work day"
// expectation; override via Service.SessionDuration.
const DefaultSessionDuration = 24 * time.Hour

// ErrInvalidCredentials is returned by Login for both "no such user" and
// "wrong password" — deliberately the SAME error for both cases, so a
// caller (or an attacker probing the login endpoint) cannot use the error
// message to enumerate which email addresses are registered.
var ErrInvalidCredentials = errors.New("invalid email or password")

// Service wires Store together with the password-hashing and
// session-token logic to provide the higher-level operations HTTP
// handlers actually call (login, logout, authenticate-a-request,
// create-a-user) — handlers should go through Service rather than
// reaching into Store directly.
type Service struct {
	Store           Store
	SessionDuration time.Duration
}

// NewService creates a Service backed by the given Store.
func NewService(store Store) *Service {
	return &Service{Store: store, SessionDuration: DefaultSessionDuration}
}

// Login verifies credentials and, on success, creates a new Session and
// returns the RAW token (the caller sets this as a cookie — see
// SetSessionCookie in middleware.go). Never returns or logs the password
// or the raw token beyond this one call.
func (s *Service) Login(ctx context.Context, email, password string) (rawToken string, user *User, err error) {
	user, err = s.Store.GetUserByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return "", nil, ErrInvalidCredentials
		}
		return "", nil, fmt.Errorf("failed to look up user: %w", err)
	}

	if !VerifyPassword(user.PasswordHash, password) {
		return "", nil, ErrInvalidCredentials
	}

	rawToken, tokenHash, err := GenerateSessionToken()
	if err != nil {
		return "", nil, err
	}

	session := &Session{
		UserID:    user.ID,
		TokenHash: tokenHash,
		ExpiresAt: time.Now().UTC().Add(s.duration()),
	}
	if err := s.Store.CreateSession(ctx, session); err != nil {
		return "", nil, fmt.Errorf("failed to create session: %w", err)
	}

	return rawToken, user, nil
}

// Authenticate resolves a raw session token (e.g. from a cookie) to the
// User it belongs to, or an error if the token is missing, invalid, or
// expired. An expired session is deleted as a side effect (best-effort
// cleanup) rather than left for DeleteExpiredSessions to find later.
func (s *Service) Authenticate(ctx context.Context, rawToken string) (*User, error) {
	if rawToken == "" {
		return nil, fmt.Errorf("no session token provided")
	}

	session, err := s.Store.GetSessionByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("invalid session")
		}
		return nil, fmt.Errorf("failed to look up session: %w", err)
	}

	if time.Now().UTC().After(session.ExpiresAt) {
		_ = s.Store.DeleteSession(ctx, session.ID) // best-effort; a failure here doesn't change the outcome
		return nil, fmt.Errorf("session expired")
	}

	user, err := s.Store.GetUserByID(ctx, session.UserID)
	if err != nil {
		return nil, fmt.Errorf("failed to look up user for session: %w", err)
	}
	return user, nil
}

// Logout invalidates a session by its raw token. Logging out an
// already-invalid/expired token is not an error — the end state (no valid
// session) is what the caller wanted either way.
func (s *Service) Logout(ctx context.Context, rawToken string) error {
	session, err := s.Store.GetSessionByTokenHash(ctx, HashToken(rawToken))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}
	return s.Store.DeleteSession(ctx, session.ID)
}

// CreateUser hashes the password and persists a new user. Used both by
// the admin "add teammate" HTTP endpoint and the `pgarchimigrator auth
// create-admin` CLI bootstrap command.
func (s *Service) CreateUser(ctx context.Context, orgID, email, password string, role Role) (*User, error) {
	hash, err := HashPassword(password)
	if err != nil {
		return nil, err
	}
	user := &User{OrganizationID: orgID, Email: email, PasswordHash: hash, Role: role}
	if err := s.Store.CreateUser(ctx, user); err != nil {
		return nil, err
	}
	return user, nil
}

// duration returns SessionDuration, falling back to
// DefaultSessionDuration only when it's truly unset (the Go zero value).
// Deliberately checks == 0, NOT <= 0: a caller may legitimately set a
// NEGATIVE SessionDuration to force sessions to be created already
// expired (e.g. TestService_Authenticate_ExpiredSession does exactly
// this) — treating every non-positive value as "unset" would silently
// discard that negative value and fall back to the 24-hour default,
// making it impossible to test expiry at all.
func (s *Service) duration() time.Duration {
	if s.SessionDuration == 0 {
		return DefaultSessionDuration
	}
	return s.SessionDuration
}
