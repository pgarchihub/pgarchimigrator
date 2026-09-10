package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/db"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/upgrade"
	"github.com/pgarchihub/pgarchimigrator/internal/auth"
	"github.com/pgarchihub/pgarchimigrator/internal/idempotency"
	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/serviceauth"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
)

// fakeStore is a minimal in-memory state.Store, duplicated here (rather
// than shared with internal/orchestrator's test-only fake) since Go test
// helpers in a `_test.go` file aren't importable across packages, and this
// project's established convention is to keep each package's tests
// self-contained rather than build a shared test-fakes package prematurely.
type fakeStore struct {
	mu   sync.Mutex
	jobs map[string]*state.Job
}

func newFakeStore() *fakeStore { return &fakeStore{jobs: map[string]*state.Job{}} }

func (f *fakeStore) Create(ctx context.Context, job *state.Job) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	cp := *job
	f.jobs[job.ID] = &cp
	return nil
}

func (f *fakeStore) UpdatePhase(ctx context.Context, jobID string, phase state.Phase) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.Phase = phase
	return nil
}

func (f *fakeStore) UpdatePhaseWithError(ctx context.Context, jobID string, phase state.Phase, lastError string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.Phase = phase
	job.LastError = lastError
	return nil
}

func (f *fakeStore) UpdateResources(ctx context.Context, jobID string, slotName, shadowTableName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.ReplicationSlotName = slotName
	job.ShadowTableName = shadowTableName
	return nil
}

func (f *fakeStore) UpdateRollbackDeadline(ctx context.Context, jobID string, deadline time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.RollbackDeadline = &deadline
	return nil
}

func (f *fakeStore) UpdateImpactPeak(ctx context.Context, jobID string, peakSeconds float64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return state.ErrJobNotFound
	}
	job.ImpactPeakQueryDurationSeconds = &peakSeconds
	return nil
}

func (f *fakeStore) Get(ctx context.Context, jobID string) (*state.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	job, ok := f.jobs[jobID]
	if !ok {
		return nil, state.ErrJobNotFound
	}
	cp := *job
	return &cp, nil
}

func (f *fakeStore) ListStale(ctx context.Context, olderThan time.Duration) ([]*state.Job, error) {
	return nil, nil
}

func (f *fakeStore) ListExpiredRollbackWindows(ctx context.Context) ([]*state.Job, error) {
	return nil, nil
}

func (f *fakeStore) UpdateDeprecatedColumnName(ctx context.Context, jobID string, deprecatedName string) error {
	return nil
}

func (f *fakeStore) UpdateIndexName(ctx context.Context, jobID string, indexName string) error {
	return nil
}

func (f *fakeStore) UpdateIndexDefinition(ctx context.Context, jobID string, definition string) error {
	return nil
}

func (f *fakeStore) UpdateConstraintName(ctx context.Context, jobID string, constraintName string) error {
	return nil
}

func (f *fakeStore) IncrementRowsProcessed(ctx context.Context, jobID string, delta int64) error {
	return nil
}

func (f *fakeStore) ListAll(ctx context.Context) ([]*state.Job, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var jobs []*state.Job
	for _, job := range f.jobs {
		cp := *job
		jobs = append(jobs, &cp)
	}
	return jobs, nil
}

// count is a test-only helper (not part of state.Store) — several tests
// assert exactly how many jobs got created without needing ListAll's
// full job data, just the count.
func (f *fakeStore) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.jobs)
}

// fakeFlow is a configurable orchestrator.Flow that never touches a real database.
type fakeFlow struct {
	executeErr  error
	rollbackErr error
}

func (f *fakeFlow) Execute(ctx context.Context, job *state.Job) error  { return f.executeErr }
func (f *fakeFlow) Rollback(ctx context.Context, job *state.Job) error { return f.rollbackErr }

// testUsers holds a ready-to-use session cookie for one user of each role,
// all in the same test organization — covers the common case of "does
// this route correctly require role X" without every test needing to
// create its own users.
type testUsers struct {
	org      *auth.Organization
	admin    *http.Cookie
	operator *http.Cookie
	viewer   *http.Cookie
}

func newTestServer(t *testing.T, store *fakeStore, flow *fakeFlow) (*Server, *testUsers) {
	t.Helper()

	orch := orchestrator.New(store,
		func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil },
		func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
			return strategy.TableStats{EstimatedRowCount: 100, HasPrimaryKey: true}, nil
		},
	)

	authStore, err := auth.NewSQLiteStore(filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("could not create auth store: %v", err)
	}
	t.Cleanup(func() { authStore.Close() })
	authService := auth.NewService(authStore)

	org := &auth.Organization{Name: "Test Org"}
	if err := authStore.CreateOrganization(context.Background(), org); err != nil {
		t.Fatalf("could not create test organization: %v", err)
	}

	srv := NewServer(orch, store, nil, authService, nil, nil, nil, nil, false, nil, db.ConnectionInfo{}) // nil Reaper: sweep endpoint tested separately; nil pool: preview endpoint needs a real Postgres, tested in engines/postgresql/preview instead

	users := &testUsers{
		org:      org,
		admin:    mustLogin(t, authService, org.ID, "admin@test.local", auth.RoleAdmin),
		operator: mustLogin(t, authService, org.ID, "operator@test.local", auth.RoleOperator),
		viewer:   mustLogin(t, authService, org.ID, "viewer@test.local", auth.RoleViewer),
	}
	return srv, users
}

// mustLogin creates a user with the given role and logs them in,
// returning the session cookie exactly as the real login handler would
// produce it (via auth.SetSessionCookie, not a hand-built cookie) so
// these tests exercise the real cookie-construction path too.
func mustLogin(t *testing.T, svc *auth.Service, orgID, email string, role auth.Role) *http.Cookie {
	t.Helper()
	ctx := context.Background()
	if _, err := svc.CreateUser(ctx, orgID, email, "test-password-123", role); err != nil {
		t.Fatalf("could not create test user %s: %v", email, err)
	}
	token, _, err := svc.Login(ctx, email, "test-password-123")
	if err != nil {
		t.Fatalf("could not log in test user %s: %v", email, err)
	}

	rec := httptest.NewRecorder()
	auth.SetSessionCookie(rec, token, time.Now().Add(time.Hour), false)
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("SetSessionCookie did not set a cookie")
	}
	return cookies[0]
}

// doRequest issues a request against srv. cookie may be nil for an
// unauthenticated request.
func doRequest(t *testing.T, srv *Server, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var reqBody *strings.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("could not marshal request body: %v", err)
		}
		reqBody = strings.NewReader(string(b))
	} else {
		reqBody = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, reqBody)
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// newSetupTestServer builds a Server backed by a genuinely fresh auth
// store — no organization, no users at all — unlike newTestServer (which
// bootstraps admin/operator/viewer accounts as a convenience for every
// other test in this file). This is deliberately separate: the setup
// flow's entire purpose only exists to be tested against a deployment
// that hasn't been bootstrapped yet.
func newSetupTestServer(t *testing.T) *Server {
	t.Helper()
	store := newFakeStore()
	orch := orchestrator.New(store,
		func(strategy.Strategy) (orchestrator.Flow, error) { return &fakeFlow{}, nil },
		func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
			return strategy.TableStats{EstimatedRowCount: 100, HasPrimaryKey: true}, nil
		},
	)
	authStore, err := auth.NewSQLiteStore(filepath.Join(t.TempDir(), "setup-test.db"))
	if err != nil {
		t.Fatalf("could not create auth store: %v", err)
	}
	t.Cleanup(func() { authStore.Close() })
	authService := auth.NewService(authStore)
	return NewServer(orch, store, nil, authService, nil, nil, nil, nil, false, nil, db.ConnectionInfo{})
}

func TestHandleSetupRequired_TrueOnFreshDeployment(t *testing.T) {
	srv := newSetupTestServer(t)

	rec := doRequest(t, srv, http.MethodGet, "/api/setup-required", nil, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid response: %v", err)
	}
	if !body["required"] {
		t.Error("expected required=true on a fresh deployment with no users")
	}
}

