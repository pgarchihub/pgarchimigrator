package api

import (
	"context"
	"sync"
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/db"
	"github.com/pgarchihub/pgarchimigrator/internal/progress"
)

// minSignificantLagChangeBytes is the floor below which a lag change is
// treated as measurement noise rather than a real trend — without this,
// a job whose lag just happens to start near 0 (a fresh slot, right
// after creation) would flip between "growing"/"shrinking" on every few
// KB of ordinary WAL traffic, since a purely percentage-based threshold
// (see lagTrendTracker.observe) is meaningless near zero.
const minSignificantLagChangeBytes = 1_000_000 // 1MB

// sustainedGrowthEscalationThreshold is how long lag has to have been
// CONTINUOUSLY growing (no intervening "stable"/"shrinking" reading)
// before the UI escalates from the routine "Growing" trend badge to an
// explicit "may not converge" warning — see attachReplicationLag's doc
// comment for the real incident (a SHADOW_TABLE migration's delta sync
// that genuinely never converged under sustained heavy write load) this
// distinction exists to surface clearly, without the tool ever
// autonomously acting on it — see this const's own reasoning below for
// why the decision to actually stop a migration is left to the human
// watching, not made automatically.
//
// Deliberately NOT an automatic-abort trigger: a load test's synthetic,
// unrelenting write pressure genuinely never converges, but a REAL
// production traffic spike easily could — automatically killing a
// migration on a plausible-but-wrong read of "this will never finish"
// risks aborting one that would have completed fine, and doing so
// without asking is a worse failure mode than a slow, correctly-flagged
// one. This surfaces a clear, actionable signal and leaves the actual
// decision (and the existing rollback control) to the person watching.
const sustainedGrowthEscalationThreshold = 3 * time.Minute

// lagTrendTracker remembers the most recently observed replication lag
// per job, purely in memory — used to compute a growing/shrinking/stable
// trend, and how long any ongoing growth has been continuous, across
// successive GET /api/migrations/{id} polls. Deliberately NOT its own
// background sampling goroutine: the frontend already polls a running
// migration's status every few seconds, so this rides along with
// requests that are happening anyway rather than adding a second,
// independent polling loop.
//
// Losing this map on a server restart is fine — correctness of the
// SHADOW_TABLE migration itself never depends on it, only the trend
// indicator's usefulness, and the next poll simply starts a fresh
// baseline ("unknown" trend for one cycle, then meaningful again — a
// sustained-growth streak that was building resets too, which is an
// acceptable trade-off for something this ephemeral rather than adding
// persistence for a purely advisory UI signal).
type lagTrendTracker struct {
	mu   sync.Mutex
	last map[string]int64 // jobID -> last observed LagBytes
	// growingSince records when a job's lag STARTED continuously
	// growing — reset (deleted) the moment a poll shows anything other
	// than "growing", so this only ever reflects an UNBROKEN streak, not
	// cumulative time spent growing across separate episodes.
	growingSince map[string]time.Time
}

func newLagTrendTracker() *lagTrendTracker {
	return &lagTrendTracker{last: make(map[string]int64), growingSince: make(map[string]time.Time)}
}

// observe records a new reading and returns the trend relative to the
// previous one for this job ("growing", "shrinking", "stable", or
// "unknown" — no prior reading yet, e.g. the first poll after the slot
// was created) plus how long lag has been continuously growing without
// interruption (zero unless the current trend is itself "growing").
func (t *lagTrendTracker) observe(jobID string, currentBytes int64) (trend string, growingDuration time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()

	prev, ok := t.last[jobID]
	t.last[jobID] = currentBytes
	if !ok {
		return "unknown", 0
	}

	// A percentage band around the previous reading counts as "stable"
	// — avoids flapping the indicator on ordinary measurement noise (a
	// single UPDATE's worth of WAL is a rounding error at gigabyte
	// scale, but could otherwise flip the sign every poll) — floored at
	// minSignificantLagChangeBytes so this doesn't collapse to
	// near-zero when prev itself is small.
	threshold := int64(float64(prev) * 0.05)
	if threshold < minSignificantLagChangeBytes {
		threshold = minSignificantLagChangeBytes
	}

	diff := currentBytes - prev
	switch {
	case diff > threshold:
		trend = "growing"
		if _, alreadyGrowing := t.growingSince[jobID]; !alreadyGrowing {
			t.growingSince[jobID] = time.Now()
		}
		growingDuration = time.Since(t.growingSince[jobID])
	case diff < -threshold:
		trend = "shrinking"
		delete(t.growingSince, jobID)
	default:
		trend = "stable"
		delete(t.growingSince, jobID)
	}
	return trend, growingDuration
}

// forget removes a job's tracked state — called once a migration reaches
// a terminal phase, so these maps don't grow unboundedly over the
// server's lifetime as jobs come and go.
func (t *lagTrendTracker) forget(jobID string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.last, jobID)
	delete(t.growingSince, jobID)
}

// attachReplicationLag enriches report in place with a live replication
// lag reading — a no-op (report left untouched) unless the job is a
// SHADOW_TABLE migration with an active replication slot right now.
// Best-effort: a query failure here is logged-and-ignored, never
// surfaced as an API error — this is a supplementary live indicator, not
// something that should make an otherwise-successful "get migration
// status" request fail.
//
// When growth has been continuous for at least
// sustainedGrowthEscalationThreshold, also sets
// ReplicationLagGrowingForSeconds — the UI's signal to escalate from a
// routine "Growing" badge to an explicit "may not converge" warning. See
// this whole file's real load-testing history: a SHADOW_TABLE
// migration's delta sync can, under heavy sustained write load, never
// converge — the ApplyEngine's decode+apply throughput simply can't keep
// pace with incoming WAL, and without a signal like this, that looked
// identical, from the outside, to a migration that was healthy but just
// slow.
func (s *Server) attachReplicationLag(ctx context.Context, report *progress.Report, slotName string) {
	if slotName == "" {
		return
	}
	if report.Terminal {
		// The migration is done (success, failure, or aborted) — the
		// slot is either already cleaned up or on its way out via
		// internal/reaper; either way, stop tracking this job so
		// lagTracker's maps don't hold it forever.
		s.lagTracker.forget(report.JobID)
		return
	}

	lag, found, err := db.FetchReplicationLag(ctx, s.Pool, slotName)
	if err != nil || !found {
		// Not an error worth surfacing to the caller — see this
		// function's own doc comment. A query failure or "no slot yet"
		// both simply mean no lag data to show right now.
		return
	}

	bytes := lag.LagBytes
	report.ReplicationLagBytes = &bytes
	trend, growingDuration := s.lagTracker.observe(report.JobID, lag.LagBytes)
	report.ReplicationLagTrend = trend
	if growingDuration >= sustainedGrowthEscalationThreshold {
		seconds := int64(growingDuration.Seconds())
		report.ReplicationLagGrowingForSeconds = &seconds
	}
}
