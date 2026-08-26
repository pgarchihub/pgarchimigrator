package api

import "testing"

func TestIsWriteLoadCautionWorthy_BelowThreshold_ReturnsFalse(t *testing.T) {
	if isWriteLoadCautionWorthy(1_000_000) { // 1MB/s, well under the 5MB/s threshold
		t.Error("expected no caution for a modest write rate")
	}
}

func TestIsWriteLoadCautionWorthy_AtThreshold_ReturnsTrue(t *testing.T) {
	if !isWriteLoadCautionWorthy(writeLoadCautionThresholdBytesPerSecond) {
		t.Error("expected caution exactly at the threshold (inclusive bound)")
	}
}

func TestIsWriteLoadCautionWorthy_AboveThreshold_ReturnsTrue(t *testing.T) {
	if !isWriteLoadCautionWorthy(writeLoadCautionThresholdBytesPerSecond * 3) {
		t.Error("expected caution well above the threshold")
	}
}

func TestIsWriteLoadCautionWorthy_Zero_ReturnsFalse(t *testing.T) {
	if isWriteLoadCautionWorthy(0) {
		t.Error("expected no caution for a completely quiet server")
	}
}