func TestHandleSetup_CreatesAdminAndLogsInImmediately(t *testing.T) {
	srv := newSetupTestServer(t)

	reqBody := map[string]string{"email": "founder@company.com", "password": "a-strong-password"}
	rec := doRequest(t, srv, http.MethodPost, "/api/setup", reqBody, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid response: %v", err)
	}
	if body["role"] != "admin" {
		t.Errorf("expected the first setup user to be role=admin, got %v", body["role"])
	}
	if body["email"] != "founder@company.com" {
		t.Errorf("expected email=founder@company.com, got %v", body["email"])
	}

	// The whole point of this endpoint is a smooth first-run experience —
	// prove the session cookie actually works, not just that one was set.
	cookies := rec.Result().Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected handleSetup to set a session cookie, immediately logging the new admin in")
	}
	meRec := doRequest(t, srv, http.MethodGet, "/api/auth/me", nil, cookies[0])
	if meRec.Code != http.StatusOK {
		t.Fatalf("the session cookie from handleSetup did not authenticate a follow-up request: %d %s", meRec.Code, meRec.Body.String())
	}
}

func TestHandleSetupRequired_FalseAfterSetupCompletes(t *testing.T) {
	srv := newSetupTestServer(t)

	doRequest(t, srv, http.MethodPost, "/api/setup", map[string]string{"email": "founder@company.com", "password": "a-strong-password"}, nil)

	rec := doRequest(t, srv, http.MethodGet, "/api/setup-required", nil, nil)
	var body map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid response: %v", err)
	}
	if body["required"] {
		t.Error("expected required=false once an admin has been created")
	}
}

// TestHandleSetup_RefusesOnceAlreadyBootstrapped is the critical security
// test for this endpoint: it must NEVER be usable to create a second,
// unauthenticated admin account once the deployment already has one —
// see handleSetup's doc comment on why this matters.
func TestHandleSetup_RefusesOnceAlreadyBootstrapped(t *testing.T) {
	srv := newSetupTestServer(t)
	firstBody := map[string]string{"email": "founder@company.com", "password": "a-strong-password"}
	doRequest(t, srv, http.MethodPost, "/api/setup", firstBody, nil)

	secondBody := map[string]string{"email": "attacker@evil.com", "password": "another-password"}
	rec := doRequest(t, srv, http.MethodPost, "/api/setup", secondBody, nil)
	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409 on a second setup attempt, got %d: %s", rec.Code, rec.Body.String())
	}

	// And prove the attacker's account was genuinely never created, not
	// just that this particular request was refused.
	loginRec := doRequest(t, srv, http.MethodPost, "/api/auth/login", secondBody, nil)
	if loginRec.Code != http.StatusUnauthorized {
		t.Fatalf("expected the second setup attempt's account to not exist at all, but login returned %d", loginRec.Code)
	}
}

func TestHandleSetup_RequiresEmailAndPassword(t *testing.T) {
	srv := newSetupTestServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/setup", map[string]string{"email": "", "password": ""}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSetup_RequiresPasswordAtLeast8Characters(t *testing.T) {
	srv := newSetupTestServer(t)

	rec := doRequest(t, srv, http.MethodPost, "/api/setup", map[string]string{"email": "founder@company.com", "password": "short"}, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for a too-short password, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleHealth(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/healthz", nil, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if body["status"] != "ok" {
		t.Errorf("expected status=ok, got %q", body["status"])
	}
}

// TestHandleLegacy_Returns404 is the direct regression test for the
// vanilla-JS dashboard's removal — it never supported anything past
// ADD_COLUMN/ALTER_COLUMN_TYPE (not even v1.0's later operations, let
// alone v2.0's), so it had already stopped being a genuine fallback by
// the time this route was removed. Confirms /legacy is genuinely gone,
// not silently still serving stale HTML.
func TestHandleLegacy_Returns404(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/legacy", nil, nil)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 now that /legacy has been removed, got %d", rec.Code)
	}
}

// TestHandleRoot_RedirectsToWebapp is a regression test for the cutover:
// "/" must send the user to the React SPA at /app.
func TestHandleRoot_RedirectsToWebapp(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/", nil, nil)

	if rec.Code != http.StatusFound {
		t.Fatalf("expected 302 Found, got %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/app/" {
		t.Errorf("expected redirect Location=/app/, got %q", loc)
	}
}

// TestHandleManifest_PubliclyAccessible_NoCookieNeeded is the direct
// regression test for handleManifest's own reasoning: an ecosystem
// consumer discovering this product for the first time has no trust
// relationship (and therefore no session cookie) yet — see AC-PF-003
// Section 8.2's registration flow, which this endpoint exists to
// support.
func TestHandleManifest_PubliclyAccessible_NoCookieNeeded(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/manifest", nil, nil) // no cookie

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "yaml") {
		t.Errorf("expected a YAML content type, got %q", ct)
	}
}

// TestHandleManifest_ContentMatchesProductID confirms the served bytes
// are genuinely this product's own manifest — not an empty embed, not
// a stale/mismatched file.
func TestHandleManifest_ContentMatchesProductID(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/manifest", nil, nil)

	body := rec.Body.String()
	if !strings.Contains(body, "productId: pgarchimigrator") {
		t.Errorf("expected the manifest to identify productId: pgarchimigrator, got: %s", body)
	}
	if !strings.Contains(body, "migration.postgresql.schema.migrate") {
		t.Errorf("expected the manifest to list its core capability, got: %s", body)
	}
}

// --- Auth boundary tests ---

// TestHandleGetConnectionInfo_ReturnsFieldsButNeverAPassword is the
// critical regression guard for db.ConnectionInfo being served directly
// over the REST API: even though the struct itself has no Password
// field (see its own doc comment), this test additionally proves the
// actual HTTP response body — the thing a browser really receives —
// contains no "password" key or the literal test password string,
// catching any FUTURE regression at the JSON-serialization boundary
// specifically, not just at the Go type level.
func TestHandleGetConnectionInfo_ReturnsFieldsButNeverAPassword(t *testing.T) {
	store := newFakeStore()
	orch := orchestrator.New(store,
		func(strategy.Strategy) (orchestrator.Flow, error) { return &fakeFlow{}, nil },
		func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
			return strategy.TableStats{EstimatedRowCount: 100, HasPrimaryKey: true}, nil
		},
	)
	authStore, err := auth.NewSQLiteStore(filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("could not create auth store: %v", err)
	}
	t.Cleanup(func() { authStore.Close() })
	authService := auth.NewService(authStore)
	org := &auth.Organization{Name: "Test Org"}
	if err := authStore.CreateOrganization(context.Background(), org); err != nil {
		t.Fatalf("could not create test organization: %v", err)
	}
	viewerCookie := mustLogin(t, authService, org.ID, "viewer@test.local", auth.RoleViewer)

	connInfo, err := db.ParseConnectionInfo("postgresql://pgarchimigrator:supersecret-test-password@dbhost.internal:5432/pgarchimigrator_prod?sslmode=disable")
	if err != nil {
		t.Fatalf("ParseConnectionInfo failed: %v", err)
	}
	srv := NewServer(orch, store, nil, authService, nil, nil, nil, nil, false, nil, connInfo)

	rec := doRequest(t, srv, http.MethodGet, "/api/connection", nil, viewerCookie)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	body := rec.Body.String()
	if strings.Contains(body, "supersecret-test-password") {
		t.Fatal("CRITICAL: the response body contains the literal database password")
	}
	if strings.Contains(strings.ToLower(body), "password") {
		t.Fatal("CRITICAL: the response body mentions \"password\" at all")
	}

	var got struct {
		Host     string
		Port     int
		Username string
		Database string
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}
	if got.Host != "dbhost.internal" || got.Port != 5432 || got.Username != "pgarchimigrator" || got.Database != "pgarchimigrator_prod" {
		t.Errorf("unexpected connection info: %+v", got)
	}
}

