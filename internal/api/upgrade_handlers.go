package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/catalog"
	"github.com/pgarchihub/pgarchimigrator/engines/postgresql/upgrade"
)

// startUpgradeRequest is the human/dashboard-facing counterpart to
// upgrade.Job's own fields — a plain JSON body, matching this
// project's other cookie-authenticated POST endpoints (unlike
// handleEcosystemStartMigration's own form-encoded OAuth2 body, which
// follows a different contract for a different caller).
type startUpgradeRequest struct {
	SourceDSN string   `json:"sourceDsn"`
	TargetDSN string   `json:"targetDsn"`
	Schemas   []string `json:"schemas"`
	// SourceReplicationDSN is optional — see upgrade.Job.SourceReplicationRef's
	// own doc comment for exactly when it must be set explicitly
	// (distinct from SourceDSN): whenever this server process and the
	// TARGET instance's own PostgreSQL server have a different network
	// view of the source.
	SourceReplicationDSN string `json:"sourceReplicationDsn,omitempty"`
	// Tables is optional, explicit table-level scoping — see
	// upgrade.Job.Tables' own doc comment for why this is mutually
	// exclusive with Schemas rather than combined with it (the
	// dashboard's own checkbox picker always sends ONE or the other,
	// never both).
	Tables []upgrade.TableRef `json:"tables,omitempty"`
}

// handleStartUpgrade creates an upgrade.Job and runs its Flow in the
// background — the engines/postgresql/upgrade analog of
// orchestrator.StartMigrationAsync, for the identical reason: a
// whole-database upgrade can genuinely take hours, far longer than any
// HTTP client should be expected to block on. Returns 503 if
// s.UpgradeStore/UpgradeConnections were never wired up (see Server's
// own doc comment on those fields), matching handleOAuthToken's
// identical "optional dependency wasn't configured" precedent.
func (s *Server) handleStartUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.UpgradeStore == nil || s.UpgradeConnections == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("database upgrade is not configured on this instance"))
		return
	}

	var req startUpgradeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if req.SourceDSN == "" || req.TargetDSN == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("sourceDsn and targetDsn are both required"))
		return
	}

	job := &upgrade.Job{
		Schemas:              req.Schemas,
		SourceConnectionRef:  req.SourceDSN,
		TargetConnectionRef:  req.TargetDSN,
		SourceReplicationRef: req.SourceReplicationDSN,
		Tables:               req.Tables,
	}
	if err := s.UpgradeStore.CreateJob(r.Context(), job); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to create upgrade job: %w", err))
		return
	}

	flow := &upgrade.Flow{Store: s.UpgradeStore, ConnectionProvider: s.UpgradeConnections}
	go func() {
		// context.Background(), not r.Context() — the incoming HTTP
		// request's context is canceled the moment this handler
		// returns (immediately after this goroutine is launched), and
		// Flow.Run can legitimately run for hours. Same reasoning as
		// orchestrator.StartMigrationAsync's own identical choice —
		// see that function's doc comment.
		_ = flow.Run(context.Background(), job)
		// Errors are deliberately not logged here beyond what Flow.Run
		// itself already recorded via Store.UpdateJobPhaseWithError —
		// a caller polling GET /api/upgrades/{id} sees job.LastError
		// directly; this goroutine has no other channel to report
		// through once the HTTP response has already been sent.
	}()

	statusURL := fmt.Sprintf("/api/upgrades/%s", job.ID)
	w.Header().Set("Location", statusURL)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"id":        job.ID,
		"status":    "accepted",
		"statusUrl": statusURL,
	})
}

func (s *Server) handleListUpgrades(w http.ResponseWriter, r *http.Request) {
	if s.UpgradeStore == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("database upgrade is not configured on this instance"))
		return
	}
	jobs, err := s.UpgradeStore.ListJobs(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to list upgrade jobs: %w", err))
		return
	}
	writeJSON(w, http.StatusOK, jobs)
}

// upgradeDetailResponse combines a Job with its own per-table progress
// — the dashboard's own single round trip for a job's detail page,
// rather than requiring two separate requests (GET the job, then GET
// its tables) for what's always shown together.
type upgradeDetailResponse struct {
	*upgrade.Job
	Tables []*upgrade.Table `json:"tables"`
}

