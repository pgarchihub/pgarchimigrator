package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/api"
	"github.com/pgarchihub/pgarchimigrator/internal/auth"
	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
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

func newTestServer(t *testing.T, store *fakeStore, flow *fakeFlow) (*api.Server, *testUsers) {
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

	srv := api.NewServer(orch, store, nil, authService, false, nil, db.ConnectionInfo{}) // nil Reaper: sweep endpoint tested separately; nil pool: preview endpoint needs a real Postgres, tested in internal/preview instead

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
func doRequest(t *testing.T, srv *api.Server, method, path string, body any, cookie *http.Cookie) *httptest.ResponseRecorder {
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
func newSetupTestServer(t *testing.T) *api.Server {
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
	return api.NewServer(orch, store, nil, authService, false, nil, db.ConnectionInfo{})
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

// TestHandleDashboard_ServesHTML verifies the legacy vanilla-JS dashboard
// is still reachable at /legacy — kept as a short-term fallback during
// the cutover to the React SPA (see the "/" redirect test below), not
// deleted outright.
func TestHandleDashboard_ServesHTML(t *testing.T) {
	srv, _ := newTestServer(t, newFakeStore(), &fakeFlow{})
	rec := doRequest(t, srv, http.MethodGet, "/legacy", nil, nil) // public: no cookie needed

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/html") {
		t.Errorf("expected Content-Type text/html, got %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "pgArchiMigrator") {
		t.Error("expected the dashboard HTML to mention 'pgArchiMigrator'")
	}
}

// TestHandleRoot_RedirectsToWebapp is a regression test for the cutover:
// "/" must now send the user to the React SPA at /app, not serve the
// legacy dashboard HTML directly.
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
	srv := api.NewServer(orch, store, nil, authService, false, nil, connInfo)

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
// through SHADOW_TABLE — a combination internal/shadowflow has no logic
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
// internal/preview.Generate is ever called (that function needs a real
// PostgreSQL connection, which this pure-unit test suite doesn't have;
// see internal/preview's own integration tests for the substantive
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
func viewerUserID(t *testing.T, srv *api.Server, adminCookie *http.Cookie) string {
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