func TestProtectedRoute_NoCookie_Returns401(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations", nil, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleStrategyMatrix_ReturnsRealOperationStrategyPairs is a direct
// regression test for a real incident (see internal/strategy's
// validStrategiesByOperation doc comment): the New Migration screen's
// strategy override dropdown used to show every strategy regardless of
// the selected operation, which let ADD_INDEX get silently forced
// through SHADOW_TABLE — a combination engines/postgresql/shadowflow has no logic
// for at all, which silently did nothing useful. This confirms the
// endpoint the frontend now filters that dropdown against actually
// reflects the same whitelist StartMigration itself enforces (same
// underlying function, strategy.ValidStrategyMatrix, so they cannot
// drift apart).
func TestHandleStrategyMatrix_ReturnsRealOperationStrategyPairs(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/strategy-matrix", nil, users.viewer)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var matrix map[string][]string
	if err := json.Unmarshal(rec.Body.Bytes(), &matrix); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}

	addIndexStrategies := matrix["ADD_INDEX"]
	if len(addIndexStrategies) != 1 || addIndexStrategies[0] != "DIRECT_DDL" {
		t.Errorf("expected ADD_INDEX to allow only DIRECT_DDL, got %v", addIndexStrategies)
	}
	for _, s := range addIndexStrategies {
		if s == "SHADOW_TABLE" {
			t.Error("SHADOW_TABLE must never appear as a valid strategy for ADD_INDEX — this is the exact incident this endpoint exists to prevent")
		}
	}

	alterTypeStrategies := matrix["ALTER_COLUMN_TYPE"]
	foundShadowTable := false
	for _, s := range alterTypeStrategies {
		if s == "SHADOW_TABLE" {
			foundShadowTable = true
		}
	}
	if !foundShadowTable {
		t.Errorf("expected ALTER_COLUMN_TYPE to allow SHADOW_TABLE (its actual use case), got %v", alterTypeStrategies)
	}
}

// TestHandleTableStats_ReturnsEstimatedRowCount verifies the New
// Migration screen's row-count endpoint reuses the exact same
// TableStatsFetcher StartMigration itself calls (see newTestServer's own
// fake, which returns EstimatedRowCount: 100) — so this number can never
// disagree with what the migration's own strategy decision was actually
// based on.
func TestHandleTableStats_ReturnsEstimatedRowCount(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/schemas/public/tables/orders/stats", nil, users.viewer)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var got strategy.TableStats
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("could not decode response: %v", err)
	}
	if got.EstimatedRowCount != 100 {
		t.Errorf("expected EstimatedRowCount=100 (matching newTestServer's fake TableStatsFetcher), got %d", got.EstimatedRowCount)
	}
}