func (s *Server) handleGetUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.UpgradeStore == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("database upgrade is not configured on this instance"))
		return
	}
	id := r.PathValue("id")
	job, err := s.UpgradeStore.GetJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("upgrade job %s not found: %w", id, err))
		return
	}
	tables, err := s.UpgradeStore.ListTables(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to list tables for upgrade job %s: %w", id, err))
		return
	}
	writeJSON(w, http.StatusOK, upgradeDetailResponse{Job: job, Tables: tables})
}

// handleRetryUpgrade starts a NEW upgrade job using the SAME
// source/target/replication connection info and schema scope as an
// existing job — retry's own upgrade counterpart to
// handleRetryMigration, see that function's own doc comment for the
// "genuinely new job, not a resumption" reasoning, which applies
// identically here (engines/postgresql/upgrade.Flow has no "reset a FAILED job"
// operation either).
//
// This is precisely why upgrade.Job stores SourceConnectionRef/
// TargetConnectionRef/SourceReplicationRef as literal strings rather
// than, say, a one-time-use token — see StaticConnectionProvider's own
// doc comment on that accepted trade-off; retry is a direct, concrete
// benefit of it: the same connection info a first attempt used is
// already sitting on the job record, so a retry needs zero re-entry of
// credentials, not even by asking the browser to resubmit anything
// sensitive — every value is read entirely server-side.
// retryUpgradeRequest is optional — an empty/absent body means "reuse
// everything exactly as stored," the original behavior. ReplicationHost/
// ReplicationPort let a retry fix the ONE field most likely to need
// adjusting between attempts (see rebuildReplicationRef's own doc
// comment for why only host/port, never full credentials, are accepted
// here) without requiring a whole new database migration to be started
// from scratch just to correct it.
type retryUpgradeRequest struct {
	ReplicationHost string `json:"replicationHost,omitempty"`
	ReplicationPort string `json:"replicationPort,omitempty"`
}

// rebuildReplicationRef takes the ORIGINAL job's own already-stored
// SourceConnectionRef (username/password/database and all) and swaps
// in a different host/port — reusing the exact same credentials rather
// than asking for them again. This is deliberately the ONLY way a
// retry can adjust its own replication address: accepting a
// caller-supplied host/port (not a full connection string) means the
// password never needs to leave this server at all, matching every
// other retry field's own "reused entirely server-side" guarantee.
func rebuildReplicationRef(sourceConnectionRef, newHost, newPort string) (string, error) {
	u, err := url.Parse(sourceConnectionRef)
	if err != nil {
		return "", fmt.Errorf("could not parse the original source connection string: %w", err)
	}
	if newHost != "" {
		// A host-only override must keep the ORIGINAL port, not silently
		// fall back to PostgreSQL's own default (5432) — a real gap
		// found while verifying this function end to end: setting
		// u.Host to a bare hostname with no port at all drops whatever
		// non-default port the original connection used.
		port := u.Port()
		if newPort != "" {
			port = newPort
		}
		u.Host = newHost
		if port != "" {
			u.Host = newHost + ":" + port
		}
	} else if newPort != "" {
		// Port-only override: keep the existing host, just replace the
		// port — url.Hostname() strips any existing ":port" suffix from
		// u.Host, so this can't accidentally end up with two ports
		// concatenated together.
		u.Host = u.Hostname() + ":" + newPort
	}
	return u.String(), nil
}

func (s *Server) handleRetryUpgrade(w http.ResponseWriter, r *http.Request) {
	if s.UpgradeStore == nil || s.UpgradeConnections == nil {
		writeError(w, http.StatusServiceUnavailable, fmt.Errorf("database upgrade is not configured on this instance"))
		return
	}
	id := r.PathValue("id")
	original, err := s.UpgradeStore.GetJob(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, fmt.Errorf("upgrade job %s not found: %w", id, err))
		return
	}

	// A body is optional — an empty/absent one is exactly the original
	// "reuse everything as stored" behavior. json.NewDecoder's own
	// io.EOF on an empty body is expected here, not an error to reject.
	var req retryUpgradeRequest
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
			return
		}
	}

	replicationRef := original.SourceReplicationRef
	if req.ReplicationHost != "" || req.ReplicationPort != "" {
		rebuilt, err := rebuildReplicationRef(original.SourceConnectionRef, req.ReplicationHost, req.ReplicationPort)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		replicationRef = rebuilt
	}

	job := &upgrade.Job{
		Schemas:              original.Schemas,
		SourceConnectionRef:  original.SourceConnectionRef,
		TargetConnectionRef:  original.TargetConnectionRef,
		SourceReplicationRef: replicationRef,
		Tables:               original.Tables,
	}
	if err := s.UpgradeStore.CreateJob(r.Context(), job); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("failed to create upgrade job: %w", err))
		return
	}

	flow := &upgrade.Flow{Store: s.UpgradeStore, ConnectionProvider: s.UpgradeConnections}
	go func() {
		_ = flow.Run(context.Background(), job) // see handleStartUpgrade's own identical goroutine for why context.Background()
	}()

	statusURL := fmt.Sprintf("/api/upgrades/%s", job.ID)
	w.Header().Set("Location", statusURL)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"id":        job.ID,
		"status":    "accepted",
		"statusUrl": statusURL,
	})
}

