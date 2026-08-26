// Package api implements the REST API and dashboard described in
// Architecture Doc Section 5 ("CLI (Cobra) + REST API", "Basit Web UI
// (Dashboard)"). It is a thin HTTP wrapper around internal/orchestrator,
// internal/state, and internal/reaper — no business logic lives here.
//
// Built on Go 1.22's stdlib http.ServeMux method+path routing
// (e.g. "POST /api/migrations") rather than a third-party router: this
// project has repeatedly hit friction resolving new Go module
// dependencies in constrained network environments (see pglogrepl's
// go.sum history), so avoiding an unnecessary new dependency here is a
// deliberate choice, not an oversight.
package api

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/internal/auth"
	"github.com/pgarchihub/pgarchimigrator/internal/catalog"
	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/orchestrator"
	"github.com/pgarchihub/pgarchimigrator/internal/preview"
	"github.com/pgarchihub/pgarchimigrator/internal/progress"
	"github.com/pgarchihub/pgarchimigrator/internal/reaper"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
	"github.com/pgarchihub/pgarchimigrator/internal/strategy"
	"github.com/pgarchihub/pgarchimigrator/internal/typecompat"
	"github.com/pgarchihub/pgarchimigrator/internal/version"
)

//go:embed dashboard.html
var dashboardFS embed.FS

// webappFS embeds the built React SPA (see /web in the repo root —
// `npm run build` there, output copied into ./webapp). Served at /app,
// and "/" redirects there — see the routes() registration above for the
// cutover reasoning. The legacy dashboard.html is kept reachable at
// /legacy in the meantime.
//
//go:embed webapp
var webappFS embed.FS

// spaFileSystem wraps an http.FileSystem so any path that doesn't resolve
// to a real embedded file falls back to index.html — required for
// client-side routing (react-router's BrowserRouter) to work on a direct
// load or refresh of a route like /app/migrations/abc123, which has no
// corresponding file in the embedded build output.
type spaFileSystem struct {
	http.FileSystem
}

func (f spaFileSystem) Open(name string) (http.File, error) {
	file, err := f.FileSystem.Open(name)
	if err != nil {
		return f.FileSystem.Open("index.html")
	}
	return file, nil
}

// Server is the REST API + dashboard HTTP handler.
type Server struct {
	Orchestrator *orchestrator.Orchestrator
	Store        state.Store
	Reaper       *reaper.Reaper // optional; nil disables POST /api/sweep (returns 503)
	AuthService  *auth.Service
	// Pool is used directly (not just through Orchestrator/Store) by
	// handlePreviewMigration, since internal/preview needs read-only
	// access to run its dry-run sanity-check queries (e.g. counting
	// existing NULLs before a SET_NOT_NULL) — see internal/preview's
	// package doc comment for why dry-run still touches the database.
	Pool *pgxpool.Pool
	// secureCookies controls the session cookie's Secure flag — true
	// whenever the server is reached over HTTPS (i.e. essentially always
	// in production, behind a reverse proxy/ingress doing TLS
	// termination). Defaults to false via NewServer's cookieSecure
	// parameter so local http://localhost testing (as used throughout
	// this project's development) keeps working without extra setup.
	secureCookies bool
	mux           *http.ServeMux
	// ConnectionInfo is the non-sensitive (no password) subset of the
	// server's PGARCHIMIGRATOR_DATABASE_URL — served via GET /api/connection so
	// the New Migration screen can show which database it's pointed at.
	// Zero-value (empty strings) is fine and simply renders as "unknown"
	// on the frontend; this is display-only and never used for anything
	// that requires it to be present.
	ConnectionInfo db.ConnectionInfo
	loginLimiter   *loginRateLimiter
	// lagTracker remembers the most recently observed replication lag
	// per job, purely in memory — see its own type doc comment for why
	// this exists and why losing it on a restart is fine.
	lagTracker *lagTrendTracker
	// impactTracker remembers the highest observed query-impact reading
	// per job — see its own type doc comment.
	impactTracker *impactTracker
}

// loginRateLimit/loginRateLimitWindow are the defaults every Server is
// built with — generous enough that a legitimate user mistyping their
// password a few times in a row never gets blocked, tight enough to
// meaningfully slow a brute-force attempt (10 guesses per 5 minutes per
// source IP is ~2,880/day, not "free" the way an unlimited endpoint is).
const (
	loginRateLimit       = 10
	loginRateLimitWindow = 5 * time.Minute
)

