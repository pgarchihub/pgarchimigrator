package auth

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "auth-test.db")
	store, err := NewSQLiteStore(dbPath)
	if err != nil {
		t.Fatalf("could not create store: %v", err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestSQLiteStore_Organization_CreateAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	org := &Organization{Name: "Acme Corp"}
	if err := store.CreateOrganization(ctx, org); err != nil {
		t.Fatalf("CreateOrganization failed: %v", err)
	}
	if org.ID == "" {
		t.Fatal("expected an auto-generated ID")
	}

	got, err := store.GetOrganization(ctx, org.ID)
	if err != nil {
		t.Fatalf("GetOrganization failed: %v", err)
	}
	if got.Name != "Acme Corp" {
		t.Errorf("expected Name='Acme Corp', got %q", got.Name)
	}
}

func TestSQLiteStore_GetOrganization_NotFound(t *testing.T) {
	store := newTestStore(t)
	_, err := store.GetOrganization(context.Background(), "nonexistent")
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestSQLiteStore_User_CreateAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	org := &Organization{Name: "Acme Corp"}
	if err := store.CreateOrganization(ctx, org); err != nil {
		t.Fatalf("CreateOrganization failed: %v", err)
	}

	user := &User{OrganizationID: org.ID, Email: "alice@acme.test", PasswordHash: "hash", Role: RoleAdmin}
	if err := store.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}
	if user.ID == "" {
		t.Fatal("expected an auto-generated ID")
	}

	byID, err := store.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserByID failed: %v", err)
	}
	if byID.Email != "alice@acme.test" || byID.Role != RoleAdmin {
		t.Errorf("unexpected user: %+v", byID)
	}

	byEmail, err := store.GetUserByEmail(ctx, "alice@acme.test")
	if err != nil {
		t.Fatalf("GetUserByEmail failed: %v", err)
	}
	if byEmail.ID != user.ID {
		t.Error("expected GetUserByEmail to find the same user as GetUserByID")
	}
}

func TestSQLiteStore_CreateUser_DuplicateEmail(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	org := &Organization{Name: "Acme Corp"}
	if err := store.CreateOrganization(ctx, org); err != nil {
		t.Fatalf("CreateOrganization failed: %v", err)
	}

	first := &User{OrganizationID: org.ID, Email: "dup@acme.test", PasswordHash: "hash", Role: RoleViewer}
	if err := store.CreateUser(ctx, first); err != nil {
		t.Fatalf("first CreateUser failed: %v", err)
	}

	second := &User{OrganizationID: org.ID, Email: "dup@acme.test", PasswordHash: "hash2", Role: RoleOperator}
	err := store.CreateUser(ctx, second)
	if err != ErrDuplicateEmail {
		t.Errorf("expected ErrDuplicateEmail, got %v", err)
	}
}

func TestSQLiteStore_ListUsersByOrganization(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	orgA := &Organization{Name: "Org A"}
	orgB := &Organization{Name: "Org B"}
	_ = store.CreateOrganization(ctx, orgA)
	_ = store.CreateOrganization(ctx, orgB)

	_ = store.CreateUser(ctx, &User{OrganizationID: orgA.ID, Email: "a1@test", PasswordHash: "h", Role: RoleViewer})
	_ = store.CreateUser(ctx, &User{OrganizationID: orgA.ID, Email: "a2@test", PasswordHash: "h", Role: RoleAdmin})
	_ = store.CreateUser(ctx, &User{OrganizationID: orgB.ID, Email: "b1@test", PasswordHash: "h", Role: RoleViewer})

	usersA, err := store.ListUsersByOrganization(ctx, orgA.ID)
	if err != nil {
		t.Fatalf("ListUsersByOrganization failed: %v", err)
	}
	if len(usersA) != 2 {
		t.Errorf("expected 2 users in Org A, got %d", len(usersA))
	}
}

func TestSQLiteStore_DeleteUser(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	org := &Organization{Name: "Acme Corp"}
	_ = store.CreateOrganization(ctx, org)
	user := &User{OrganizationID: org.ID, Email: "todelete@acme.test", PasswordHash: "h", Role: RoleViewer}
	_ = store.CreateUser(ctx, user)

	if err := store.DeleteUser(ctx, user.ID); err != nil {
		t.Fatalf("DeleteUser failed: %v", err)
	}
	if _, err := store.GetUserByID(ctx, user.ID); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after deletion, got %v", err)
	}
}

