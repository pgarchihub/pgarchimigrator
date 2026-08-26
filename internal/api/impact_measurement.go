package api

import (
	"context"
	"log"
	"sync"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/progress"
	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// impactTracker remembers the highest MaxDurationSeconds ever observed
// per job, purely in memory — a running "worst it got" across the whole
// migration, not just whatever a single instantaneous snapshot happens
// to show (which could easily miss a spike that occurred between two
// polls). Same rationale as lagTrendTracker for riding along with
// existing polling rather than running its own background sampling loop
// — see attachImpactMeasurement's own doc comment for why this is
// opt-in at all, unlike the lag/checkpoint/resource indicators.
//
// This is the in-memory, fast-path cache the live (still-running)
// branch reads/writes on every poll — see state.Job.ImpactPeakQueryDurationSeconds
// for the DURABLE counterpart that survives a server restart and is what
// a TERMINAL job's peak is actually read from.
type impactTracker struct {
	mu   sync.Mutex
	peak map[string]float64 // jobID -> highest MaxDurationSeconds observed so far
}

func newImpactTracker() *impactTracker {
	return &impactTracker{peak: make(map[string]float64)}
}

// observe records a new reading and returns the running peak for this
// job (the current reading, or the previous peak, whichever is higher).
func (t *impactTracker) observe(jobID string, currentMaxDuration float64) float64 {
	t.mu.Lock()
	defer t.mu.Unlock()

	if currentMaxDuration > t.peak[jobID] {
		t.peak[jobID] = currentMaxDuration
	}
	return t.peak[jobID]
}

// forget removes a job's tracked peak — called once a migration reaches
// a terminal phase, so this map doesn't grow unboundedly over the
// server's lifetime as jobs come and go. Safe to call even if the job
// was never tracked (a no-op map delete).
func (t *impactTracker) forget(jobID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.peak, jobID)
}

// attachImpactMeasurement enriches report with "is this migration making
// real queries against this table slow" data — see db.FetchTableActivity's
// own doc comment for how the LIVE reading is measured.
//
// Two different paths depending on whether the job is terminal:
//
//   - TERMINAL: reads the DURABLE, already-persisted peak straight off
//     job (state.Job.ImpactPeakQueryDurationSeconds) — a free in-memory
//     field read on data the caller (handleGetMigration) already fetched,
//     no new query. Shown UNCONDITIONALLY (regardless of the measure
//     parameter) — this is the automatic post-migration impact report
//     Faz D's roadmap calls for: if impact measurement was ever turned on
//     WHILE this migration was running, its result is now a permanent
//     part of this job's history, not something that only appeared for
//     whoever happened to be watching at the right moment. nil (not
//     shown) means impact measurement was simply never turned on for
//     this migration — see state.Job's own doc comment on that field for
//     why this is distinguished from "measured, found to be zero".
//
//   - NOT terminal (still running): only runs, and only queries live,
//     when measure is true — this is the one indicator in the trust-layer
//     set with a genuinely non-trivial query cost (a three-way join
//     across pg_locks/pg_class/pg_stat_activity, run on every poll for
//     however long a migration runs) rather than a single cheap
//     system-view read, hence opt-in (see
//     pgArchiMigrator_Guven_Katmani_Tasarimi.md's "Faz 2.4"). Each live
//     reading is written through to the durable store (best-effort) as
//     well as the in-memory tracker — catching the EXACT moment a
//     migration transitions to terminal isn't reliable (depends on
//     someone polling with measure=true at just the right instant), so
//     persisting continuously on every live poll means the last
//     successfully-written value is already correct (or very close) by
//     the time the migration finishes, regardless of exact timing.
func (s *Server) attachImpactMeasurement(ctx context.Context, report *progress.Report, job *state.Job, measure bool) {
	if report.Terminal {
		s.impactTracker.forget(report.JobID) // no-op if never tracked; harmless either way
		report.ImpactPeakQueryDurationSeconds = job.ImpactPeakQueryDurationSeconds
		return
	}
	if !measure {
		return
	}

	activity, err := db.FetchTableActivity(ctx, s.Pool, job.SchemaName, job.TableName)
	if err != nil {
		// Best-effort — see this function's own doc comment: a query
		// failure here is logged-and-ignored, never surfaced as an API
		// error, since this is a supplementary, explicitly-opted-into
		// indicator, not something that should make an otherwise-
		// successful "get migration status" request fail.
		return
	}

	peak := s.impactTracker.observe(report.JobID, activity.MaxDurationSeconds)
	report.ImpactActiveQueries = &activity.ActiveQueries
	report.ImpactPeakQueryDurationSeconds = &peak

	if err := s.Store.UpdateImpactPeak(ctx, report.JobID, peak); err != nil {
		log.Printf("api: failed to persist impact peak for job %s: %v", report.JobID, err)
	}
}
