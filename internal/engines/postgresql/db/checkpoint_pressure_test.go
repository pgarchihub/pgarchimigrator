package db

import "testing"

func TestIsCheckpointPressured_RequestedExceedsTimedPastThreshold_ReturnsTrue(t *testing.T) {
	if !isCheckpointPressured(10, 3) {
		t.Error("expected pressured=true when requested (10) exceeds timed (3) and is past the minimum threshold")
	}
}

func TestIsCheckpointPressured_TimedExceedsRequested_ReturnsFalse(t *testing.T) {
	if isCheckpointPressured(3, 50) {
		t.Error("expected pressured=false when timed checkpoints dominate — the healthy, normal case")
	}
}

// TestIsCheckpointPressured_FewRequestedCheckpoints_ReturnsFalseEvenIfHigherThanTimed
// is the direct regression test for the minimum-threshold reasoning in
// isCheckpointPressured's own doc comment: a freshly-started or very
// quiet server can have a couple of requested checkpoints against zero
// timed ones without that being a meaningful signal of anything.
func TestIsCheckpointPressured_FewRequestedCheckpoints_ReturnsFalseEvenIfHigherThanTimed(t *testing.T) {
	if isCheckpointPressured(2, 0) {
		t.Error("expected pressured=false below the minimum requested-count threshold, even though requested > timed")
	}
}

func TestIsCheckpointPressured_EqualCounts_ReturnsFalse(t *testing.T) {
	if isCheckpointPressured(10, 10) {
		t.Error("expected pressured=false when requested does not exceed timed")
	}
}

func TestIsCheckpointPressured_ZeroZero_ReturnsFalse(t *testing.T) {
	if isCheckpointPressured(0, 0) {
		t.Error("expected pressured=false for a server with no checkpoint activity yet")
	}
}