// NewServer builds a Server with all routes registered.
func NewServer(
	orch *orchestrator.Orchestrator,
	store state.Store,
	r *reaper.Reaper,
	authService *auth.Service,
	secureCookies bool,
	pool *pgxpool.Pool,
	connInfo db.ConnectionInfo,
) *Server {
	s := &Server{
		Orchestrator: orch, Store: store, Reaper: r, AuthService: authService,
		secureCookies: secureCookies, Pool: pool, ConnectionInfo: connInfo,
		loginLimiter:  newLoginRateLimiter(loginRateLimit, loginRateLimitWindow),
		lagTracker:    newLagTrendTracker(),
		impactTracker: newImpactTracker(),
	}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

// ServeHTTP makes Server itself an http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// protect wraps a handler behind auth.RequireAuth + auth.RequireRole,
// converting the resulting http.Handler back into the plain
// func(w, r) shape http.ServeMux.HandleFunc expects — a small local
// helper purely to avoid repeating that conversion at every route.
func (s *Server) protect(minRole auth.Role, h http.HandlerFunc) http.HandlerFunc {
	return auth.RequireAuth(s.AuthService, auth.RequireRole(minRole, h)).ServeHTTP
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	// Public — the whole point is letting the login/setup screens show
	// which build is actually running without needing to be signed in
	// first (useful for support requests: "what version are you on").
	s.mux.HandleFunc("GET /api/version", s.handleVersion)

	// Auth endpoints are intentionally NOT wrapped in s.protect — they're
	// how a session is established (or torn down) in the first place.
	s.mux.HandleFunc("POST /api/auth/login", s.handleLogin)
	// Public — no session exists yet on a fresh deployment, and by
	// definition can't (see handleSetup's doc comment for why this is
	// still safe as a public, unauthenticated endpoint).
	s.mux.HandleFunc("GET /api/setup-required", s.handleSetupRequired)
	s.mux.HandleFunc("POST /api/setup", s.handleSetup)
	s.mux.HandleFunc("POST /api/auth/logout", s.handleLogout)
	s.mux.HandleFunc("GET /api/auth/me", s.protect(auth.RoleViewer, s.handleMe))

	s.mux.HandleFunc("POST /api/migrations", s.protect(auth.RoleOperator, s.handleStartMigration))
	s.mux.HandleFunc("POST /api/migrations/preview", s.protect(auth.RoleViewer, s.handlePreviewMigration))
	// Read-only catalog browsing for the New Migration screen's
	// schema/table/column dropdowns — see internal/catalog's package doc
	// comment. RoleViewer is enough: nothing here mutates anything or
	// reads actual row data, only pg_catalog/information_schema metadata.
	s.mux.HandleFunc("GET /api/schemas", s.protect(auth.RoleViewer, s.handleListSchemas))
	s.mux.HandleFunc("GET /api/schemas/{schema}/tables", s.protect(auth.RoleViewer, s.handleListTables))
	s.mux.HandleFunc("GET /api/schemas/{schema}/tables/{table}/columns", s.protect(auth.RoleViewer, s.handleListColumns))
	s.mux.HandleFunc("GET /api/schemas/{schema}/tables/{table}/sample", s.protect(auth.RoleViewer, s.handleSampleRows))
	// Estimated row count for the New Migration screen's table overview
	// panel — reuses the exact same TableStats function StartMigration
	// itself calls (pg_class.reltuples under the hood, see
	// strategy.TableStats's own doc comment on why this is an estimate,
	// not an exact count), so this can never show a number that
	// disagrees with what the migration's own strategy decision was
	// actually based on.
	s.mux.HandleFunc("GET /api/schemas/{schema}/tables/{table}/stats", s.protect(auth.RoleViewer, s.handleTableStats))
	// Non-sensitive connection info (host/port/username/database, never
	// the password) for the New Migration screen's read-only "connected
	// to" banner — see db.ConnectionInfo's doc comment.
	s.mux.HandleFunc("GET /api/connection", s.protect(auth.RoleViewer, s.handleGetConnectionInfo))
	// Static, compile-time-known domain knowledge (which strategies each
	// operation actually supports) — see strategy.ValidStrategyMatrix's
	// doc comment for why the New Migration screen fetches this rather
	// than hardcoding its own copy that could drift out of sync with
	// what StartMigration actually enforces.
	s.mux.HandleFunc("GET /api/strategy-matrix", s.protect(auth.RoleViewer, s.handleStrategyMatrix))
	// Deliberately its own endpoint, not folded into /api/migrations/preview
	// — this blocks for writeLoadSampleDuration (10s), only called when
	// the New Migration screen's opt-in "check current write load" step
	// is explicitly triggered. See handleEstimateWriteLoad's own doc
	// comment.
	s.mux.HandleFunc("POST /api/migrations/estimate-write-load", s.protect(auth.RoleViewer, s.handleEstimateWriteLoad))
	s.mux.HandleFunc("GET /api/migrations", s.protect(auth.RoleViewer, s.handleListMigrations))
	// Historical, aggregate view across every job the store knows about
	// — see progress.ComputeAnalytics's own doc comment.
	s.mux.HandleFunc("GET /api/analytics", s.protect(auth.RoleViewer, s.handleGetAnalytics))
	s.mux.HandleFunc("GET /api/migrations/{id}", s.protect(auth.RoleViewer, s.handleGetMigration))
	s.mux.HandleFunc("POST /api/migrations/{id}/rollback", s.protect(auth.RoleOperator, s.handleRollback))
	s.mux.HandleFunc("POST /api/sweep", s.protect(auth.RoleAdmin, s.handleSweep))

	s.mux.HandleFunc("GET /api/users", s.protect(auth.RoleAdmin, s.handleListUsers))
	s.mux.HandleFunc("POST /api/users", s.protect(auth.RoleAdmin, s.handleCreateUser))
	s.mux.HandleFunc("DELETE /api/users/{id}", s.protect(auth.RoleAdmin, s.handleDeleteUser))
	s.mux.HandleFunc("PATCH /api/users/{id}/role", s.protect(auth.RoleAdmin, s.handleUpdateUserRole))

	// "/" redirects to the React SPA at /app — the cutover point, once all
	// four core screens (Dashboard, New Migration, Migration Detail,
	// Users) were built and confirmed working end-to-end against a real
	// server. The legacy vanilla-JS dashboard is kept reachable at
	// /legacy, not deleted, as a short-term fallback during this
	// transition — nothing here prevents reverting this one redirect line
	// if something unexpected turns up.
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/app/", http.StatusFound)
	})
	s.mux.HandleFunc("GET /legacy", s.handleDashboard) // public: the page itself gates content client-side via /api/auth/me
	s.registerWebapp()                                 // GET /app/... — the React SPA; see webappFS's doc comment
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"version": version.Version})
}