// introspectSourceRequest carries just the source connection info —
// the dashboard calls this BEFORE a job exists at all, purely to
// populate the schema/table checkbox picker (see NewUpgrade.tsx).
type introspectSourceRequest struct {
	SourceDSN string `json:"sourceDsn"`
}

// introspectedSchema is one schema's own name plus every table found
// in it — the dashboard's own tree structure for its checkbox picker.
type introspectedSchema struct {
	Name   string   `json:"name"`
	Tables []string `json:"tables"`
}

type introspectSourceResponse struct {
	Schemas []introspectedSchema `json:"schemas"`
}

// introspectTimeout bounds how long handleIntrospectUpgradeSource will
// wait for the given source to respond — this request happens WHILE a
// person is filling out a form, so a genuinely unreachable host (a
// typo'd hostname, a firewalled port) must fail back to the UI quickly
// rather than hanging the request indefinitely.
const introspectTimeout = 10 * time.Second

// handleIntrospectUpgradeSource connects to an ARBITRARY, caller-supplied
// PostgreSQL instance and lists its schemas/tables — a genuinely new
// capability this package didn't have before Priority 3: every other
// connection this product makes is either to its own configured
// PGARCHIMIGRATOR_DATABASE_URL, or as part of an upgrade job that
// already exists. This one exists purely to populate the dashboard's
// schema/table checkbox picker BEFORE a job is created at all.
//
// Grants no capability an admin didn't already have — starting a real
// upgrade already lets this same caller connect to and act on any
// PostgreSQL instance whose connection string they can supply; this
// endpoint only reads catalog metadata (engines/postgresql/catalog.ListSchemas/
// ListTables, the exact same introspection every other operation in
// this codebase already uses), so it doesn't expand what an admin
// could already do. RoleAdmin-gated to match the same role
// handleStartUpgrade itself requires.
//
// The pool opened here is short-lived and closed before this handler
// returns — unlike a real upgrade's own sourcePool (kept open for the
// duration of Flow.Run), there's nothing for this one-shot metadata
// query to keep around afterward.
func (s *Server) handleIntrospectUpgradeSource(w http.ResponseWriter, r *http.Request) {
	var req introspectSourceRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid request body: %w", err))
		return
	}
	if req.SourceDSN == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("sourceDsn is required"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), introspectTimeout)
	defer cancel()

	pool, err := pgxpool.New(ctx, req.SourceDSN)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("invalid source connection string: %w", err))
		return
	}
	defer pool.Close()

	// pgxpool.New itself doesn't necessarily attempt a connection
	// eagerly — Ping forces an immediate, real connection attempt, so
	// an unreachable host fails HERE with a clear error rather than on
	// whichever catalog query happens to run first below.
	if err := pool.Ping(ctx); err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("could not connect to source: %w", err))
		return
	}

	schemaNames, err := catalog.ListSchemas(ctx, pool)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Errorf("could not list schemas on source: %w", err))
		return
	}

	schemas := make([]introspectedSchema, 0, len(schemaNames))
	for _, schemaName := range schemaNames {
		tables, err := catalog.ListTables(ctx, pool, schemaName)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("could not list tables in schema %q: %w", schemaName, err))
			return
		}
		schemas = append(schemas, introspectedSchema{Name: schemaName, Tables: tables})
	}

	writeJSON(w, http.StatusOK, introspectSourceResponse{Schemas: schemas})
}