func TestProtectedRoute_InvalidCookie_Returns401(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	badCookie := &http.Cookie{Name: "pgarchimigrator_session", Value: "not-a-real-token"}
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations", nil, badCookie)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleStartMigration_ViewerRole_Returns403(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	reqBody := map[string]any{"table": "orders", "column": "status", "operation": "ADD_COLUMN"}
	rec := doRequest(t, srv, http.MethodPost, "/api/migrations", reqBody, users.viewer)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (viewer cannot start migrations), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleSweep_OperatorRole_Returns403(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodPost, "/api/sweep", nil, users.operator)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (operator cannot run sweep, admin-only), got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- Migration endpoint tests (operating at each route's minimum required role) ---

func TestHandleStartMigration_MissingFields_Returns400(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodPost, "/api/migrations", map[string]string{"schema": "public"}, users.operator)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandlePreviewMigration_MissingFields_Returns400 verifies request
// validation on the dry-run endpoint — deliberately only the validation
// path, which is safe to test here since it runs BEFORE
// engines/postgresql/preview.Generate is ever called (that function needs a real
// PostgreSQL connection, which this pure-unit test suite doesn't have;
// see engines/postgresql/preview's own integration tests for the substantive
// dry-run behavior — NULL-count warnings, statement previews, etc.).
func TestHandlePreviewMigration_MissingFields_Returns400(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodPost, "/api/migrations/preview", map[string]string{"schema": "public"}, users.viewer)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleStartMigration_Success(t *testing.T) {
	store := newFakeStore()
	srv, users := newTestServer(t, store, &fakeFlow{})

	reqBody := map[string]any{
		"table": "orders", "column": "status", "operation": "ADD_COLUMN",
		"type": "TEXT", "default": "'active'",
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/migrations", reqBody, users.operator)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var report map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &report); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if report["JobID"] == nil || report["JobID"] == "" {
		t.Error("expected a non-empty JobID in the response")
	}
	if report["Strategy"] != "DIRECT_DDL" {
		t.Errorf("expected strategy DIRECT_DDL for a small table, got %v", report["Strategy"])
	}
}

func TestHandleStartMigration_FlowFails_Returns422(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{executeErr: context.DeadlineExceeded})

	reqBody := map[string]any{"table": "orders", "column": "status", "operation": "ADD_COLUMN"}
	rec := doRequest(t, srv, http.MethodPost, "/api/migrations", reqBody, users.operator)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("expected 422, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleListMigrations(t *testing.T) {
	store := newFakeStore()
	_ = store.Create(context.Background(), &state.Job{ID: "job-1", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted})
	_ = store.Create(context.Background(), &state.Job{ID: "job-2", Strategy: "SHADOW_TABLE", Phase: state.PhaseSyncing})
	srv, users := newTestServer(t, store, &fakeFlow{})

	// Viewer is the minimum role for this route — using it here doubles
	// as confirmation that Viewer really can read, not just that a higher
	// role can.
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations", nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var reports []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &reports); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if len(reports) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(reports))
	}
}

func TestHandleGetMigration_Found(t *testing.T) {
	store := newFakeStore()
	_ = store.Create(context.Background(), &state.Job{ID: "job-1", Strategy: "DIRECT_DDL", Phase: state.PhaseCompleted})
	srv, users := newTestServer(t, store, &fakeFlow{})

	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/job-1", nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleGetMigration_NotFound(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/migrations/nonexistent", nil, users.viewer)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleRollback_Success(t *testing.T) {
	store := newFakeStore()
	_ = store.Create(context.Background(), &state.Job{ID: "job-1", Strategy: "DIRECT_DDL", Phase: state.PhaseFailed})
	srv, users := newTestServer(t, store, &fakeFlow{})

	rec := doRequest(t, srv, http.MethodPost, "/api/migrations/job-1/rollback", nil, users.operator)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleRollback_NotFound(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodPost, "/api/migrations/nonexistent/rollback", nil, users.operator)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestHandleSweep_NoReaperConfigured_Returns503(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{}) // built with nil Reaper
	rec := doRequest(t, srv, http.MethodPost, "/api/sweep", nil, users.admin)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- Auth endpoint tests ---

func TestHandleLogin_Success(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{}) // newTestServer's side effect already creates admin@test.local

	reqBody := map[string]string{"email": "admin@test.local", "password": "test-password-123"}
	rec := doRequest(t, srv, http.MethodPost, "/api/auth/login", reqBody, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(rec.Result().Cookies()) == 0 {
		t.Error("expected a session cookie to be set on successful login")
	}
}

func TestHandleLogin_WrongPassword_Returns401(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})

	reqBody := map[string]string{"email": "admin@test.local", "password": "wrong-password"}
	rec := doRequest(t, srv, http.MethodPost, "/api/auth/login", reqBody, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleLogin_RateLimited_Returns429 is the API-level regression
// guard for the loginRateLimiter added specifically because this
// endpoint has no other brute-force protection — every OTHER endpoint
// already requires a valid session, which naturally limits abuse in a
// way a fresh login attempt never does.
func TestHandleLogin_RateLimited_Returns429(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	reqBody := map[string]string{"email": "admin@test.local", "password": "wrong-password"}

	// 10 matches internal/api's own unexported loginRateLimit constant —
	// duplicated here as a literal (not a reference) because this file is
	// `package api_test` (external/black-box tests, using only api's
	// exported surface), which cannot see an unexported constant from
	// `package api`. See ratelimit_test.go (package api, white-box) for
	// tests against the limiter's internals directly.
	const limit = 10
	for i := 0; i < limit; i++ {
		rec := doRequest(t, srv, http.MethodPost, "/api/auth/login", reqBody, nil)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d: expected 401 (still under the limit), got %d: %s", i+1, rec.Code, rec.Body.String())
		}
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/auth/login", reqBody, nil)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("expected 429 after exceeding the limit, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleMe_Authenticated(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/auth/me", nil, users.admin)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if body["email"] != "admin@test.local" {
		t.Errorf("expected email=admin@test.local, got %v", body["email"])
	}
}

func TestHandleMe_Unauthenticated_Returns401(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/auth/me", nil, nil)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
}

func TestHandleLogout_ClearsSession(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	rec := doRequest(t, srv, http.MethodPost, "/api/auth/logout", nil, users.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// The SAME cookie must no longer work after logout.
	rec2 := doRequest(t, srv, http.MethodGet, "/api/auth/me", nil, users.admin)
	if rec2.Code != http.StatusUnauthorized {
		t.Errorf("expected the logged-out session to be rejected, got %d", rec2.Code)
	}
}

// --- User management tests (admin-only) ---

func TestHandleCreateUser_AsAdmin_Succeeds(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	reqBody := map[string]string{"email": "newbie@test.local", "password": "another-password", "role": "viewer"}
	rec := doRequest(t, srv, http.MethodPost, "/api/users", reqBody, users.admin)

	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateUser_AsOperator_Returns403(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	reqBody := map[string]string{"email": "newbie2@test.local", "password": "another-password", "role": "viewer"}
	rec := doRequest(t, srv, http.MethodPost, "/api/users", reqBody, users.operator)

	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (operator cannot manage users), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateUser_DuplicateEmail_Returns409(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	reqBody := map[string]string{"email": "admin@test.local" /* already exists */, "password": "whatever1", "role": "viewer"}
	rec := doRequest(t, srv, http.MethodPost, "/api/users", reqBody, users.admin)

	if rec.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleCreateUser_InvalidRole_Returns400(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	reqBody := map[string]string{"email": "newbie3@test.local", "password": "another-password", "role": "superuser"}
	rec := doRequest(t, srv, http.MethodPost, "/api/users", reqBody, users.admin)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleListUsers_AsAdmin(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/api/users", nil, users.admin)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if len(list) != 3 { // admin, operator, viewer created by newTestServer
		t.Errorf("expected 3 users in the test org, got %d", len(list))
	}
}

func TestHandleDeleteUser_CannotDeleteSelf(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	// Look up the admin's own ID via /api/auth/me first.
	meRec := doRequest(t, srv, http.MethodGet, "/api/auth/me", nil, users.admin)
	var me map[string]any
	if err := json.Unmarshal(meRec.Body.Bytes(), &me); err != nil {
		t.Fatalf("invalid /api/auth/me response: %v", err)
	}
	selfID, _ := me["id"].(string)

	rec := doRequest(t, srv, http.MethodDelete, "/api/users/"+selfID, nil, users.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (cannot delete own account), got %d: %s", rec.Code, rec.Body.String())
	}
}

// viewerUserID looks up the seed viewer user's ID via /api/users (as an
// admin) — a stable target for the role-update tests below, distinct
// from whichever admin cookie is making the request.
func viewerUserID(t *testing.T, srv *Server, adminCookie *http.Cookie) string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodGet, "/api/users", nil, adminCookie)
	var list []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("invalid /api/users response: %v", err)
	}
	for _, u := range list {
		if u["role"] == "viewer" {
			id, _ := u["id"].(string)
			return id
		}
	}
	t.Fatal("could not find the seed viewer user")
	return ""
}

func TestHandleUpdateUserRole_AsAdmin_Succeeds(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	targetID := viewerUserID(t, srv, users.admin)

	rec := doRequest(t, srv, http.MethodPatch, "/api/users/"+targetID+"/role", map[string]string{"role": "operator"}, users.admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify the change actually took, not just that the handler
	// returned 200 — a real regression scenario if UpdateUserRole's SQL
	// silently affected zero rows without erroring.
	listRec := doRequest(t, srv, http.MethodGet, "/api/users", nil, users.admin)
	var list []map[string]any
	if err := json.Unmarshal(listRec.Body.Bytes(), &list); err != nil {
		t.Fatalf("invalid /api/users response: %v", err)
	}
	found := false
	for _, u := range list {
		if u["id"] == targetID {
			found = true
			if u["role"] != "operator" {
				t.Errorf("expected role=operator after update, got %v", u["role"])
			}
		}
	}
	if !found {
		t.Error("target user disappeared after the role update")
	}
}

func TestHandleUpdateUserRole_AsOperator_Returns403(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	targetID := viewerUserID(t, srv, users.admin)

	rec := doRequest(t, srv, http.MethodPatch, "/api/users/"+targetID+"/role", map[string]string{"role": "operator"}, users.operator)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("expected 403 (operator cannot manage users), got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUpdateUserRole_InvalidRole_Returns400(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	targetID := viewerUserID(t, srv, users.admin)

	rec := doRequest(t, srv, http.MethodPatch, "/api/users/"+targetID+"/role", map[string]string{"role": "superuser"}, users.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleUpdateUserRole_UnknownUser_Returns404(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	rec := doRequest(t, srv, http.MethodPatch, "/api/users/does-not-exist/role", map[string]string{"role": "admin"}, users.admin)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandleUpdateUserRole_CannotChangeOwnRole mirrors
// TestHandleDeleteUser_CannotDeleteSelf's exact reasoning: an accidental
// self-demotion could lock the only admin out of user management with no
// recovery path.
func TestHandleUpdateUserRole_CannotChangeOwnRole(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	meRec := doRequest(t, srv, http.MethodGet, "/api/auth/me", nil, users.admin)
	var me map[string]any
	if err := json.Unmarshal(meRec.Body.Bytes(), &me); err != nil {
		t.Fatalf("invalid /api/auth/me response: %v", err)
	}
	selfID, _ := me["id"].(string)

	rec := doRequest(t, srv, http.MethodPatch, "/api/users/"+selfID+"/role", map[string]string{"role": "viewer"}, users.admin)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 (cannot change own role), got %d: %s", rec.Code, rec.Body.String())
	}
}

// --- OAuth2 token endpoint (POST /oauth/token) tests ---

// doOAuthRequest sends a form-urlencoded request — deliberately NOT
// doRequest above, which always sets Content-Type: application/json;
// RFC 6749 Section 4.4.2 mandates application/x-www-form-urlencoded for
// the client_credentials grant, and handleOAuthToken parses the request
// with r.ParseForm accordingly.
func doOAuthRequest(t *testing.T, srv *Server, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

// newTestServerWithServiceAuth mirrors newTestServer, but ALSO wires up
// a real serviceauth.Service (newTestServer's own ServiceAuth stays nil
// — see its own NewServer call — since most tests have no need for it).
// Returns the registered test client's raw credentials alongside the
// server, for tests to exchange at /oauth/token.
func newTestServerWithServiceAuth(t *testing.T, store *fakeStore, flow *fakeFlow, scopes []string) (srv *Server, clientID, clientSecret string) {
	t.Helper()

	orch := orchestrator.New(store,
		func(strategy.Strategy) (orchestrator.Flow, error) { return flow, nil },
		func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
			return strategy.TableStats{EstimatedRowCount: 100, HasPrimaryKey: true}, nil
		},
	)

	authStore, err := auth.NewSQLiteStore(filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("could not create auth store: %v", err)
	}
	t.Cleanup(func() { authStore.Close() })
	authService := auth.NewService(authStore)

	svcAuthStore, err := serviceauth.NewSQLiteStore(filepath.Join(t.TempDir(), "serviceauth-test.db"))
	if err != nil {
		t.Fatalf("could not create service-auth store: %v", err)
	}
	t.Cleanup(func() { svcAuthStore.Close() })
	svcAuthService := serviceauth.NewService(svcAuthStore)

	rawSecret, secretHash, err := serviceauth.GenerateClientSecret()
	if err != nil {
		t.Fatalf("could not generate client secret: %v", err)
	}
	client := &serviceauth.Client{Name: "test-client", ClientID: "test-client-id", ClientSecretHash: secretHash, Scopes: scopes}
	if err := svcAuthStore.CreateClient(context.Background(), client); err != nil {
		t.Fatalf("could not create test client: %v", err)
	}

	srv = NewServer(orch, store, nil, authService, svcAuthService, nil, nil, nil, false, nil, db.ConnectionInfo{})
	return srv, client.ClientID, rawSecret
}

// newTestServerWithServiceAuthAndUpgrade combines
// newTestServerWithServiceAuth's own ServiceAuth setup with
// newTestServerWithUpgrade's own real upgrade.SQLiteStore — needed for
// handleEcosystemStartUpgrade's own tests, which (unlike
// handleEcosystemStartMigration's) require BOTH wired up at once.
func newTestServerWithServiceAuthAndUpgrade(t *testing.T, scopes []string) (srv *Server, clientID, clientSecret string) {
	t.Helper()

	orch := orchestrator.New(newFakeStore(),
		func(strategy.Strategy) (orchestrator.Flow, error) { return &fakeFlow{}, nil },
		func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
			return strategy.TableStats{EstimatedRowCount: 100, HasPrimaryKey: true}, nil
		},
	)

	authStore, err := auth.NewSQLiteStore(filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("could not create auth store: %v", err)
	}
	t.Cleanup(func() { authStore.Close() })
	authService := auth.NewService(authStore)

	svcAuthStore, err := serviceauth.NewSQLiteStore(filepath.Join(t.TempDir(), "serviceauth-test.db"))
	if err != nil {
		t.Fatalf("could not create service-auth store: %v", err)
	}
	t.Cleanup(func() { svcAuthStore.Close() })
	svcAuthService := serviceauth.NewService(svcAuthStore)

	rawSecret, secretHash, err := serviceauth.GenerateClientSecret()
	if err != nil {
		t.Fatalf("could not generate client secret: %v", err)
	}
	client := &serviceauth.Client{Name: "test-client", ClientID: "test-client-id", ClientSecretHash: secretHash, Scopes: scopes}
	if err := svcAuthStore.CreateClient(context.Background(), client); err != nil {
		t.Fatalf("could not create test client: %v", err)
	}

	upgradeStore, err := upgrade.NewSQLiteStore(filepath.Join(t.TempDir(), "upgrade-test.db"))
	if err != nil {
		t.Fatalf("could not create upgrade store: %v", err)
	}
	t.Cleanup(func() { upgradeStore.Close() })

	idempotencyStore, err := idempotency.NewSQLiteStore(filepath.Join(t.TempDir(), "idempotency-test.db"))
	if err != nil {
		t.Fatalf("could not create idempotency store: %v", err)
	}
	t.Cleanup(func() { idempotencyStore.Close() })

	srv = NewServer(orch, newFakeStore(), nil, authService, svcAuthService, upgradeStore, upgrade.StaticConnectionProvider{}, idempotencyStore, false, nil, db.ConnectionInfo{})
	return srv, client.ClientID, rawSecret
}

func TestHandleOAuthToken_ServiceAuthNotConfigured_Returns503(t *testing.T) {
	// Uses plain newTestServer (nil ServiceAuth) — the direct regression
	// test for handleOAuthToken's own "optional dependency wasn't wired
	// up" precedent, matching handleSweep's identical nil-Reaper
	// behavior.
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {"x"}, "client_secret": {"y"}}
	rec := doOAuthRequest(t, srv, form)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestHandleOAuthToken_ValidCredentials_ReturnsAccessToken(t *testing.T) {
	srv, clientID, clientSecret := newTestServerWithServiceAuth(t, newFakeStore(), &fakeFlow{}, []string{"pgarchimigrator.read"})
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	rec := doOAuthRequest(t, srv, form)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if body["access_token"] == "" || body["access_token"] == nil {
		t.Error("expected a non-empty access_token")
	}
	if body["token_type"] != "Bearer" {
		t.Errorf("expected token_type=Bearer, got %v", body["token_type"])
	}
	if body["scope"] != "pgarchimigrator.read" {
		t.Errorf("expected scope='pgarchimigrator.read', got %v", body["scope"])
	}
}

// TestHandleOAuthToken_WrongSecret_ReturnsInvalidClient is the direct
// regression test for handleOAuthToken correctly mapping
// serviceauth.ErrInvalidClient to RFC 6749's own "invalid_client" error
// code and 401 status — not a generic 500.
func TestHandleOAuthToken_WrongSecret_ReturnsInvalidClient(t *testing.T) {
	srv, clientID, _ := newTestServerWithServiceAuth(t, newFakeStore(), &fakeFlow{}, []string{"pgarchimigrator.read"})
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {"wrong-secret"}}
	rec := doOAuthRequest(t, srv, form)

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_client" {
		t.Errorf("expected error=invalid_client, got %v", body["error"])
	}
}

func TestHandleOAuthToken_ScopeExceedsAllowed_ReturnsInvalidScope(t *testing.T) {
	srv, clientID, clientSecret := newTestServerWithServiceAuth(t, newFakeStore(), &fakeFlow{}, []string{"pgarchimigrator.read"})
	form := url.Values{
		"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret},
		"scope": {"pgarchimigrator.read pgarchimigrator.migrate"},
	}
	rec := doOAuthRequest(t, srv, form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "invalid_scope" {
		t.Errorf("expected error=invalid_scope, got %v", body["error"])
	}
}

func TestHandleOAuthToken_UnsupportedGrantType_Rejected(t *testing.T) {
	srv, clientID, clientSecret := newTestServerWithServiceAuth(t, newFakeStore(), &fakeFlow{}, []string{"pgarchimigrator.read"})
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	rec := doOAuthRequest(t, srv, form)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
	var body map[string]string
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["error"] != "unsupported_grant_type" {
		t.Errorf("expected error=unsupported_grant_type, got %v", body["error"])
	}
}

// TestHandleOAuthToken_IssuedToken_ActuallyWorksOnAProtectedRoute is an
// end-to-end regression test spanning handleOAuthToken AND
// serviceauth.RequireBearer together — confirms a token obtained from
// THIS endpoint is genuinely usable, not just well-formed. Uses the
// manifest endpoint (already public) only as a convenient always-200
// target; the actual behavior under test is independent of which route
// is called.
func TestHandleOAuthToken_IssuedToken_ActuallyWorksOnAProtectedRoute(t *testing.T) {
	srv, clientID, clientSecret := newTestServerWithServiceAuth(t, newFakeStore(), &fakeFlow{}, []string{"pgarchimigrator.read"})
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	tokenRec := doOAuthRequest(t, srv, form)

	var body map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &body)
	rawToken, _ := body["access_token"].(string)
	if rawToken == "" {
		t.Fatal("expected a usable access token from the token endpoint")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/manifest", nil)
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("expected the issued token to work, got %d", rec.Code)
	}
}

// --- Ecosystem migration endpoint (POST /api/v1/migrations) tests ---

func TestHandleEcosystemStartMigration_NoScope_Returns403(t *testing.T) {
	srv, clientID, clientSecret := newTestServerWithServiceAuth(t, newFakeStore(), &fakeFlow{}, []string{"pgarchimigrator.read"}) // read only, not migrate
	tokenForm := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	tokenRec := doOAuthRequest(t, srv, tokenForm)
	var tokenBody map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &tokenBody)
	rawToken, _ := tokenBody["access_token"].(string)

	body, _ := json.Marshal(map[string]string{"table": "orders", "operation": "ADD_COLUMN", "column": "total", "type": "numeric"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migrations", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a token without the migrate scope, got %d", rec.Code)
	}
}

// TestHandleEcosystemStartMigration_ValidRequest_Returns202WithLocation
// is the direct regression test for AC-PF-003 Section 13.2's own
// "202 Accepted + operation reference" pattern — the entire reason this
// endpoint is shaped differently from handleStartMigration.
func TestHandleEcosystemStartMigration_ValidRequest_Returns202WithLocation(t *testing.T) {
	store := newFakeStore()
	srv, clientID, clientSecret := newTestServerWithServiceAuth(t, store, &fakeFlow{}, []string{"pgarchimigrator.migrate"})
	tokenForm := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	tokenRec := doOAuthRequest(t, srv, tokenForm)
	var tokenBody map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &tokenBody)
	rawToken, _ := tokenBody["access_token"].(string)

	reqBody, _ := json.Marshal(map[string]string{"table": "orders", "operation": "ADD_COLUMN", "column": "total", "type": "numeric"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migrations", strings.NewReader(string(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Error("expected a Location header pointing at the new job")
	}
	var body operationAcceptedResponseForTest
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if body.Status != "accepted" {
		t.Errorf("expected status='accepted', got %q", body.Status)
	}
	if body.OperationID == "" {
		t.Error("expected a non-empty operationId")
	}
	if store.count() != 1 {
		t.Errorf("expected exactly 1 job to have been created, got %d", store.count())
	}
}

// operationAcceptedResponseForTest mirrors api's own unexported
// operationAcceptedResponse — duplicated here (this is an external
// _test package, api_test, with no access to api's unexported types)
// purely to decode the response body's known shape.
type operationAcceptedResponseForTest struct {
	OperationID string `json:"operationId"`
	Status      string `json:"status"`
	StatusURL   string `json:"statusUrl"`
}

// TestHandleEcosystemStartMigration_PropagatesCorrelationHeaders is the
// direct regression test for AC-PF-003 Section 12.3's own header
// contract actually reaching the created job — not just being read and
// discarded. Confirms X-Archi-Command-Id specifically lands on
// CausationID (not a same-named CausationID header, which doesn't
// exist in the spec — see docs/ecosystem/ARCHITECTURE.md's own note on
// this).
func TestHandleEcosystemStartMigration_PropagatesCorrelationHeaders(t *testing.T) {
	store := newFakeStore()
	srv, clientID, clientSecret := newTestServerWithServiceAuth(t, store, &fakeFlow{}, []string{"pgarchimigrator.migrate"})
	tokenForm := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	tokenRec := doOAuthRequest(t, srv, tokenForm)
	var tokenBody map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &tokenBody)
	rawToken, _ := tokenBody["access_token"].(string)

	reqBody, _ := json.Marshal(map[string]string{"table": "orders", "operation": "ADD_COLUMN", "column": "total", "type": "numeric"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migrations", strings.NewReader(string(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)
	req.Header.Set("X-Correlation-ID", "cor-abc123")
	req.Header.Set("X-Archi-Lifecycle-Id", "lc-xyz789")
	req.Header.Set("X-Archi-Command-Id", "cmd-def456")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	var body operationAcceptedResponseForTest
	_ = json.Unmarshal(rec.Body.Bytes(), &body)

	job, err := store.Get(context.Background(), body.OperationID)
	if err != nil {
		t.Fatalf("could not read back the created job: %v", err)
	}
	if job.CorrelationID != "cor-abc123" {
		t.Errorf("expected CorrelationID='cor-abc123', got %q", job.CorrelationID)
	}
	if job.LifecycleID != "lc-xyz789" {
		t.Errorf("expected LifecycleID='lc-xyz789', got %q", job.LifecycleID)
	}
	if job.CausationID != "cmd-def456" {
		t.Errorf("expected CausationID='cmd-def456' (from X-Archi-Command-Id), got %q", job.CausationID)
	}
}

func TestHandleEcosystemStartMigration_ServiceAuthNotConfigured_Returns503(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{}) // nil ServiceAuth
	reqBody, _ := json.Marshal(map[string]string{"table": "orders", "operation": "ADD_COLUMN", "column": "total", "type": "numeric"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/migrations", strings.NewReader(string(reqBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer irrelevant")
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

// --- Upgrade endpoint (/api/upgrades) tests ---

func TestHandleStartUpgrade_NotConfigured_Returns503(t *testing.T) {
	// newTestServer's own UpgradeStore stays nil — see its own doc
	// comment for why most tests have no need for it, matching
	// ServiceAuth's identical default.
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	body := map[string]any{"sourceDsn": "postgresql://x", "targetDsn": "postgresql://y"}
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades", body, users.admin)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestHandleStartUpgrade_RequiresAdminRole(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	body := map[string]any{"sourceDsn": "postgresql://x", "targetDsn": "postgresql://y"}
	// operator is below the RoleAdmin minimum this route requires — see
	// server.go's own routes() comment on why upgrades need a stricter
	// role than migrations do.
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades", body, users.operator)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for an operator (below RoleAdmin), got %d", rec.Code)
	}
}

// newTestServerWithUpgrade mirrors newTestServerWithServiceAuth's own
// pattern — a real upgrade.SQLiteStore (this package has no non-SQLite
// Store implementation, matching internal/auth/internal/serviceauth's
// own precedent), wired into a real *Server.
func newTestServerWithUpgrade(t *testing.T) (*Server, *testUsers) {
	t.Helper()

	orch := orchestrator.New(newFakeStore(),
		func(strategy.Strategy) (orchestrator.Flow, error) { return &fakeFlow{}, nil },
		func(ctx context.Context, schema, table string) (strategy.TableStats, error) {
			return strategy.TableStats{EstimatedRowCount: 100, HasPrimaryKey: true}, nil
		},
	)

	authStore, err := auth.NewSQLiteStore(filepath.Join(t.TempDir(), "auth-test.db"))
	if err != nil {
		t.Fatalf("could not create auth store: %v", err)
	}
	t.Cleanup(func() { authStore.Close() })
	authService := auth.NewService(authStore)

	org := &auth.Organization{Name: "Test Org"}
	if err := authStore.CreateOrganization(context.Background(), org); err != nil {
		t.Fatalf("could not create test organization: %v", err)
	}

	upgradeStore, err := upgrade.NewSQLiteStore(filepath.Join(t.TempDir(), "upgrade-test.db"))
	if err != nil {
		t.Fatalf("could not create upgrade store: %v", err)
	}
	t.Cleanup(func() { upgradeStore.Close() })

	srv := NewServer(orch, newFakeStore(), nil, authService, nil, upgradeStore, upgrade.StaticConnectionProvider{}, nil, false, nil, db.ConnectionInfo{})
	users := &testUsers{
		org:      org,
		admin:    mustLogin(t, authService, org.ID, "admin@test.local", auth.RoleAdmin),
		operator: mustLogin(t, authService, org.ID, "operator@test.local", auth.RoleOperator),
		viewer:   mustLogin(t, authService, org.ID, "viewer@test.local", auth.RoleViewer),
	}
	return srv, users
}

// TestHandleStartUpgrade_ValidRequest_Returns202WithJobID confirms the
// handler creates a real, durable job and responds with AC-PF-003
// Section 13.2's own "202 Accepted + operation reference" shape —
// matching handleEcosystemStartMigration's identical pattern, applied
// here to the cookie-authenticated dashboard surface rather than the
// OAuth2 one. Does NOT wait for (or assert anything about) the
// background Flow.Run itself succeeding — that needs a real PostgreSQL
// pair and is covered by engines/postgresql/upgrade's own
// flow_integration_test.go; this test is purely about the HTTP
// contract: did a job get created and durably persisted, and did the
// response look right.
func TestHandleStartUpgrade_ValidRequest_Returns202WithJobID(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)
	body := map[string]any{
		"sourceDsn": "postgresql://nonexistent-host-for-this-test/db",
		"targetDsn": "postgresql://nonexistent-host-for-this-test/db",
		"schemas":   []string{"public"},
	}
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades", body, users.admin)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Error("expected a Location header")
	}
	var respBody map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if respBody["id"] == "" {
		t.Error("expected a non-empty job id")
	}
	if respBody["status"] != "accepted" {
		t.Errorf("expected status='accepted', got %q", respBody["status"])
	}
}

func TestHandleStartUpgrade_MissingDSNs_Returns400(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades", map[string]any{}, users.admin)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing sourceDsn/targetDsn, got %d", rec.Code)
	}
}