// --- Auth handlers ---

type loginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	// Checked before even decoding the body — an attacker hammering this
	// endpoint shouldn't get a JSON-parse error to distinguish "malformed
	// request" from "rate limited" timing-wise, and there's no reason to
	// do the (cheap, but non-zero) decode work at all once the limit's
	// been hit.
	if !s.loginLimiter.allow(clientIP(r)) {
		writeError(w, http.StatusTooManyRequests, fmt.Errorf("too many login attempts — please wait a few minutes and try again"))
		return
	}

	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	token, user, err := s.AuthService.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		// auth.ErrInvalidCredentials is deliberately the same error for
		// "no such user" and "wrong password" — see its doc comment.
		// Anything else (a genuine store failure) still surfaces as 401
		// here rather than 500, so a login attempt never leaks whether
		// the failure was "bad credentials" vs. "our database is down".
		writeError(w, http.StatusUnauthorized, err)
		return
	}

	// == 0 (not <= 0), matching auth.Service's own internal duration()
	// check — a negative SessionDuration is a deliberate, valid way to
	// force already-expired sessions (used in tests), and must not be
	// silently overridden here.
	dur := s.AuthService.SessionDuration
	if dur == 0 {
		dur = auth.DefaultSessionDuration
	}
	auth.SetSessionCookie(w, token, time.Now().UTC().Add(dur), s.secureCookies)
	writeJSON(w, http.StatusOK, map[string]any{"id": user.ID, "email": user.Email, "role": user.Role})
}