func TestSQLiteStore_UpdateUserRole(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	org := &Organization{Name: "Acme Corp"}
	_ = store.CreateOrganization(ctx, org)
	user := &User{OrganizationID: org.ID, Email: "promote@acme.test", PasswordHash: "h", Role: RoleViewer}
	if err := store.CreateUser(ctx, user); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	if err := store.UpdateUserRole(ctx, user.ID, RoleAdmin); err != nil {
		t.Fatalf("UpdateUserRole failed: %v", err)
	}

	got, err := store.GetUserByID(ctx, user.ID)
	if err != nil {
		t.Fatalf("GetUserByID failed: %v", err)
	}
	if got.Role != RoleAdmin {
		t.Errorf("expected Role=admin after update, got %q", got.Role)
	}
	// Everything else about the user must be untouched — this is an
	// UPDATE of one column, not a delete-and-recreate (see the Store
	// interface doc comment on UpdateUserRole for why that distinction
	// matters: recreating would silently invalidate sessions and change
	// the user's ID).
	if got.ID != user.ID {
		t.Errorf("expected the user's ID to be unchanged, got %q (was %q)", got.ID, user.ID)
	}
	if got.Email != user.Email {
		t.Errorf("expected the user's email to be unchanged, got %q", got.Email)
	}
}

func TestSQLiteStore_UpdateUserRole_UnknownUser_ReturnsErrNotFound(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	err := store.UpdateUserRole(ctx, "does-not-exist", RoleAdmin)
	if err != ErrNotFound {
		t.Errorf("expected ErrNotFound for an unknown user id, got %v", err)
	}
}

func TestSQLiteStore_Session_CreateAndGet(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, tokenHash, err := GenerateSessionToken()
	if err != nil {
		t.Fatalf("GenerateSessionToken failed: %v", err)
	}

	session := &Session{UserID: "user_123", TokenHash: tokenHash, ExpiresAt: time.Now().UTC().Add(1 * time.Hour)}
	if err := store.CreateSession(ctx, session); err != nil {
		t.Fatalf("CreateSession failed: %v", err)
	}

	got, err := store.GetSessionByTokenHash(ctx, tokenHash)
	if err != nil {
		t.Fatalf("GetSessionByTokenHash failed: %v", err)
	}
	if got.UserID != "user_123" {
		t.Errorf("expected UserID='user_123', got %q", got.UserID)
	}
}

func TestSQLiteStore_DeleteSession(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, tokenHash, _ := GenerateSessionToken()
	session := &Session{UserID: "user_123", TokenHash: tokenHash, ExpiresAt: time.Now().UTC().Add(1 * time.Hour)}
	_ = store.CreateSession(ctx, session)

	if err := store.DeleteSession(ctx, session.ID); err != nil {
		t.Fatalf("DeleteSession failed: %v", err)
	}
	if _, err := store.GetSessionByTokenHash(ctx, tokenHash); err != ErrNotFound {
		t.Errorf("expected ErrNotFound after deletion, got %v", err)
	}
}

func TestSQLiteStore_DeleteExpiredSessions(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()

	_, expiredHash, _ := GenerateSessionToken()
	expired := &Session{UserID: "u1", TokenHash: expiredHash, ExpiresAt: time.Now().UTC().Add(-1 * time.Hour)}
	_ = store.CreateSession(ctx, expired)

	_, freshHash, _ := GenerateSessionToken()
	fresh := &Session{UserID: "u2", TokenHash: freshHash, ExpiresAt: time.Now().UTC().Add(1 * time.Hour)}
	_ = store.CreateSession(ctx, fresh)

	deleted, err := store.DeleteExpiredSessions(ctx)
	if err != nil {
		t.Fatalf("DeleteExpiredSessions failed: %v", err)
	}
	if deleted != 1 {
		t.Errorf("expected 1 expired session to be deleted, got %d", deleted)
	}

	if _, err := store.GetSessionByTokenHash(ctx, freshHash); err != nil {
		t.Errorf("expected the fresh session to survive, got: %v", err)
	}
}