// TestHandleGetUpgrade_ReturnsJobAndTables is the direct regression
// test for handleGetUpgrade combining a Job with its own per-table
// progress in one response — the dashboard's own single round trip for
// a job's detail page.
func TestHandleGetUpgrade_ReturnsJobAndTables(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)
	startBody := map[string]any{"sourceDsn": "postgresql://x/db", "targetDsn": "postgresql://y/db"}
	startRec := doRequest(t, srv, http.MethodPost, "/api/upgrades", startBody, users.admin)
	var startResp map[string]string
	_ = json.Unmarshal(startRec.Body.Bytes(), &startResp)
	jobID := startResp["id"]

	rec := doRequest(t, srv, http.MethodGet, "/api/upgrades/"+jobID, nil, users.operator)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var detail map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if detail["ID"] != jobID {
		t.Errorf("expected the job's own ID in the response, got %v", detail["ID"])
	}
	if _, ok := detail["tables"]; !ok {
		t.Error("expected a 'tables' field in the response, even if empty")
	}
}

func TestHandleGetUpgrade_UnknownID_Returns404(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)
	rec := doRequest(t, srv, http.MethodGet, "/api/upgrades/nonexistent-job-id", nil, users.operator)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleListUpgrades_ReturnsCreatedJobs(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)
	body := map[string]any{"sourceDsn": "postgresql://x/db", "targetDsn": "postgresql://y/db"}
	doRequest(t, srv, http.MethodPost, "/api/upgrades", body, users.admin)
	doRequest(t, srv, http.MethodPost, "/api/upgrades", body, users.admin)

	rec := doRequest(t, srv, http.MethodGet, "/api/upgrades", nil, users.viewer)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var jobs []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &jobs); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if len(jobs) != 2 {
		t.Errorf("expected 2 upgrade jobs, got %d", len(jobs))
	}
}