// setupIsRequired is the single source of truth handleSetupRequired and
// handleSetup both use: "no users exist yet in the default organization".
// EnsureDefaultOrganization is idempotent (see its own doc comment) —
// safe to call on every check, not just the first.
func (s *Server) setupIsRequired(ctx context.Context) (bool, error) {
	org, err := auth.EnsureDefaultOrganization(ctx, s.AuthService.Store, "Default Organization")
	if err != nil {
		return false, fmt.Errorf("failed to check setup status: %w", err)
	}
	users, err := s.AuthService.Store.ListUsersByOrganization(ctx, org.ID)
	if err != nil {
		return false, fmt.Errorf("failed to check setup status: %w", err)
	}
	return len(users) == 0, nil
}

// handleSetupRequired reports whether this deployment has any user yet —
// the frontend uses this to decide whether to show the first-run "Create
// your admin account" wizard instead of the ordinary login screen.
func (s *Server) handleSetupRequired(w http.ResponseWriter, r *http.Request) {
	required, err := s.setupIsRequired(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"required": required})
}

type setupRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

// handleSetup creates the first admin user for a fresh deployment —
// public (no auth required to call it), but STRICTLY one-time: it
// re-checks setupIsRequired right before creating the user and refuses
// (409) if anyone already exists, so this can never become a standing
// "create an admin without logging in" backdoor — it's only ever
// reachable in the narrow window between a fresh deploy and the first
// account being created.
//
// The re-check here is best-effort, not a hard transactional lock — two
// truly concurrent setup attempts could theoretically both pass the
// check before either INSERT completes, creating two admins instead of
// blocking the second. An accepted trade-off: this deployment model is
// already single-instance/single-operator (see TR-13), and the realistic
// exposure window is a few minutes right after `docker run`, not an
// ongoing attack surface worth a distributed lock over.
func (s *Server) handleSetup(w http.ResponseWriter, r *http.Request) {
	required, err := s.setupIsRequired(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	if !required {
		writeError(w, http.StatusConflict, fmt.Errorf("this deployment is already set up — please sign in instead"))
		return
	}

	var req setupRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("email and password are required"))
		return
	}
	// Matches `pgarchimigrator auth create-admin`'s exact validation — the CLI
	// and this endpoint are two doors into the same bootstrap action, and
	// should hold new admins to the same bar either way.
	if len(req.Password) < 8 {
		writeError(w, http.StatusBadRequest, fmt.Errorf("password must be at least 8 characters"))
		return
	}

	org, err := auth.EnsureDefaultOrganization(r.Context(), s.AuthService.Store, "Default Organization")
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to set up the default organization: %w", err))
		return
	}

	newAdmin, err := s.AuthService.CreateUser(r.Context(), org.ID, req.Email, req.Password, auth.RoleAdmin)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, auth.ErrDuplicateEmail) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}

	// Log the new admin in immediately, mirroring handleLogin's session
	// setup exactly — the whole point of this endpoint is a smooth
	// first-run experience, and making them separately log in right
	// after creating their own account would defeat that.
	token, user, err := s.AuthService.Login(r.Context(), req.Email, req.Password)
	if err != nil {
		// The account genuinely was created successfully at this point —
		// surfacing this as a failure would be misleading to the caller.
		// Fall back to reporting success without a session; the frontend
		// will land on the login screen instead of being auto-signed-in.
		writeJSON(w, http.StatusCreated, map[string]any{"id": newAdmin.ID, "email": newAdmin.Email, "role": newAdmin.Role})
		return
	}
	dur := s.AuthService.SessionDuration
	if dur == 0 {
		dur = auth.DefaultSessionDuration
	}
	auth.SetSessionCookie(w, token, time.Now().UTC().Add(dur), s.secureCookies)
	writeJSON(w, http.StatusOK, map[string]any{"id": user.ID, "email": user.Email, "role": user.Role})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := auth.SessionCookieValue(r); token != "" {
		_ = s.AuthService.Logout(r.Context(), token) // best-effort: cookie is cleared client-side regardless
	}
	auth.ClearSessionCookie(w)
	writeJSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFromContext(r.Context())
	if user == nil {
		// Shouldn't happen — this route is wrapped in s.protect — but
		// fail safely rather than dereference a nil user below.
		writeError(w, http.StatusUnauthorized, fmt.Errorf("not authenticated"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": user.ID, "email": user.Email, "role": user.Role})
}

// --- User management handlers (admin only) ---

type createUserRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	Role     string `json:"role"`
}

func (s *Server) handleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req createUserRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if req.Email == "" || req.Password == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("email and password are required"))
		return
	}

	role := auth.Role(req.Role)
	switch role {
	case auth.RoleViewer, auth.RoleOperator, auth.RoleAdmin:
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid role %q (must be viewer, operator, or admin)", req.Role))
		return
	}

	// New users are always created in the CALLING admin's own
	// organization — there is currently no cross-organization user
	// management endpoint, matching this package's single-org-per-deployment
	// scope for now (see internal/auth's package doc comment for the
	// multi-tenant extension path).
	currentUser := auth.UserFromContext(r.Context())
	newUser, err := s.AuthService.CreateUser(r.Context(), currentUser.OrganizationID, req.Email, req.Password, role)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, auth.ErrDuplicateEmail) {
			status = http.StatusConflict
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": newUser.ID, "email": newUser.Email, "role": newUser.Role})
}

