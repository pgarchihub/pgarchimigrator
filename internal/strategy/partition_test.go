package strategy

import "testing"

func TestExpandPartitionRule_Monthly_GeneratesCorrectCount(t *testing.T) {
	bounds, err := ExpandPartitionRule("monthly", "2024-01-01", "2024-04-01", "orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bounds) != 3 {
		t.Fatalf("expected 3 monthly partitions (Jan, Feb, Mar), got %d: %+v", len(bounds), bounds)
	}
}

func TestExpandPartitionRule_Monthly_BoundsAreContiguous(t *testing.T) {
	bounds, err := ExpandPartitionRule("monthly", "2024-01-01", "2024-04-01", "orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for i := 1; i < len(bounds); i++ {
		if bounds[i].From != bounds[i-1].To {
			t.Errorf("expected contiguous bounds — partition %d's From (%s) should equal partition %d's To (%s)",
				i, bounds[i].From, i-1, bounds[i-1].To)
		}
	}
	if bounds[0].From != "2024-01-01" {
		t.Errorf("expected the first partition to start at 2024-01-01, got %s", bounds[0].From)
	}
	if bounds[len(bounds)-1].To != "2024-04-01" {
		t.Errorf("expected the last partition to end exactly at 2024-04-01, got %s", bounds[len(bounds)-1].To)
	}
}

func TestExpandPartitionRule_Yearly_GeneratesCorrectCount(t *testing.T) {
	bounds, err := ExpandPartitionRule("yearly", "2020-01-01", "2025-01-01", "events")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bounds) != 5 {
		t.Fatalf("expected 5 yearly partitions, got %d", len(bounds))
	}
}

func TestExpandPartitionRule_Daily_GeneratesCorrectCount(t *testing.T) {
	bounds, err := ExpandPartitionRule("daily", "2024-01-01", "2024-01-08", "logs")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bounds) != 7 {
		t.Fatalf("expected 7 daily partitions, got %d", len(bounds))
	}
}

// TestExpandPartitionRule_PartialFinalInterval is the direct regression
// test for a `to` date that doesn't land exactly on an interval
// boundary — the final partition must cover exactly up to `to`, not
// silently extend past it or get dropped.
func TestExpandPartitionRule_PartialFinalInterval(t *testing.T) {
	bounds, err := ExpandPartitionRule("monthly", "2024-01-01", "2024-01-20", "orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bounds) != 1 {
		t.Fatalf("expected exactly 1 (partial) partition, got %d: %+v", len(bounds), bounds)
	}
	if bounds[0].To != "2024-01-20" {
		t.Errorf("expected the partial partition to end exactly at 'to' (2024-01-20), got %s", bounds[0].To)
	}
}

func TestExpandPartitionRule_NamesIncludeThePrefix(t *testing.T) {
	bounds, err := ExpandPartitionRule("monthly", "2024-01-01", "2024-02-01", "orders")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(bounds) != 1 || bounds[0].Name != "orders_202401" {
		t.Errorf("expected name 'orders_202401', got %+v", bounds)
	}
}

func TestExpandPartitionRule_InvalidInterval_Rejected(t *testing.T) {
	if _, err := ExpandPartitionRule("weekly", "2024-01-01", "2024-02-01", "orders"); err == nil {
		t.Error("expected an error for an unsupported interval (only daily/monthly/yearly are valid)")
	}
}

func TestExpandPartitionRule_InvalidDateFormat_Rejected(t *testing.T) {
	if _, err := ExpandPartitionRule("monthly", "01/01/2024", "2024-02-01", "orders"); err == nil {
		t.Error("expected an error for a non-ISO-8601 date")
	}
}

func TestExpandPartitionRule_ToBeforeFrom_Rejected(t *testing.T) {
	if _, err := ExpandPartitionRule("monthly", "2024-06-01", "2024-01-01", "orders"); err == nil {
		t.Error("expected an error when 'to' is before 'from'")
	}
}

// TestExpandPartitionRule_ExcessivePartitionCount_Rejected is the direct
// regression test for the sanity guard against a typo'd date range
// (e.g. a swapped year) silently generating thousands of partitions.
func TestExpandPartitionRule_ExcessivePartitionCount_Rejected(t *testing.T) {
	// Daily partitions across ~4 years is well over 1000 days.
	if _, err := ExpandPartitionRule("daily", "2020-01-01", "2025-01-01", "logs"); err == nil {
		t.Error("expected an error for a rule that would generate an excessive number of partitions")
	}
}