// --- Introspection endpoint (POST /api/upgrades/introspect-source) tests ---

func TestHandleIntrospectUpgradeSource_MissingSourceDsn_Returns400(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades/introspect-source", map[string]any{}, users.admin)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", rec.Code)
	}
}

// TestHandleIntrospectUpgradeSource_UnreachableSource_Returns502 is the
// one real, end-to-end-verifiable behavior of this handler available in
// this sandbox (no real reachable PostgreSQL instance to introspect
// successfully against here) — confirms an unreachable/invalid source
// fails with a clear 502, not a hang or a 500.
func TestHandleIntrospectUpgradeSource_UnreachableSource_Returns502(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	body := map[string]any{"sourceDsn": "postgresql://user:pass@nonexistent-host-for-this-test:5432/db"}
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades/introspect-source", body, users.admin)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("expected 502 for an unreachable source, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestHandleIntrospectUpgradeSource_RequiresAdminRole(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	body := map[string]any{"sourceDsn": "postgresql://x/db"}
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades/introspect-source", body, users.operator)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for an operator (below RoleAdmin), got %d", rec.Code)
	}
}

// TestHandleStartUpgrade_TablesField_CarriesOverToTheJob is the direct
// regression test for Priority 3's own table-level scoping actually
// reaching the created job — not just being accepted and silently
// dropped.
func TestHandleStartUpgrade_TablesField_CarriesOverToTheJob(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)
	body := map[string]any{
		"sourceDsn": "postgresql://x/db",
		"targetDsn": "postgresql://y/db",
		"tables": []map[string]string{
			{"schema": "public", "table": "orders"},
			{"schema": "reporting", "table": "events"},
		},
	}
	startRec := doRequest(t, srv, http.MethodPost, "/api/upgrades", body, users.admin)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", startRec.Code, startRec.Body.String())
	}
	var startResp map[string]string
	_ = json.Unmarshal(startRec.Body.Bytes(), &startResp)

	detailRec := doRequest(t, srv, http.MethodGet, "/api/upgrades/"+startResp["id"], nil, users.admin)
	var detail map[string]any
	_ = json.Unmarshal(detailRec.Body.Bytes(), &detail)

	tables, ok := detail["Tables"].([]any)
	if !ok || len(tables) != 2 {
		t.Fatalf("expected 2 tables to carry over onto the job, got: %v", detail["Tables"])
	}
}