func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	currentUser := auth.UserFromContext(r.Context())
	users, err := s.AuthService.Store.ListUsersByOrganization(r.Context(), currentUser.OrganizationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	resp := make([]map[string]any, 0, len(users))
	for _, u := range users {
		resp = append(resp, map[string]any{"id": u.ID, "email": u.Email, "role": u.Role})
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleDeleteUser refuses to let an admin delete their OWN account — not
// because that's unsafe in principle, but because it's almost always a
// mistake (e.g. a misclick), and there is currently no "transfer
// ownership" flow to fall back on if the last admin account is removed.
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	currentUser := auth.UserFromContext(r.Context())
	if id == currentUser.ID {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cannot delete your own account"))
		return
	}

	if err := s.AuthService.Store.DeleteUser(r.Context(), id); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, auth.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

type updateUserRoleRequest struct {
	Role string `json:"role"`
}

// handleUpdateUserRole refuses to let an admin change their OWN role,
// mirroring handleDeleteUser's identical self-protection reasoning: an
// accidental self-demotion (e.g. a misclick picking "viewer") could lock
// out the only admin in an organization, and there's currently no
// "transfer ownership" or "restore admin" flow to recover from that.
func (s *Server) handleUpdateUserRole(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	currentUser := auth.UserFromContext(r.Context())
	if id == currentUser.ID {
		writeError(w, http.StatusBadRequest, fmt.Errorf("cannot change your own role"))
		return
	}

	var req updateUserRoleRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}

	role := auth.Role(req.Role)
	switch role {
	case auth.RoleViewer, auth.RoleOperator, auth.RoleAdmin:
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid role %q (must be viewer, operator, or admin)", req.Role))
		return
	}

	if err := s.AuthService.Store.UpdateUserRole(r.Context(), id, role); err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, auth.ErrNotFound) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
}

// startMigrationRequest is the POST /api/migrations request body — a JSON
// mirror of orchestrator.MigrationRequest/strategy.ColumnChange, since
// those internal types aren't meant to double as a wire format (e.g.
// strategy.Strategy's exact string values are an internal implementation
// detail we still want to accept here, but validating/documenting them at
// the API boundary separately keeps the two concerns decoupled).
type startMigrationRequest struct {
	Schema           string `json:"schema"`
	Table            string `json:"table"`
	Column           string `json:"column"`
	Operation        string `json:"operation"`
	Type             string `json:"type"`
	Default          string `json:"default"`
	VolatileDefault  bool   `json:"volatile_default"`
	StrategyOverride string `json:"strategy_override"`
	Actor            string `json:"actor"`
	IndexName        string `json:"index_name"`       // ADD_INDEX (optional, auto-generated if empty) / DROP_INDEX (required)
	ConstraintName   string `json:"constraint_name"`  // SET_NOT_NULL (optional, auto-generated if empty) / ADD_CONSTRAINT (required)
	CheckExpression  string `json:"check_expression"` // ADD_CONSTRAINT only (required)
	NewColumnName    string `json:"new_column_name"`  // RENAME_COLUMN only (required)
	// Name/Description are purely human-facing, optional labels for the
	// job — see state.Job.Name's doc comment. Neither affects strategy
	// selection, validation, or the generated SQL in any way, so
	// handlePreviewMigration simply never looks at them (a dry-run
	// doesn't create a Job, so there's nothing to attach them to).
	Name        string `json:"name"`
	Description string `json:"description"`
}

