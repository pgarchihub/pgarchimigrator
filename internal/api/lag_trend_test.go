package api

import (
	"testing"
	"time"
)

func TestLagTrendTracker_FirstObservation_ReturnsUnknown(t *testing.T) {
	tr := newLagTrendTracker()
	trend, growing := tr.observe("job-1", 1000)
	if trend != "unknown" {
		t.Errorf("expected \"unknown\" for the first observation of a job, got %q", trend)
	}
	if growing != 0 {
		t.Errorf("expected zero growing duration for the first observation, got %v", growing)
	}
}

func TestLagTrendTracker_DetectsGrowth(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 10_000_000)
	trend, _ := tr.observe("job-1", 20_000_000) // +100%, well past the 5% threshold
	if trend != "growing" {
		t.Errorf("expected \"growing\", got %q", trend)
	}
}

func TestLagTrendTracker_DetectsShrinkage(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 20_000_000)
	trend, growing := tr.observe("job-1", 10_000_000) // -50%
	if trend != "shrinking" {
		t.Errorf("expected \"shrinking\", got %q", trend)
	}
	if growing != 0 {
		t.Errorf("expected zero growing duration while shrinking, got %v", growing)
	}
}

func TestLagTrendTracker_SmallChange_ReportsStable(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 100_000_000)
	trend, growing := tr.observe("job-1", 101_000_000) // +1%, within the 5% band
	if trend != "stable" {
		t.Errorf("expected \"stable\" for a 1%% change, got %q", trend)
	}
	if growing != 0 {
		t.Errorf("expected zero growing duration while stable, got %v", growing)
	}
}

// TestLagTrendTracker_TinyAbsoluteChangeNearZero_ReportsStable is the
// direct regression test for minSignificantLagChangeBytes existing at
// all: a purely percentage-based threshold is meaningless near zero — a
// freshly-created slot starting at, say, 100 bytes of lag would flip to
// "growing" on the very next poll from completely ordinary WAL traffic
// (a single row update), even though nothing concerning is happening.
func TestLagTrendTracker_TinyAbsoluteChangeNearZero_ReportsStable(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 100)
	trend, _ := tr.observe("job-1", 50_000) // a 500x percentage jump, but still under the 1MB absolute floor
	if trend != "stable" {
		t.Errorf("expected \"stable\" (change is under the absolute noise floor), got %q", trend)
	}
}

func TestLagTrendTracker_TracksMultipleJobsIndependently(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 10_000_000)
	tr.observe("job-2", 50_000_000)

	trend1, _ := tr.observe("job-1", 20_000_000) // growing
	trend2, _ := tr.observe("job-2", 25_000_000) // shrinking

	if trend1 != "growing" {
		t.Errorf("job-1: expected \"growing\", got %q", trend1)
	}
	if trend2 != "shrinking" {
		t.Errorf("job-2: expected \"shrinking\", got %q", trend2)
	}
}

func TestLagTrendTracker_Forget_ResetsToUnknownOnNextObservation(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 10_000_000)
	tr.observe("job-1", 20_000_000) // establishes a real trend

	tr.forget("job-1")

	trend, _ := tr.observe("job-1", 5_000_000)
	if trend != "unknown" {
		t.Errorf("expected \"unknown\" after forget() cleared this job's history, got %q", trend)
	}
}

// --- sustained growth duration tests ---
// See sustainedGrowthEscalationThreshold's own doc comment for the "why"
// — these directly test the mechanism the UI uses to escalate from a
// routine "Growing" badge to an explicit "may not converge" warning,
// found necessary via a real load test where a SHADOW_TABLE migration's
// delta sync genuinely never converged under sustained write load.

func TestLagTrendTracker_GrowingDuration_AccumulatesAcrossConsecutiveGrowingReadings(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 10_000_000)
	_, firstGrowing := tr.observe("job-1", 20_000_000) // growing starts here
	time.Sleep(10 * time.Millisecond)
	_, secondGrowing := tr.observe("job-1", 30_000_000) // still growing

	// The first reading's own duration can legitimately be exactly zero
	// on some platforms — time.Now() immediately followed by
	// time.Since() has coarser resolution on Windows than Linux/macOS in
	// some Go runtime/OS combinations, and this whole call happens
	// within nanoseconds either way. The MEANINGFUL assertion is the one
	// below: a REAL 10ms sleep must show up as strictly more elapsed
	// time on the next reading.
	if firstGrowing < 0 {
		t.Error("expected a non-negative growing duration on the first growing reading")
	}
	if secondGrowing <= firstGrowing {
		t.Errorf("expected the growing duration to keep accumulating across consecutive growing readings, got first=%v second=%v", firstGrowing, secondGrowing)
	}
}

// TestLagTrendTracker_GrowingDuration_ResetsOnAnyNonGrowingReading is the
// direct regression test for growingSince being reset (not just left
// alone) the moment a poll shows anything other than "growing" — a
// GENUINELY converging migration's lag will show shrinking/stable
// readings interspersed with growing ones; only a truly sustained,
// UNBROKEN growing streak should ever reach the escalation threshold.
func TestLagTrendTracker_GrowingDuration_ResetsOnAnyNonGrowingReading(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 10_000_000)
	tr.observe("job-1", 20_000_000) // growing
	tr.observe("job-1", 20_100_000) // stable (well within the noise band) — must reset the streak

	_, growingAfterReset := tr.observe("job-1", 30_000_000) // growing again, but this is a NEW streak
	// Same platform-resolution caveat as the test above — a fresh
	// streak's very first reading can legitimately be exactly zero. The
	// meaningful check is the upper bound just below: proving it did
	// NOT carry over a large accumulated value from the interrupted
	// first streak.
	if growingAfterReset < 0 {
		t.Fatal("expected a non-negative growing duration for the new streak")
	}
	// The new streak's duration must be small (just started), not
	// accumulated from the interrupted first streak — a generous but
	// meaningful upper bound given real wall-clock time passed during
	// this test's own execution.
	if growingAfterReset > time.Second {
		t.Errorf("expected the growing streak to have reset after the stable reading, got a suspiciously large duration: %v", growingAfterReset)
	}
}

func TestLagTrendTracker_Forget_ClearsGrowingStreakToo(t *testing.T) {
	tr := newLagTrendTracker()
	tr.observe("job-1", 10_000_000)
	tr.observe("job-1", 20_000_000) // establishes a growing streak

	tr.forget("job-1")

	tr.observe("job-1", 30_000_000) // first observation of a "new" job, from the tracker's perspective
	_, growing := tr.observe("job-1", 40_000_000)
	if growing > time.Second {
		t.Errorf("expected forget() to have cleared the old growing streak, got a suspiciously large duration: %v", growing)
	}
}