// --- Ecosystem upgrade endpoint (POST /api/v1/upgrades) tests ---

func TestHandleEcosystemStartUpgrade_NotConfigured_Returns503(t *testing.T) {
	// newTestServerWithServiceAuth's own UpgradeStore stays nil.
	srv, clientID, clientSecret := newTestServerWithServiceAuth(t, newFakeStore(), &fakeFlow{}, []string{"pgarchimigrator.upgrade"})
	tokenForm := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	tokenRec := doOAuthRequest(t, srv, tokenForm)
	var tokenBody map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &tokenBody)
	rawToken, _ := tokenBody["access_token"].(string)

	body, _ := json.Marshal(map[string]string{"sourceDsn": "postgresql://x", "targetDsn": "postgresql://y"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upgrades", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

func TestHandleEcosystemStartUpgrade_WrongScope_Returns403(t *testing.T) {
	srv, clientID, clientSecret := newTestServerWithServiceAuthAndUpgrade(t, []string{"pgarchimigrator.migrate"}) // migrate, not upgrade
	tokenForm := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	tokenRec := doOAuthRequest(t, srv, tokenForm)
	var tokenBody map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &tokenBody)
	rawToken, _ := tokenBody["access_token"].(string)

	body, _ := json.Marshal(map[string]string{"sourceDsn": "postgresql://x", "targetDsn": "postgresql://y"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upgrades", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("expected 403 for a token without the upgrade scope, got %d", rec.Code)
	}
}

// TestHandleEcosystemStartUpgrade_ValidRequest_Returns202WithJobID
// mirrors TestHandleEcosystemStartMigration_ValidRequest_Returns202WithLocation's
// own pattern, applied to the upgrade endpoint.
func TestHandleEcosystemStartUpgrade_ValidRequest_Returns202WithJobID(t *testing.T) {
	srv, clientID, clientSecret := newTestServerWithServiceAuthAndUpgrade(t, []string{"pgarchimigrator.upgrade"})
	tokenForm := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	tokenRec := doOAuthRequest(t, srv, tokenForm)
	var tokenBody map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &tokenBody)
	rawToken, _ := tokenBody["access_token"].(string)

	body, _ := json.Marshal(map[string]string{"sourceDsn": "postgresql://nonexistent/db", "targetDsn": "postgresql://nonexistent/db"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/upgrades", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+rawToken)
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc == "" {
		t.Error("expected a Location header")
	}
	var respBody operationAcceptedResponseForTest
	if err := json.Unmarshal(rec.Body.Bytes(), &respBody); err != nil {
		t.Fatalf("invalid JSON response: %v", err)
	}
	if respBody.OperationID == "" {
		t.Error("expected a non-empty operationId")
	}
	if respBody.Status != "accepted" {
		t.Errorf("expected status='accepted', got %q", respBody.Status)
	}
}

// TestHandleEcosystemStartUpgrade_IdempotencyKey_SecondRequestDoesNotCreateANewJob
// is the direct end-to-end regression test for idempotency.Middleware
// actually being wired into this specific route correctly — a retry
// with the same Idempotency-Key must not start a second, duplicate
// upgrade job.
func TestHandleEcosystemStartUpgrade_IdempotencyKey_SecondRequestDoesNotCreateANewJob(t *testing.T) {
	srv, clientID, clientSecret := newTestServerWithServiceAuthAndUpgrade(t, []string{"pgarchimigrator.upgrade"})
	tokenForm := url.Values{"grant_type": {"client_credentials"}, "client_id": {clientID}, "client_secret": {clientSecret}}
	tokenRec := doOAuthRequest(t, srv, tokenForm)
	var tokenBody map[string]any
	_ = json.Unmarshal(tokenRec.Body.Bytes(), &tokenBody)
	rawToken, _ := tokenBody["access_token"].(string)

	body, _ := json.Marshal(map[string]string{"sourceDsn": "postgresql://nonexistent/db", "targetDsn": "postgresql://nonexistent/db"})

	makeRequest := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/upgrades", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+rawToken)
		req.Header.Set("Idempotency-Key", "retry-test-key-1")
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, req)
		return rec
	}

	rec1 := makeRequest()
	rec2 := makeRequest()

	if rec1.Code != http.StatusAccepted || rec2.Code != http.StatusAccepted {
		t.Fatalf("expected both requests to return 202, got %d and %d", rec1.Code, rec2.Code)
	}

	var resp1, resp2 operationAcceptedResponseForTest
	_ = json.Unmarshal(rec1.Body.Bytes(), &resp1)
	_ = json.Unmarshal(rec2.Body.Bytes(), &resp2)

	if resp1.OperationID != resp2.OperationID {
		t.Errorf("expected the SAME job id from both requests (no duplicate job created), got %q and %q", resp1.OperationID, resp2.OperationID)
	}
	if rec2.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("expected the second response to be marked as replayed")
	}
}

// --- Retry endpoint tests ---