// buildMigrationRequest validates a decoded startMigrationRequest and
// converts it into an orchestrator.MigrationRequest — shared by
// handleStartMigration and handlePreviewMigration so the two can never
// drift into validating requests differently. --column, --index-name, and
// --constraint-name/--check-expression are each required for some
// operations but not others — see cmd/pgarchimigrator/main.go's identical
// validation for the reasoning.
func buildMigrationRequest(req startMigrationRequest) (orchestrator.MigrationRequest, error) {
	if req.Table == "" || req.Operation == "" {
		return orchestrator.MigrationRequest{}, fmt.Errorf("table and operation are required")
	}

	switch strategy.Operation(req.Operation) {
	case strategy.OpDropIndex:
		if req.IndexName == "" {
			return orchestrator.MigrationRequest{}, fmt.Errorf("index_name is required for DROP_INDEX")
		}
	case strategy.OpAddConstraint:
		if req.ConstraintName == "" {
			return orchestrator.MigrationRequest{}, fmt.Errorf("constraint_name is required for ADD_CONSTRAINT")
		}
		if req.CheckExpression == "" {
			return orchestrator.MigrationRequest{}, fmt.Errorf("check_expression is required for ADD_CONSTRAINT")
		}
	case strategy.OpRenameColumn:
		if req.Column == "" {
			return orchestrator.MigrationRequest{}, fmt.Errorf("column (the existing name) is required for RENAME_COLUMN")
		}
		if req.NewColumnName == "" {
			return orchestrator.MigrationRequest{}, fmt.Errorf("new_column_name is required for RENAME_COLUMN")
		}
	default:
		if req.Column == "" {
			return orchestrator.MigrationRequest{}, fmt.Errorf("column is required for %s", req.Operation)
		}
	}

	schema := req.Schema
	if schema == "" {
		schema = "public"
	}
	actor := req.Actor
	if actor == "" {
		actor = "api"
	}

	return orchestrator.MigrationRequest{
		SchemaName: schema,
		TableName:  req.Table,
		Change: strategy.ColumnChange{
			Operation:         strategy.Operation(req.Operation),
			ColumnName:        req.Column,
			NewType:           req.Type,
			DefaultValue:      req.Default,
			IsVolatileDefault: req.VolatileDefault,
			IndexName:         req.IndexName,
			ConstraintName:    req.ConstraintName,
			CheckExpression:   req.CheckExpression,
			NewColumnName:     req.NewColumnName,
		},
		StrategyOverride: strategy.Strategy(req.StrategyOverride),
		Actor:            actor,
		Name:             req.Name,
		Description:      req.Description,
	}, nil
}

// handlePreviewMigration is the dry-run counterpart of
// handleStartMigration: same request shape and validation, but never
// creates a job or runs any DDL — see internal/preview's package doc
// comment. RoleViewer is enough to call this (unlike RoleOperator for the
// real thing) since it makes no changes at all.
// resolveTypeCompatibility fills in migReq.Change.TypeConversionCompatible
// for ALTER_COLUMN_TYPE requests using internal/typecompat's curated,
// conservative detection — see that package's doc comment for the exact
// scope and the safety reasoning. Skipped entirely when the caller gave
// an explicit StrategyOverride: an explicit choice always wins, this
// detection only fills in the answer when they didn't say. Shared by
// handleStartMigration and handlePreviewMigration so a dry run and the
// real migration it previews can never disagree about this.
func (s *Server) resolveTypeCompatibility(ctx context.Context, migReq *orchestrator.MigrationRequest) error {
	if migReq.Change.Operation != strategy.OpAlterType || migReq.StrategyOverride != "" {
		return nil
	}
	currentType, err := typecompat.CurrentColumnType(ctx, s.Pool, migReq.SchemaName, migReq.TableName, migReq.Change.ColumnName)
	if err != nil {
		return fmt.Errorf("failed to determine the column's current type for compatibility detection: %w", err)
	}
	migReq.Change.TypeConversionCompatible = typecompat.IsCompatible(currentType, migReq.Change.NewType)
	return nil
}

