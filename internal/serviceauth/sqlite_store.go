package serviceauth

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo required — matches internal/auth's own choice
)

// ErrDuplicateClientID is returned by CreateClient when the client_id is
// already registered (the service_clients table enforces this with a
// UNIQUE constraint) — mirrors auth.ErrDuplicateEmail's exact role for
// the equivalent uniqueness constraint on User.Email.
var ErrDuplicateClientID = errors.New("a client with this client_id already exists")

// timeLayout mirrors internal/auth's own SQLiteStore identically — a
// fixed-width, always-UTC layout so string comparison (used by
// DeleteExpiredAccessTokens' `WHERE expires_at < ?`) matches real
// chronological order. See internal/state/sqlite_store.go's original,
// more detailed explanation of why RFC3339Nano would NOT be safe here.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

const createServiceAuthSchemaSQL = `
CREATE TABLE IF NOT EXISTS service_clients (
	id                 TEXT PRIMARY KEY,
	name               TEXT NOT NULL,
	client_id          TEXT NOT NULL UNIQUE,
	client_secret_hash TEXT NOT NULL,
	scopes             TEXT NOT NULL,
	created_at         TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_service_clients_client_id ON service_clients (client_id);
CREATE TABLE IF NOT EXISTS service_access_tokens (
	id         TEXT PRIMARY KEY,
	client_id  TEXT NOT NULL,
	token_hash TEXT NOT NULL UNIQUE,
	scopes     TEXT NOT NULL,
	expires_at TEXT NOT NULL,
	created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_service_access_tokens_token_hash ON service_access_tokens (token_hash);
CREATE INDEX IF NOT EXISTS idx_service_access_tokens_expires_at ON service_access_tokens (expires_at);
`

// SQLiteStore is the SQLite-backed implementation of Store. Deliberately
// uses its OWN database file (see NewSQLiteStore) rather than sharing
// internal/state's checkpoint database — same "zero dependency on
// pgArchiMigrator-specific code" reasoning as internal/auth's own
// SQLiteStore, which this package mirrors throughout; a caller wiring
// this product together (see cmd/pgarchimigrator's buildWiring) is free
// to point this at the SAME physical .db file internal/auth uses, since
// SQLite has no objection to multiple unrelated table sets living in one
// file — that's a wiring-level convenience decision, not something this
// package assumes.
type SQLiteStore struct {
	db *sql.DB
}

var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore opens (creating if necessary) a SQLite database at path
// and applies the service-auth schema.
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("failed to open sqlite (%s): %w", path, err)
	}
	db.SetMaxOpenConns(1) // SQLite: single writer, same rationale as internal/state and internal/auth

	if _, err := db.Exec(createServiceAuthSchemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create service-auth schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

// Close closes the underlying database connection.
func (s *SQLiteStore) Close() error {
	return s.db.Close()
}

func (s *SQLiteStore) CreateClient(ctx context.Context, client *Client) error {
	if client.ID == "" {
		client.ID = newID("svcclient")
	}
	if client.CreatedAt.IsZero() {
		client.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO service_clients (id, name, client_id, client_secret_hash, scopes, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		client.ID, client.Name, client.ClientID, client.ClientSecretHash, encodeScopes(client.Scopes), client.CreatedAt.Format(timeLayout),
	)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return ErrDuplicateClientID
		}
		return fmt.Errorf("failed to create client: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetClientByClientID(ctx context.Context, clientID string) (*Client, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, name, client_id, client_secret_hash, scopes, created_at FROM service_clients WHERE client_id = ?`, clientID)
	return scanClient(row)
}

func (s *SQLiteStore) ListClients(ctx context.Context) ([]*Client, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, client_id, client_secret_hash, scopes, created_at FROM service_clients ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("failed to list clients: %w", err)
	}
	defer rows.Close()

	var clients []*Client
	for rows.Next() {
		client, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	return clients, rows.Err()
}

func (s *SQLiteStore) DeleteClient(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM service_clients WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete client: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) CreateAccessToken(ctx context.Context, token *AccessToken) error {
	if token.ID == "" {
		token.ID = newID("svctoken")
	}
	if token.CreatedAt.IsZero() {
		token.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO service_access_tokens (id, client_id, token_hash, scopes, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
		token.ID, token.ClientID, token.TokenHash, encodeScopes(token.Scopes), token.ExpiresAt.UTC().Format(timeLayout), token.CreatedAt.Format(timeLayout),
	)
	if err != nil {
		return fmt.Errorf("failed to create access token: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetAccessTokenByHash(ctx context.Context, tokenHash string) (*AccessToken, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, client_id, token_hash, scopes, expires_at, created_at FROM service_access_tokens WHERE token_hash = ?`, tokenHash)
	return scanAccessToken(row)
}

func (s *SQLiteStore) DeleteExpiredAccessTokens(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM service_access_tokens WHERE expires_at < ?`, time.Now().UTC().Format(timeLayout))
	if err != nil {
		return 0, fmt.Errorf("failed to delete expired access tokens: %w", err)
	}
	return res.RowsAffected()
}

// rowScanner lets scanClient/scanAccessToken work with both
// *sql.Row (QueryRowContext) and *sql.Rows (QueryContext) — same
// pattern internal/state.scanJob uses for the identical reason.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanClient(row rowScanner) (*Client, error) {
	var c Client
	var scopesRaw, createdAt string
	if err := row.Scan(&c.ID, &c.Name, &c.ClientID, &c.ClientSecretHash, &scopesRaw, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to scan client: %w", err)
	}
	c.Scopes = decodeScopes(scopesRaw)
	parsed, err := time.Parse(timeLayout, createdAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	c.CreatedAt = parsed
	return &c, nil
}

func scanAccessToken(row rowScanner) (*AccessToken, error) {
	var t AccessToken
	var scopesRaw, expiresAt, createdAt string
	if err := row.Scan(&t.ID, &t.ClientID, &t.TokenHash, &scopesRaw, &expiresAt, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("failed to scan access token: %w", err)
	}
	t.Scopes = decodeScopes(scopesRaw)
	parsedExpires, err := time.Parse(timeLayout, expiresAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse expires_at: %w", err)
	}
	t.ExpiresAt = parsedExpires
	parsedCreated, err := time.Parse(timeLayout, createdAt)
	if err != nil {
		return nil, fmt.Errorf("failed to parse created_at: %w", err)
	}
	t.CreatedAt = parsedCreated
	return &t, nil
}

// encodeScopes/decodeScopes store Scopes as a single comma-separated
// column rather than a separate join table — deliberately simple for
// what's expected to be a short, small list (a handful of scopes per
// client/token, not an unbounded set), matching this project's own
// established "don't reach for a JSON column or a join table when a
// delimited string genuinely is enough" pattern (see e.g.
// strategy.ColumnChange.PartitionBoundsJSON's own doc comment for a
// case where JSON WAS warranted, for contrast — that's a variable-shape
// structure; this is a flat list of short strings, a strictly simpler
// case).
func encodeScopes(scopes []string) string {
	return strings.Join(scopes, ",")
}

func decodeScopes(raw string) []string {
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func newID(prefix string) string {
	buf := make([]byte, 12)
	_, _ = rand.Read(buf) // crypto/rand.Read never partially fails in practice; error is non-actionable here
	return fmt.Sprintf("%s_%s", prefix, hex.EncodeToString(buf))
}

// isUniqueConstraintErr checks for SQLite's UNIQUE constraint violation
// by substring match on the driver's error text — same technique and
// same reasoning as internal/auth's identical helper: modernc.org/sqlite
// doesn't export a typed sentinel for this.
func isUniqueConstraintErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}
