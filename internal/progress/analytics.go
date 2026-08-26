package progress

import (
	"time"

	"github.com/pgarchihub/pgarchimigrator/internal/state"
)

// Analytics summarizes a set of jobs (typically every job the store
// knows about) into the aggregate view Faz D of
// pgArchiMigrator_Guven_Katmani_Tasarimi.md's roadmap calls for — "hangi
// strateji ne sıklıkla kullanıldı, ortalama süre, başarısızlık oranı"
// (which strategy is used how often, average duration, failure rate).
// Computed ENTIRELY from data the store already has (Job.Strategy/
// Phase/CreatedAt/UpdatedAt) — no new database queries against the
// target PostgreSQL server, no new cost beyond what listing jobs already
// does.
type Analytics struct {
	TotalMigrations int `json:"totalMigrations"`
	// TerminalMigrations excludes anything still in progress — the
	// denominator for FailureRate and the population AverageDurationMs
	// is computed over, since an in-progress job's duration isn't
	// meaningful yet and its eventual outcome isn't known.
	TerminalMigrations int `json:"terminalMigrations"`
	// FailureRate is Failed / TerminalMigrations, 0 when there are no
	// terminal migrations yet (not NaN — see the doc comment on
	// ComputeAnalytics for why this is deliberately not divide-by-zero).
	FailureRate float64 `json:"failureRate"`
	// AverageDurationMs is the mean CreatedAt-to-UpdatedAt span across
	// terminal jobs — a coarse, whole-fleet figure; see
	// StrategyBreakdown for a per-strategy figure, which is usually more
	// actionable (a fleet dominated by fast DIRECT_DDL jobs would
	// otherwise make one slow SHADOW_TABLE migration look unremarkable
	// in the average).
	AverageDurationMs float64                   `json:"averageDurationMs"`
	StrategyBreakdown map[string]*StrategyStats `json:"strategyBreakdown"`
}

// StrategyStats is one strategy's slice of Analytics — see Analytics'
// own doc comment.
type StrategyStats struct {
	Count             int     `json:"count"`
	FailureRate       float64 `json:"failureRate"`
	AverageDurationMs float64 `json:"averageDurationMs"`
}

// ComputeAnalytics aggregates jobs into an Analytics summary. Pure,
// synchronous, in-memory — safe to call on every request to the
// analytics endpoint without needing its own caching layer, given
// realistic job-history sizes (thousands, not millions, of rows for any
// deployment this tool is meant for).
//
// Division-by-zero is deliberately avoided throughout (an empty or
// all-still-running job list yields FailureRate=0/AverageDurationMs=0,
// not NaN) — a NaN silently serialized to JSON becomes `null` in most
// encoders, which would be a confusing, undocumented special case for
// the frontend to have to handle; a plain 0 for "nothing to report yet"
// is simpler and still accurate (there's genuinely no failure rate to
// speak of yet).
func ComputeAnalytics(jobs []*state.Job) *Analytics {
	a := &Analytics{
		TotalMigrations:   len(jobs),
		StrategyBreakdown: make(map[string]*StrategyStats),
	}

	var totalDuration time.Duration
	var totalFailed int
	// perStrategy accumulates raw sums before the final division pass —
	// keeps StrategyStats itself simple (just the final, already-divided
	// numbers), rather than needing running-average bookkeeping.
	type accum struct {
		count, failed int
		duration      time.Duration
	}
	perStrategy := make(map[string]*accum)

	for _, job := range jobs {
		if !isTerminalPhase(job.Phase) {
			continue
		}
		a.TerminalMigrations++

		duration := job.UpdatedAt.Sub(job.CreatedAt)
		if duration < 0 {
			// Should never legitimately happen (would mean UpdatedAt
			// predates CreatedAt) — clamped defensively rather than
			// letting a single bad record skew the whole fleet's average
			// with a negative number, matching format.formatDuration's
			// identical clamping on the frontend for the same reasoning.
			duration = 0
		}
		totalDuration += duration

		failed := job.Phase == state.PhaseFailed
		if failed {
			totalFailed++
		}

		if perStrategy[job.Strategy] == nil {
			perStrategy[job.Strategy] = &accum{}
		}
		s := perStrategy[job.Strategy]
		s.count++
		s.duration += duration
		if failed {
			s.failed++
		}
	}

	if a.TerminalMigrations > 0 {
		a.FailureRate = float64(totalFailed) / float64(a.TerminalMigrations)
		a.AverageDurationMs = float64(totalDuration.Milliseconds()) / float64(a.TerminalMigrations)
	}

	for strat, s := range perStrategy {
		stats := &StrategyStats{Count: s.count}
		if s.count > 0 {
			stats.FailureRate = float64(s.failed) / float64(s.count)
			stats.AverageDurationMs = float64(s.duration.Milliseconds()) / float64(s.count)
		}
		a.StrategyBreakdown[strat] = stats
	}

	return a
}

// isTerminalPhase mirrors Report.Terminal's own computation (see
// Compute below) — duplicated rather than shared because Report's
// Terminal field is computed as a side effect of building the whole
// Report, and pulling that apart just to reuse one boolean would add
// more coupling than the three-line duplication costs.
func isTerminalPhase(phase state.Phase) bool {
	switch phase {
	case state.PhaseCompleted, state.PhaseFailed, state.PhaseAborted:
		return true
	default:
		return false
	}
}