func (s *Server) handlePreviewMigration(w http.ResponseWriter, r *http.Request) {
	var req startMigrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	migReq, err := buildMigrationRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.resolveTypeCompatibility(r.Context(), &migReq); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	report, err := preview.Generate(r.Context(), s.Pool, s.Orchestrator.TableStats, migReq)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, report)
}

// handleListSchemas/handleListTables/handleListColumns back the New
// Migration screen's schema/table/column dropdowns — see
// internal/catalog's package doc comment for the read-only guarantee.
func (s *Server) handleListSchemas(w http.ResponseWriter, r *http.Request) {
	schemas, err := catalog.ListSchemas(r.Context(), s.Pool)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, schemas)
}

func (s *Server) handleListTables(w http.ResponseWriter, r *http.Request) {
	schema := r.PathValue("schema")
	tables, err := catalog.ListTables(r.Context(), s.Pool, schema)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, tables)
}

func (s *Server) handleListColumns(w http.ResponseWriter, r *http.Request) {
	schema := r.PathValue("schema")
	table := r.PathValue("table")
	columns, err := catalog.ListColumns(r.Context(), s.Pool, schema, table)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, columns)
}

// sampleRowLimit is fixed (not caller-configurable) — this endpoint is a
// quick "what does the data actually look like" glance for the New
// Migration screen, not a data browser; keeping it small keeps the query
// cheap regardless of table size.
const sampleRowLimit = 5

func (s *Server) handleSampleRows(w http.ResponseWriter, r *http.Request) {
	schema := r.PathValue("schema")
	table := r.PathValue("table")
	result, err := catalog.SampleRows(r.Context(), s.Pool, schema, table, sampleRowLimit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleTableStats(w http.ResponseWriter, r *http.Request) {
	schema := r.PathValue("schema")
	table := r.PathValue("table")
	stats, err := s.Orchestrator.TableStats(r.Context(), schema, table)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) handleGetConnectionInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.ConnectionInfo)
}

// handleStrategyMatrix serves strategy.ValidStrategyMatrix() as JSON —
// map[string][]string once encoded, keyed by operation. Purely static,
// server-build-time-known data; no database access, no per-request work.
func (s *Server) handleStrategyMatrix(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, strategy.ValidStrategyMatrix())
}

func (s *Server) handleStartMigration(w http.ResponseWriter, r *http.Request) {
	var req startMigrationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	migReq, err := buildMigrationRequest(req)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.resolveTypeCompatibility(r.Context(), &migReq); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	job, err := s.Orchestrator.StartMigration(r.Context(), migReq)
	if job == nil {
		// No job was ever created — the request itself was invalid
		// (bad stats fetch, bad strategy decision, etc.), not a migration
		// that ran and failed.
		writeError(w, http.StatusBadRequest, err)
		return
	}

	status := http.StatusOK
	if err != nil {
		// A job WAS created and Execute ran, but it failed — this is a
		// legitimate, inspectable outcome (job.Phase/LastError explain
		// why), so we return 200-adjacent semantics via 422 rather than a
		// generic 500: the API worked correctly, the migration did not.
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, progress.Compute(job))
}

func (s *Server) handleListMigrations(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.ListAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	reports := make([]*progress.Report, 0, len(jobs))
	for _, j := range jobs {
		reports = append(reports, progress.Compute(j))
	}
	writeJSON(w, http.StatusOK, reports)
}

// handleGetAnalytics serves the historical migration analytics summary
// (see progress.ComputeAnalytics's own doc comment) — computed
// entirely from the same job records handleListMigrations already
// reads, no new database queries against the target PostgreSQL server.
func (s *Server) handleGetAnalytics(w http.ResponseWriter, r *http.Request) {
	jobs, err := s.Store.ListAll(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, progress.ComputeAnalytics(jobs))
}

