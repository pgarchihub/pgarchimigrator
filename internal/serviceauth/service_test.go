package serviceauth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeStore is a minimal in-memory Store — real behavior, no database.
type fakeStore struct {
	clients []*Client
	tokens  []*AccessToken
	nextID  int
}

func newFakeStore() *fakeStore {
	return &fakeStore{}
}

func (f *fakeStore) newID() string {
	f.nextID++
	return fmt.Sprintf("id-%d", f.nextID)
}

func (f *fakeStore) CreateClient(ctx context.Context, c *Client) error {
	c.ID = f.newID()
	c.CreatedAt = time.Now().UTC()
	f.clients = append(f.clients, c)
	return nil
}

func (f *fakeStore) GetClientByClientID(ctx context.Context, clientID string) (*Client, error) {
	for _, c := range f.clients {
		if c.ClientID == clientID {
			return c, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) ListClients(ctx context.Context) ([]*Client, error) {
	return f.clients, nil
}

func (f *fakeStore) DeleteClient(ctx context.Context, id string) error {
	for i, c := range f.clients {
		if c.ID == id {
			f.clients = append(f.clients[:i], f.clients[i+1:]...)
			return nil
		}
	}
	return ErrNotFound
}

func (f *fakeStore) CreateAccessToken(ctx context.Context, t *AccessToken) error {
	t.ID = f.newID()
	t.CreatedAt = time.Now().UTC()
	f.tokens = append(f.tokens, t)
	return nil
}

func (f *fakeStore) GetAccessTokenByHash(ctx context.Context, tokenHash string) (*AccessToken, error) {
	for _, t := range f.tokens {
		if t.TokenHash == tokenHash {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

func (f *fakeStore) DeleteExpiredAccessTokens(ctx context.Context) (int64, error) {
	now := time.Now().UTC()
	var kept []*AccessToken
	var deleted int64
	for _, t := range f.tokens {
		if now.After(t.ExpiresAt) {
			deleted++
			continue
		}
		kept = append(kept, t)
	}
	f.tokens = kept
	return deleted, nil
}

func setupClient(t *testing.T, store *fakeStore, scopes []string) (clientID, clientSecret string) {
	t.Helper()
	rawSecret, secretHash, err := GenerateClientSecret()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	client := &Client{Name: "test-client", ClientID: "test-client-id", ClientSecretHash: secretHash, Scopes: scopes}
	if err := store.CreateClient(context.Background(), client); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return client.ClientID, rawSecret
}

func TestGenerateClientSecret_ProducesDistinctValuesEachCall(t *testing.T) {
	raw1, hash1, _ := GenerateClientSecret()
	raw2, hash2, _ := GenerateClientSecret()
	if raw1 == raw2 {
		t.Error("expected two calls to produce distinct raw secrets")
	}
	if hash1 == hash2 {
		t.Error("expected two calls to produce distinct hashes")
	}
}

func TestHashSecret_IsDeterministic(t *testing.T) {
	raw, hash, _ := GenerateClientSecret()
	if HashSecret(raw) != hash {
		t.Error("expected HashSecret(raw) to match the hash returned alongside it")
	}
}

func TestIssueToken_ValidCredentials_Succeeds(t *testing.T) {
	store := newFakeStore()
	clientID, clientSecret := setupClient(t, store, []string{"pgarchimigrator.read", "pgarchimigrator.migrate"})
	svc := NewService(store)

	rawToken, granted, expiresAt, err := svc.IssueToken(context.Background(), clientID, clientSecret, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if rawToken == "" {
		t.Error("expected a non-empty token")
	}
	if len(granted) != 2 {
		t.Errorf("expected all 2 of the client's scopes to be granted for an empty request, got %v", granted)
	}
	if !expiresAt.After(time.Now().UTC()) {
		t.Error("expected expiresAt to be in the future")
	}
}

// TestIssueToken_WrongSecret_And_UnknownClientID_ReturnTheSameError is
// the direct regression test for the enumeration-resistance property
// this package deliberately mirrors from auth.ErrInvalidCredentials —
// a caller probing valid client IDs must not be able to distinguish
// "wrong secret" from "no such client" by the error returned.
func TestIssueToken_WrongSecret_And_UnknownClientID_ReturnTheSameError(t *testing.T) {
	store := newFakeStore()
	clientID, _ := setupClient(t, store, []string{"pgarchimigrator.read"})
	svc := NewService(store)

	_, _, _, errWrongSecret := svc.IssueToken(context.Background(), clientID, "wrong-secret", nil)
	_, _, _, errUnknownClient := svc.IssueToken(context.Background(), "nonexistent-client-id", "anything", nil)

	if !errors.Is(errWrongSecret, ErrInvalidClient) {
		t.Errorf("expected ErrInvalidClient for a wrong secret, got %v", errWrongSecret)
	}
	if !errors.Is(errUnknownClient, ErrInvalidClient) {
		t.Errorf("expected ErrInvalidClient for an unknown client_id, got %v", errUnknownClient)
	}
}

func TestIssueToken_RequestedScopeSubsetOfAllowed_Succeeds(t *testing.T) {
	store := newFakeStore()
	clientID, clientSecret := setupClient(t, store, []string{"pgarchimigrator.read", "pgarchimigrator.migrate"})
	svc := NewService(store)

	_, granted, _, err := svc.IssueToken(context.Background(), clientID, clientSecret, []string{"pgarchimigrator.read"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(granted) != 1 || granted[0] != "pgarchimigrator.read" {
		t.Errorf("expected exactly the requested scope to be granted, got %v", granted)
	}
}

// TestIssueToken_RequestedScopeExceedsAllowed_Fails is the direct
// regression test for IssueToken's own documented behavior: requesting
// a scope outside what the client is registered for must fail loudly
// (ErrInvalidScope), never silently narrow to a smaller grant.
func TestIssueToken_RequestedScopeExceedsAllowed_Fails(t *testing.T) {
	store := newFakeStore()
	clientID, clientSecret := setupClient(t, store, []string{"pgarchimigrator.read"})
	svc := NewService(store)

	_, _, _, err := svc.IssueToken(context.Background(), clientID, clientSecret, []string{"pgarchimigrator.read", "pgarchimigrator.migrate"})
	if !errors.Is(err, ErrInvalidScope) {
		t.Errorf("expected ErrInvalidScope, got %v", err)
	}
}

func TestAuthenticate_ValidToken_ReturnsClientAndScopes(t *testing.T) {
	store := newFakeStore()
	clientID, clientSecret := setupClient(t, store, []string{"pgarchimigrator.migrate"})
	svc := NewService(store)
	rawToken, _, _, _ := svc.IssueToken(context.Background(), clientID, clientSecret, nil)

	client, scopes, err := svc.Authenticate(context.Background(), rawToken)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if client.ClientID != clientID {
		t.Errorf("expected client %q, got %q", clientID, client.ClientID)
	}
	if len(scopes) != 1 || scopes[0] != "pgarchimigrator.migrate" {
		t.Errorf("expected the token's own granted scopes, got %v", scopes)
	}
}

func TestAuthenticate_UnknownToken_Fails(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)

	_, _, err := svc.Authenticate(context.Background(), "totally-made-up-token")
	if err == nil {
		t.Fatal("expected an error for an unknown token")
	}
}

func TestAuthenticate_EmptyToken_Fails(t *testing.T) {
	store := newFakeStore()
	svc := NewService(store)

	_, _, err := svc.Authenticate(context.Background(), "")
	if err == nil {
		t.Fatal("expected an error for an empty token")
	}
}

// TestAuthenticate_ExpiredToken_Fails mirrors
// auth.TestService_Authenticate_ExpiredSession's exact technique —
// setting a deliberately negative AccessTokenDuration so a freshly
// issued token is already expired, without needing to sleep in a test.
func TestAuthenticate_ExpiredToken_Fails(t *testing.T) {
	store := newFakeStore()
	clientID, clientSecret := setupClient(t, store, []string{"pgarchimigrator.read"})
	svc := NewService(store)
	svc.AccessTokenDuration = -1 * time.Hour // already expired the moment it's issued

	rawToken, _, _, err := svc.IssueToken(context.Background(), clientID, clientSecret, nil)
	if err != nil {
		t.Fatalf("unexpected error issuing the token: %v", err)
	}

	_, _, err = svc.Authenticate(context.Background(), rawToken)
	if err == nil {
		t.Fatal("expected an error for an expired token")
	}
}

func TestScopesSubsetOf(t *testing.T) {
	if !scopesSubsetOf([]string{"a"}, []string{"a", "b"}) {
		t.Error("expected {a} to be a subset of {a, b}")
	}
	if scopesSubsetOf([]string{"a", "c"}, []string{"a", "b"}) {
		t.Error("expected {a, c} to NOT be a subset of {a, b} (c is missing)")
	}
	if !scopesSubsetOf(nil, []string{"a"}) {
		t.Error("expected an empty/nil requested set to always be a subset")
	}
}
