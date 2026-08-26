package api

import "testing"

func TestImpactTracker_FirstObservation_ReturnsItself(t *testing.T) {
	tr := newImpactTracker()
	if got := tr.observe("job-1", 2.5); got != 2.5 {
		t.Errorf("expected the first observation itself, got %v", got)
	}
}

// TestImpactTracker_TracksRunningPeak_NotJustLatestReading is the direct
// regression test for why this tracker exists at all — a single
// instantaneous snapshot on each poll could easily miss a spike that
// happened between two polls; the running peak keeps the worst reading
// seen across the whole migration, not just whatever the latest poll
// happens to show.
func TestImpactTracker_TracksRunningPeak_NotJustLatestReading(t *testing.T) {
	tr := newImpactTracker()
	tr.observe("job-1", 5.0) // a spike
	got := tr.observe("job-1", 0.5)
	if got != 5.0 {
		t.Errorf("expected the tracker to keep reporting the peak (5.0) even after a lower reading, got %v", got)
	}
}

func TestImpactTracker_UpdatesPeakWhenExceeded(t *testing.T) {
	tr := newImpactTracker()
	tr.observe("job-1", 2.0)
	got := tr.observe("job-1", 4.0)
	if got != 4.0 {
		t.Errorf("expected the peak to update to a genuinely higher reading, got %v", got)
	}
}

func TestImpactTracker_TracksMultipleJobsIndependently(t *testing.T) {
	tr := newImpactTracker()
	tr.observe("job-1", 10.0)
	tr.observe("job-2", 1.0)

	got1 := tr.observe("job-1", 3.0) // still below job-1's own peak
	got2 := tr.observe("job-2", 6.0) // a new peak for job-2

	if got1 != 10.0 {
		t.Errorf("job-1: expected peak to remain 10.0, got %v", got1)
	}
	if got2 != 6.0 {
		t.Errorf("job-2: expected peak to update to 6.0, got %v", got2)
	}
}

func TestImpactTracker_Forget_ResetsPeakOnNextObservation(t *testing.T) {
	tr := newImpactTracker()
	tr.observe("job-1", 8.0)

	tr.forget("job-1")

	got := tr.observe("job-1", 1.0)
	if got != 1.0 {
		t.Errorf("expected forget() to have cleared the old peak, got %v", got)
	}
}
