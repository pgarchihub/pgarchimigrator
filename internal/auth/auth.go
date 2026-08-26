// Package auth implements a general-purpose authentication/authorization
// layer: organizations, users, roles (RBAC), and sessions.
//
// Deliberately generic — nothing in this package references migrations,
// jobs, or any other pgArchiMigrator-specific concept — so it can be extracted
// into a shared library for other products in the same family later
// without rework. It is embedded directly in this binary for now rather
// than run as a separate service, since standing up a separate control
// plane before any single product has real users would be solving a
// problem that doesn't exist yet.
package auth

import (
	"context"
	"errors"
	"time"
)

// Role is a coarse-grained permission level within an Organization.
type Role string

const (
	// RoleViewer can view dashboards/status but cannot start migrations,
	// roll back, or manage users.
	RoleViewer Role = "viewer"
	// RoleOperator can additionally start migrations and roll them back —
	// the day-to-day "does the work" role.
	RoleOperator Role = "operator"
	// RoleAdmin can additionally manage users and organization settings.
	RoleAdmin Role = "admin"
)

// roleRank gives each role a numeric level for "at least this role" checks.
var roleRank = map[Role]int{
	RoleViewer:   1,
	RoleOperator: 2,
	RoleAdmin:    3,
}

// Satisfies reports whether role r meets or exceeds the required role —
// e.g. RoleAdmin.Satisfies(RoleOperator) is true, but the reverse is false.
// An unrecognized role satisfies nothing (roleRank's zero value is 0,
// lower than every real role), which fails closed rather than open.
func (r Role) Satisfies(required Role) bool {
	return roleRank[r] >= roleRank[required]
}

// Organization is the tenant boundary. A self-hosted deployment will
// typically have exactly one; a future multi-tenant SaaS deployment could
// have many, sharing the same database with rows scoped by
// OrganizationID — see the package doc comment.
type Organization struct {
	ID        string
	Name      string
	CreatedAt time.Time
}

// User belongs to exactly one Organization and has exactly one Role
// within it. PasswordHash is a bcrypt hash — see password.go — and must
// never hold a plaintext password.
type User struct {
	ID             string
	OrganizationID string
	Email          string
	PasswordHash   string
	Role           Role
	CreatedAt      time.Time
}

// Session represents a logged-in session. TokenHash is a SHA-256 hash of
// the raw session token: the raw token exists only in the client's cookie
// and briefly in this server's memory at issuance time — it is never
// persisted in plaintext, so a database leak alone cannot be used to
// impersonate active sessions.
type Session struct {
	ID        string
	UserID    string
	TokenHash string
	ExpiresAt time.Time
	CreatedAt time.Time
}

// Store persists organizations, users, and sessions.
type Store interface {
	CreateOrganization(ctx context.Context, org *Organization) error
	GetOrganization(ctx context.Context, id string) (*Organization, error)

	CreateUser(ctx context.Context, user *User) error
	GetUserByID(ctx context.Context, id string) (*User, error)
	GetUserByEmail(ctx context.Context, email string) (*User, error)
	ListUsersByOrganization(ctx context.Context, orgID string) ([]*User, error)
	DeleteUser(ctx context.Context, id string) error
	// UpdateUserRole changes an existing user's role in place — the
	// alternative (delete-then-recreate) would also silently invalidate
	// every one of that user's active sessions and lose their user ID
	// (breaking anything that referenced it, e.g. audit log entries),
	// neither of which is what "change this person's role" should do.
	UpdateUserRole(ctx context.Context, id string, role Role) error

	CreateSession(ctx context.Context, session *Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	DeleteSession(ctx context.Context, id string) error
	// DeleteExpiredSessions removes sessions past their ExpiresAt and
	// returns how many were deleted — intended to be called periodically
	// (e.g. from the same background loop as internal/reaper) to keep the
	// sessions table from growing unbounded.
	DeleteExpiredSessions(ctx context.Context) (int64, error)
}

// DefaultOrgID is a fixed, well-known ID for the single organization in a
// self-hosted, single-tenant deployment — see EnsureDefaultOrganization.
// A future multi-tenant product built on this package would create
// Organizations explicitly per customer instead and never call
// EnsureDefaultOrganization at all.
const DefaultOrgID = "default"

// EnsureDefaultOrganization returns the single default organization,
// creating it (with the given display name) on first use. This is the
// bootstrap path for self-hosted deployments — see `pgarchimigrator auth
// create-admin`, the only caller today.
func EnsureDefaultOrganization(ctx context.Context, store Store, name string) (*Organization, error) {
	org, err := store.GetOrganization(ctx, DefaultOrgID)
	if err == nil {
		return org, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return nil, err
	}
	org = &Organization{ID: DefaultOrgID, Name: name}
	if err := store.CreateOrganization(ctx, org); err != nil {
		return nil, err
	}
	return org, nil
}