// TestHandleRetryMigration_CreatesNewJobWithSameParameters is the
// direct regression test for buildMigrationRequestFromJob actually
// round-tripping a job's own operation parameters correctly — starts a
// real migration, retries it, and confirms the NEW job has the exact
// same schema/table/operation/column, not just "some job got created."
func TestHandleRetryMigration_CreatesNewJobWithSameParameters(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})

	startBody := map[string]any{"table": "orders", "operation": "ADD_COLUMN", "column": "total", "type": "numeric"}
	startRec := doRequest(t, srv, http.MethodPost, "/api/migrations", startBody, users.operator)
	if startRec.Code != http.StatusOK {
		t.Fatalf("expected the initial migration to start with 200, got %d: %s", startRec.Code, startRec.Body.String())
	}
	var original map[string]any
	_ = json.Unmarshal(startRec.Body.Bytes(), &original)
	originalID, _ := original["JobID"].(string)
	if originalID == "" {
		t.Fatalf("expected a JobID in the start response, got: %s", startRec.Body.String())
	}

	retryRec := doRequest(t, srv, http.MethodPost, "/api/migrations/"+originalID+"/retry", nil, users.operator)
	if retryRec.Code != http.StatusOK {
		t.Fatalf("expected the retry to return 200, got %d: %s", retryRec.Code, retryRec.Body.String())
	}
	var retried map[string]any
	_ = json.Unmarshal(retryRec.Body.Bytes(), &retried)
	retriedID, _ := retried["JobID"].(string)

	if retriedID == "" || retriedID == originalID {
		t.Errorf("expected a NEW, distinct job id from retry, got %q (original was %q)", retriedID, originalID)
	}
	if retried["TableName"] != "orders" {
		t.Errorf("expected TableName='orders' to carry over, got %v", retried["TableName"])
	}
	if retried["Operation"] != "ADD_COLUMN" {
		t.Errorf("expected Operation='ADD_COLUMN' to carry over, got %v", retried["Operation"])
	}
	// progress.Report (what this endpoint actually returns) has no raw
	// ColumnName field by design — describeOperation folds it into a
	// human-readable OperationSummary instead (see that function's own
	// ADD_COLUMN case: fmt.Sprintf("Added column %q ...", job.ColumnName,
	// ...)), so asserting the parameter carried over means checking it
	// shows up there, not on a field this response was never meant to have.
	summary, _ := retried["OperationSummary"].(string)
	if !strings.Contains(summary, `"total"`) {
		t.Errorf("expected the retried job's OperationSummary to mention column %q, got %q", "total", summary)
	}
}

func TestHandleRetryMigration_UnknownID_Returns404(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodPost, "/api/migrations/nonexistent-job-id/retry", nil, users.operator)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

// TestHandleRetryUpgrade_CreatesNewJobWithSameConnectionInfo is the
// direct regression test for handleRetryUpgrade's own central promise:
// a retry needs ZERO re-entry of connection info (including passwords),
// because it's read entirely server-side from the original job.
func TestHandleRetryUpgrade_CreatesNewJobWithSameConnectionInfo(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)

	startBody := map[string]any{
		"sourceDsn": "postgresql://user:secret@nonexistent-host/db",
		"targetDsn": "postgresql://user:secret@nonexistent-host2/db",
		"schemas":   []string{"public", "billing"},
	}
	startRec := doRequest(t, srv, http.MethodPost, "/api/upgrades", startBody, users.admin)
	if startRec.Code != http.StatusAccepted {
		t.Fatalf("expected the initial upgrade to start with 202, got %d: %s", startRec.Code, startRec.Body.String())
	}
	var original map[string]string
	_ = json.Unmarshal(startRec.Body.Bytes(), &original)
	originalID := original["id"]

	retryRec := doRequest(t, srv, http.MethodPost, "/api/upgrades/"+originalID+"/retry", nil, users.admin)
	if retryRec.Code != http.StatusAccepted {
		t.Fatalf("expected the retry to return 202, got %d: %s", retryRec.Code, retryRec.Body.String())
	}
	var retried map[string]string
	_ = json.Unmarshal(retryRec.Body.Bytes(), &retried)
	if retried["id"] == "" || retried["id"] == originalID {
		t.Errorf("expected a NEW, distinct job id from retry, got %q (original was %q)", retried["id"], originalID)
	}

	// The real proof: fetch the new job and confirm its own connection
	// info matches the original — read server-side, never resubmitted
	// by the client.
	detailRec := doRequest(t, srv, http.MethodGet, "/api/upgrades/"+retried["id"], nil, users.admin)
	var detail map[string]any
	_ = json.Unmarshal(detailRec.Body.Bytes(), &detail)
	if detail["SourceConnectionRef"] != "postgresql://user:secret@nonexistent-host/db" {
		t.Errorf("expected SourceConnectionRef to carry over from the original job, got %v", detail["SourceConnectionRef"])
	}
}

func TestHandleRetryUpgrade_UnknownID_Returns404(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades/nonexistent-job-id/retry", nil, users.admin)

	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404, got %d", rec.Code)
	}
}

func TestHandleRetryUpgrade_NotConfigured_Returns503(t *testing.T) {
	srv, users := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodPost, "/api/upgrades/some-id/retry", nil, users.admin)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got %d", rec.Code)
	}
}

// --- rebuildReplicationRef: verified end to end against real DSN
// strings in a standalone script before being ported here (this
// package's own pgx dependency means it can't be exercised with real
// go test in this sandbox — see docs/TESTING.md's own note on this
// constraint) — these are the exact same cases, now living alongside
// the function itself for CI.

func TestRebuildReplicationRef_HostAndPort(t *testing.T) {
	got, err := rebuildReplicationRef("postgresql://pgarchimigrator:pgarchimigrator_dev_only@localhost:55432/pgarchimigrator_test?sslmode=disable", "pg-logical", "5432")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "postgresql://pgarchimigrator:pgarchimigrator_dev_only@pg-logical:5432/pgarchimigrator_test?sslmode=disable"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// TestRebuildReplicationRef_HostOnly_PreservesOriginalPort is the
// direct regression test for a real gap found while verifying this
// function: a host-only override (port left blank) must keep the
// ORIGINAL non-default port, not silently fall back to PostgreSQL's
// own default (5432) — losing a genuinely non-default port like 55432
// would itself break the very retry this feature exists to fix.
func TestRebuildReplicationRef_HostOnly_PreservesOriginalPort(t *testing.T) {
	got, err := rebuildReplicationRef("postgresql://user:pass@localhost:55432/db", "pg-logical", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "postgresql://user:pass@pg-logical:55432/db"
	if got != want {
		t.Errorf("got %q, want %q (the original :55432 must survive a host-only override)", got, want)
	}
}

func TestRebuildReplicationRef_PortOnly(t *testing.T) {
	got, err := rebuildReplicationRef("postgresql://user:pass@localhost:55432/db", "", "5433")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "postgresql://user:pass@localhost:5433/db"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRebuildReplicationRef_PreservesSpecialCharactersInPassword(t *testing.T) {
	got, err := rebuildReplicationRef("postgresql://user:p%40ss@localhost:55432/db", "pg-logical", "5432")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "postgresql://user:p%40ss@pg-logical:5432/db"
	if got != want {
		t.Errorf("got %q, want %q — the password must survive byte-for-byte, unread and unmodified", got, want)
	}
}

// TestHandleRetryUpgrade_WithReplicationOverride_RebuildsSourceReplicationRef
// is the direct regression test for the real, repeated bug report this
// override exists to fix: a retry whose ORIGINAL job never had a
// working SourceReplicationRef predictably failed the same way every
// time, with no way to correct it short of starting an entirely new
// database migration. A caller can now supply just a host/port in the
// retry request body and get a NEW job whose SourceReplicationRef is
// rebuilt from it — the password is never part of the request at all.
func TestHandleRetryUpgrade_WithReplicationOverride_RebuildsSourceReplicationRef(t *testing.T) {
	srv, users := newTestServerWithUpgrade(t)

	startBody := map[string]any{
		"sourceDsn": "postgresql://user:secret@localhost:55432/db",
		"targetDsn": "postgresql://user:secret@localhost:55434/db",
	}
	startRec := doRequest(t, srv, http.MethodPost, "/api/upgrades", startBody, users.admin)
	var original map[string]string
	_ = json.Unmarshal(startRec.Body.Bytes(), &original)

	retryBody := map[string]any{"replicationHost": "pg-logical"}
	retryRec := doRequest(t, srv, http.MethodPost, "/api/upgrades/"+original["id"]+"/retry", retryBody, users.admin)
	if retryRec.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", retryRec.Code, retryRec.Body.String())
	}
	var retried map[string]string
	_ = json.Unmarshal(retryRec.Body.Bytes(), &retried)

	detailRec := doRequest(t, srv, http.MethodGet, "/api/upgrades/"+retried["id"], nil, users.admin)
	var detail map[string]any
	_ = json.Unmarshal(detailRec.Body.Bytes(), &detail)

	got, _ := detail["SourceReplicationRef"].(string)
	want := "postgresql://user:secret@pg-logical:55432/db"
	if got != want {
		t.Errorf("expected SourceReplicationRef %q, got %q", want, got)
	}
}
