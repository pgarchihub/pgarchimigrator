package auth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required — matches internal/state's choice
)

// ErrNotFound is returned when a lookup by ID/email/token finds nothing.
var ErrNotFound = errors.New("not found")

// ErrDuplicateEmail is returned by CreateUser when the email is already
// registered (the users table enforces this with a UNIQUE constraint).
var ErrDuplicateEmail = errors.New("a user with this email already exists")

// timeLayout mirrors internal/state's SQLiteStore: a fixed-width,
// always-UTC layout so that string comparison (used by
// DeleteExpiredSessions' `WHERE expires_at < ?`) matches real
// chronological order — see internal/state/sqlite_store.go's identical
// constant for the full explanation of why RFC3339Nano would NOT be safe
// here (it trims trailing zeros, producing variable-width strings).
const timeLayout = "2006-01-02T15:04:05.000000000Z"

const createAuthSchemaSQL = `
CREATE TABLE IF NOT EXISTS organizations (
	id         TEXT PRIMARY KEY,
	name       TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
	id              TEXT PRIMARY KEY,
	organization_id TEXT NOT NULL,
	email           TEXT NOT NULL UNIQUE,
	password_hash   TEXT NOT NULL,
	role            TEXT NOT NULL,
	created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_users_org ON users (organization_id);
CREATE TABLE IF NOT EXISTS sessions (
	id         TEXT PRIMARY KEY,
	user_id    TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_token_hash ON sessions (token_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_expires_at ON sessions (expires_at);
`

// SQLiteStore is the SQLite-backed implementation of Store. Deliberately
// uses its OWN database file (see NewSQLiteStore), separate from
// internal/state's checkpoint store, so this package has zero dependency
// on pgArchiMigrator-specific code — see the package doc comment.
type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore opens (creating if necessary) a SQLite database at path
// and applies the auth schema.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite (%s): %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite: single writer, same rationale as internal/state

	if _, err := db.Exec(createAuthSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create auth schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close closes the underlying database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) CreateOrganization(ctx context.Context, org *Organization) error {
	if org.ID == "" {
		org.ID = newID("org")
	}
	if org.CreatedAt.IsZero() {
		org.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO organizations (id, name, created_at) VALUES (?, ?, ?)`,
		org.ID, org.Name, org.CreatedAt.Format(timeLayout))
	if err != nil {
		return fmt.Errorf("failed to create organization: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetOrganization(ctx context.Context, id string) (*Organization, error) {
	var org Organization
	var createdAt string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, created_at FROM organizations WHERE id = ?`, id).
		Scan(&org.ID, &org.Name, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get organization: %w", err)
	}
	if org.CreatedAt, err = time.Parse(timeLayout, createdAt); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	return &org, nil
}

func (s *SQLiteStore) CreateUser(ctx context.Context, user *User) error {
	if user.ID == "" {
		user.ID = newID("user")
	}
	if user.CreatedAt.IsZero() {
		user.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, organization_id, email, password_hash, role, created_at)
		VALUES (?, ?, ?, ?, ?, ?)
	`, user.ID, user.OrganizationID, user.Email, user.PasswordHash, string(user.Role), user.CreatedAt.Format(timeLayout))
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrDuplicateEmail
		}
		return fmt.Errorf("failed to create user: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetUserByID(ctx context.Context, id string) (*User, error) {
	return s.scanUserRow(s.db.QueryRowContext(ctx, `
		SELECT id, organization_id, email, password_hash, role, created_at FROM users WHERE id = ?
	`, id))
}

func (s *SQLiteStore) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	return s.scanUserRow(s.db.QueryRowContext(ctx, `
		SELECT id, organization_id, email, password_hash, role, created_at FROM users WHERE email = ?
	`, email))
}

func (s *SQLiteStore) scanUserRow(row *sql.Row) (*User, error) {
	var user User
	var role, createdAt string
	err := row.Scan(&user.ID, &user.OrganizationID, &user.Email, &user.PasswordHash, &role, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to scan user: %w", err)
	}
	user.Role = Role(role)
	if user.CreatedAt, err = time.Parse(timeLayout, createdAt); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	return &user, nil
}

func (s *SQLiteStore) ListUsersByOrganization(ctx context.Context, orgID string) ([]*User, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, organization_id, email, password_hash, role, created_at
		FROM users WHERE organization_id = ? ORDER BY created_at
	`, orgID)
	if err != nil {
		return nil, fmt.Errorf("failed to list users: %w", err)
	}
	defer rows.Close()

	var users []*User
	for rows.Next() {
		var user User
		var role, createdAt string
		if err := rows.Scan(&user.ID, &user.OrganizationID, &user.Email, &user.PasswordHash, &role, &createdAt); err != nil {
			return nil, fmt.Errorf("failed to scan user row: %w", err)
		}
		user.Role = Role(role)
		if user.CreatedAt, err = time.Parse(timeLayout, createdAt); err != nil {
			return nil, fmt.Errorf("failed to parse created_at: %w", err)
		}
		users = append(users, &user)
	}
	return users, rows.Err()
}

func (s *SQLiteStore) DeleteUser(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete user: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) UpdateUserRole(ctx context.Context, id string, role Role) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET role = ? WHERE id = ?`, string(role), id)
	if err != nil {
		return fmt.Errorf("failed to update user role: %w", err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) CreateSession(ctx context.Context, session *Session) error {
	if session.ID == "" {
		session.ID = newID("sess")
	}
	if session.CreatedAt.IsZero() {
		session.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, user_id, token_hash, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?)
	`, session.ID, session.UserID, session.TokenHash,
		session.ExpiresAt.UTC().Format(timeLayout), session.CreatedAt.Format(timeLayout))
	if err != nil {
		return fmt.Errorf("failed to create session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	var session Session
	var expiresAt, createdAt string
	err := s.db.QueryRowContext(ctx, `
		SELECT id, user_id, token_hash, expires_at, created_at FROM sessions WHERE token_hash = ?
	`, tokenHash).Scan(&session.ID, &session.UserID, &session.TokenHash, &expiresAt, &createdAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get session: %w", err)
	}
	if session.ExpiresAt, err = time.Parse(timeLayout, expiresAt); err != nil {
		return nil, fmt.Errorf("failed to parse expires_at: %w", err)
	}
	if session.CreatedAt, err = time.Parse(timeLayout, createdAt); err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	return &session, nil
}

func (s *SQLiteStore) DeleteSession(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("failed to delete session: %w", err)
	}
	return nil
}

func (s *SQLiteStore) DeleteExpiredSessions(ctx context.Context) (int64, error) {
	now := time.Now().UTC().Format(timeLayout)
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, now)
	if err != nil {
		return 0, fmt.Errorf("failed to delete expired sessions: %w", err)
	}
	return res.RowsAffected()
}

// newID generates a reasonably unique, non-guessable identifier — the same
// approach as internal/orchestrator.generateJobID (crypto/rand, no
// external UUID dependency needed for a single-instance deployment).
func newID(prefix string) string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf) // crypto/rand.Read never partially fails in practice; error is non-actionable here
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(buf))
}

// isUniqueConstraintErr checks for SQLite's UNIQUE constraint violation by
// substring match on the driver's error text — modernc.org/sqlite doesn't
// export a typed sentinel for this the way some other SQL drivers do, so a
// substring check on the message (stable across the driver's versions) is
// the pragmatic option here.
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
