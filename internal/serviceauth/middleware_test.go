package serviceauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func okHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestRequireBearer_NoHeader_Returns401(t *testing.T) {
	svc := NewService(newFakeStore())
	handler := RequireBearer(svc, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

func TestRequireBearer_InvalidToken_Returns401(t *testing.T) {
	svc := NewService(newFakeStore())
	handler := RequireBearer(svc, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer totally-made-up-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// TestRequireBearer_ValidToken_PassesThroughWithClientAndScopesInContext
// is the direct regression test for RequireBearer's own core promise —
// not just "the request is allowed through," but that the downstream
// handler can actually retrieve WHICH client and WHICH scopes via
// ClientFromContext/ScopesFromContext.
func TestRequireBearer_ValidToken_PassesThroughWithClientAndScopesInContext(t *testing.T) {
	store := newFakeStore()
	clientID, clientSecret := setupClient(t, store, []string{"pgarchimigrator.migrate"})
	svc := NewService(store)
	rawToken, _, _, _ := svc.IssueToken(context.Background(), clientID, clientSecret, nil)

	var gotClient *Client
	var gotScopes []string
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotClient = ClientFromContext(r.Context())
		gotScopes = ScopesFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	handler := RequireBearer(svc, inner)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if gotClient == nil || gotClient.ClientID != clientID {
		t.Errorf("expected the authenticated client in context, got %+v", gotClient)
	}
	if len(gotScopes) != 1 || gotScopes[0] != "pgarchimigrator.migrate" {
		t.Errorf("expected the granted scopes in context, got %v", gotScopes)
	}
}

func TestRequireScope_MissingScope_Returns403(t *testing.T) {
	store := newFakeStore()
	clientID, clientSecret := setupClient(t, store, []string{"pgarchimigrator.read"})
	svc := NewService(store)
	rawToken, _, _, _ := svc.IssueToken(context.Background(), clientID, clientSecret, nil)

	handler := RequireBearer(svc, RequireScope("pgarchimigrator.migrate", okHandler()))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a token missing the required scope, got %d", rec.Code)
	}
}

func TestRequireScope_HasScope_PassesThrough(t *testing.T) {
	store := newFakeStore()
	clientID, clientSecret := setupClient(t, store, []string{"pgarchimigrator.migrate"})
	svc := NewService(store)
	rawToken, _, _, _ := svc.IssueToken(context.Background(), clientID, clientSecret, nil)

	handler := RequireBearer(svc, RequireScope("pgarchimigrator.migrate", okHandler()))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected 200 for a token carrying the required scope, got %d", rec.Code)
	}
}

func TestBearerToken_ExtractsFromValidHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer abc123")
	if got := bearerToken(req); got != "abc123" {
		t.Errorf("expected 'abc123', got %q", got)
	}
}

// TestBearerToken_WrongScheme_ReturnsEmpty confirms a non-Bearer
// Authorization header (e.g. Basic auth, or a stray cookie-style value)
// is correctly rejected rather than partially parsed.
func TestBearerToken_WrongScheme_ReturnsEmpty(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	if got := bearerToken(req); got != "" {
		t.Errorf("expected an empty string for a non-Bearer scheme, got %q", got)
	}
}

func TestClientFromContext_NoAuth_ReturnsNil(t *testing.T) {
	if ClientFromContext(context.Background()) != nil {
		t.Error("expected nil for a context with no authenticated client")
	}
}

// TestRequireBearer_NilService_Returns503 is the direct regression test
// for RequireBearer's own nil-Service guard — a caller of an instance
// with service-to-service auth never configured must get a clear 503,
// not a panic.
func TestRequireBearer_NilService_Returns503(t *testing.T) {
	handler := RequireBearer(nil, okHandler())

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}
