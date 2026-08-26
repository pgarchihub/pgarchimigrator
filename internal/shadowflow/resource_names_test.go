package shadowflow

import "testing"

// TestResourceNames_MatchesInternalNaming verifies the exported wrapper
// returns exactly what resourceNamesFor computes internally — this is the
// contract internal/reaper's SweepExpiredRollbackWindows relies on to
// recompute the same tempTable/slotName/pubName without them being
// persisted on the Job record.
func TestResourceNames_MatchesInternalNaming(t *testing.T) {
	jobID := "abc-123-def-456"
	tableName := "orders"

	wantShadow, wantTemp, wantSlot, wantPub := ResourceNames(jobID, tableName)

	internal := resourceNamesFor(jobID, tableName)
	if wantShadow != internal.shadowTable {
		t.Errorf("shadowTable mismatch: exported=%q internal=%q", wantShadow, internal.shadowTable)
	}
	if wantTemp != internal.tempTable {
		t.Errorf("tempTable mismatch: exported=%q internal=%q", wantTemp, internal.tempTable)
	}
	if wantSlot != internal.slotName {
		t.Errorf("slotName mismatch: exported=%q internal=%q", wantSlot, internal.slotName)
	}
	if wantPub != internal.pubName {
		t.Errorf("pubName mismatch: exported=%q internal=%q", wantPub, internal.pubName)
	}
}

// TestResourceNames_Deterministic verifies calling it twice with the same
// inputs always produces the same names — required for reaper to find
// resources created by a (possibly long-finished) Execute call.
func TestResourceNames_Deterministic(t *testing.T) {
	a1, b1, c1, d1 := ResourceNames("job-1", "orders")
	a2, b2, c2, d2 := ResourceNames("job-1", "orders")
	if a1 != a2 || b1 != b2 || c1 != c2 || d1 != d2 {
		t.Error("expected ResourceNames to be deterministic for the same inputs")
	}
}