func (s *Server) handleGetMigration(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	job, err := s.Store.Get(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("job %q not found: %w", id, err))
		return
	}
	report := progress.Compute(job)
	s.attachReplicationLag(r.Context(), report, job.ReplicationSlotName)
	s.attachResourceStatus(r.Context(), report, job)
	s.attachCheckpointPressure(r.Context(), report)
	measureImpact := r.URL.Query().Get("measureImpact") == "true"
	s.attachImpactMeasurement(r.Context(), report, job, measureImpact)
	writeJSON(w, http.StatusOK, report)
}

func (s *Server) handleRollback(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	actor := r.URL.Query().Get("actor")
	if actor == "" {
		actor = "api"
	}

	job, err := s.Orchestrator.RollbackMigration(r.Context(), id, actor)
	if job == nil {
		writeError(w, http.StatusNotFound, err)
		return
	}

	status := http.StatusOK
	if err != nil {
		status = http.StatusUnprocessableEntity
	}
	writeJSON(w, status, progress.Compute(job))
}

// handleSweep runs one on-demand reaper pass (see `pgarchimigrator sweep` in
// cmd/pgarchimigrator/main.go, which wraps the same two calls) — useful for a
// dashboard "Clean up now" button, or an external scheduler hitting this
// endpoint instead of running a long-lived reaper.Run loop.
func (s *Server) handleSweep(w http.ResponseWriter, r *http.Request) {
	if s.Reaper == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("reaper is not configured on this server"))
		return
	}

	scanResult, scanErr := s.Reaper.ScanOnce(r.Context())
	sweepResult, sweepErr := s.Reaper.SweepExpiredRollbackWindows(r.Context())

	resp := map[string]any{"scan": scanResult, "sweep": sweepResult}
	status := http.StatusOK
	if scanErr != nil || sweepErr != nil {
		status = http.StatusInternalServerError
		if scanErr != nil {
			resp["scan_error"] = scanErr.Error()
		}
		if sweepErr != nil {
			resp["sweep_error"] = sweepErr.Error()
		}
	}
	writeJSON(w, status, resp)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data, err := dashboardFS.ReadFile("dashboard.html")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_, _ = w.Write(data)
}

// registerWebapp mounts the embedded React SPA at /app/. If the embedded
// "webapp" directory is somehow empty (shouldn't happen in a normal
// build, since it's committed to the repo — but defensively handled in
// case a future build step ever clears it), this logs and skips
// registration rather than panicking the whole server over a UI route.
func (s *Server) registerWebapp() {
	sub, err := fs.Sub(webappFS, "webapp")
	if err != nil {
		log.Printf("webapp not embedded, skipping /app: %v", err)
		return
	}
	fileServer := http.FileServer(spaFileSystem{http.FS(sub)})
	s.mux.Handle("GET /app/", http.StripPrefix("/app", fileServer))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends a JSON error response. For a 500 (internal server
// error), the real error text is deliberately NOT sent to the client —
// only logged server-side — and a generic message is returned instead.
//
// Found during a security self-audit: this function previously sent
// err.Error() to the client verbatim regardless of status code, meaning
// an internal failure (a raw pgx connection error, a SQL execution
// error) was returned as-is to ANY authenticated caller, including the
// lowest-privilege RoleViewer tier — potentially exposing internal
// infrastructure details (database host/port, internal error text) that
// have no business being visible outside server logs. Every OTHER status
// code (400, 401, 403, 404, 409, 422, 429) still returns the real message
// verbatim: those are deliberately caller-facing validation/business-logic
// errors ("email is required", "invalid role", "job not found") that are
// both safe and genuinely useful to show in full — the split is by
// status code, not a blanket policy either way.
func writeError(w http.ResponseWriter, status int, err error) {
	msg := "unknown error"
	if err != nil {
		msg = err.Error()
	}
	if status == http.StatusInternalServerError {
		log.Printf("internal error: %v", err)
		msg = "an internal error occurred; check server logs for details"
	}
	writeJSON(w, status, map[string]string{"error": msg})
}

// Shutdown is a thin passthrough kept for symmetry with http.Server's
// lifecycle; cmd/pgarchimigrator/main.go's `serve` command owns the actual
// http.Server and calls its Shutdown directly — this exists so future
// callers of Server (e.g. tests, or an embedding application) have a
// single, obvious place to look for graceful-shutdown wiring if Server
// ever grows its own background goroutines.
func (s *Server) Shutdown(ctx context.Context) error {
	return nil
}