// --- Service tests ---

func newTestService(t *testing.T) (*Service, *Organization) {
	t.Helper()
	store := newTestStore(t)
	ctx := context.Background()
	org := &Organization{Name: "Acme Corp"}
	if err := store.CreateOrganization(ctx, org); err != nil {
		t.Fatalf("CreateOrganization failed: %v", err)
	}
	return NewService(store), org
}

func TestService_Login_Success(t *testing.T) {
	svc, org := newTestService(t)
	ctx := context.Background()

	if _, err := svc.CreateUser(ctx, org.ID, "alice@acme.test", "hunter22", RoleOperator); err != nil {
		t.Fatalf("CreateUser failed: %v", err)
	}

	token, user, err := svc.Login(ctx, "alice@acme.test", "hunter22")
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	if token == "" {
		t.Error("expected a non-empty session token")
	}
	if user.Email != "alice@acme.test" {
		t.Errorf("unexpected user: %+v", user)
	}
}

func TestService_Login_WrongPassword(t *testing.T) {
	svc, org := newTestService(t)
	ctx := context.Background()
	_, _ = svc.CreateUser(ctx, org.ID, "alice@acme.test", "hunter22", RoleOperator)

	_, _, err := svc.Login(ctx, "alice@acme.test", "wrong-password")
	if err != ErrInvalidCredentials {
		t.Errorf("expected ErrInvalidCredentials, got %v", err)
	}
}

func TestService_Login_NonexistentEmail_SameErrorAsWrongPassword(t *testing.T) {
	svc, _ := newTestService(t)
	_, _, err := svc.Login(context.Background(), "nobody@acme.test", "whatever")
	if err != ErrInvalidCredentials {
		t.Errorf("expected ErrInvalidCredentials (not a distinguishing error), got %v", err)
	}
}

func TestService_Authenticate_ValidSession(t *testing.T) {
	svc, org := newTestService(t)
	ctx := context.Background()
	_, _ = svc.CreateUser(ctx, org.ID, "alice@acme.test", "hunter22", RoleOperator)
	token, _, err := svc.Login(ctx, "alice@acme.test", "hunter22")
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}

	user, err := svc.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate failed: %v", err)
	}
	if user.Email != "alice@acme.test" {
		t.Errorf("unexpected user: %+v", user)
	}
}

func TestService_Authenticate_ExpiredSession(t *testing.T) {
	svc, org := newTestService(t)
	svc.SessionDuration = -1 * time.Hour // force immediate expiry
	ctx := context.Background()
	_, _ = svc.CreateUser(ctx, org.ID, "alice@acme.test", "hunter22", RoleOperator)
	token, _, err := svc.Login(ctx, "alice@acme.test", "hunter22")
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}

	if _, err := svc.Authenticate(ctx, token); err == nil {
		t.Fatal("expected an already-expired session to fail authentication")
	}
}

func TestService_Authenticate_InvalidToken(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.Authenticate(context.Background(), "not-a-real-token"); err == nil {
		t.Fatal("expected an unrecognized token to fail authentication")
	}
}

func TestService_Logout_InvalidatesSession(t *testing.T) {
	svc, org := newTestService(t)
	ctx := context.Background()
	_, _ = svc.CreateUser(ctx, org.ID, "alice@acme.test", "hunter22", RoleOperator)
	token, _, err := svc.Login(ctx, "alice@acme.test", "hunter22")
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}

	if err := svc.Logout(ctx, token); err != nil {
		t.Fatalf("Logout failed: %v", err)
	}
	if _, err := svc.Authenticate(ctx, token); err == nil {
		t.Error("expected the session to be invalid after logout")
	}
}

func TestService_Logout_AlreadyLoggedOut_IsNotAnError(t *testing.T) {
	svc, _ := newTestService(t)
	if err := svc.Logout(context.Background(), "never-existed"); err != nil {
		t.Errorf("expected logging out a nonexistent token to be a no-op, got: %v", err)
	}
}
