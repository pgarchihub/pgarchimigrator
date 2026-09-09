package strategy

import (
	"fmt"
	"time"
)

// maxGeneratedPartitions guards against a typo'd date range (e.g. a
// swapped year) silently generating an absurd number of partitions —
// PostgreSQL itself has no hard limit this low, but a request for
// thousands of partitions is far more likely a mistake than a genuine
// intent, and each one is a real DDL statement this project would then
// have to execute.
const maxGeneratedPartitions = 1000

// ExpandPartitionRule generates an explicit list of PartitionBound
// values from a convenience rule — e.g. interval="monthly",
// from="2024-01-01", to="2027-01-01" becomes 36 individual monthly
// partitions. RANGE partitioning only: LIST partitioning has no
// equivalent rule-based shortcut in this project, since an
// algorithmically-generated set of category values doesn't make sense
// the way a calendar interval does — LIST partitions must always be
// specified explicitly.
//
// namePrefix becomes each partition's name prefix (e.g. "orders" ->
// "orders_2024_01", "orders_2024_02", ...) — see PartitionBound.Name's
// own doc comment for how this is used downstream.
//
// This is a pure function with no database access — the caller (see
// internal/api's handling of a rule-based PARTITION_TABLE request) is
// responsible for calling this BEFORE constructing the ColumnChange, so
// engines/postgresql/ddlflow/engines/postgresql/shadowflow only ever see the final,
// explicit bounds (see ColumnChange.PartitionBoundsJSON's own doc
// comment for why).
func ExpandPartitionRule(interval, from, to, namePrefix string) ([]PartitionBound, error) {
	const dateLayout = "2006-01-02"

	fromT, err := time.Parse(dateLayout, from)
	if err != nil {
		return nil, fmt.Errorf("invalid from date %q (expected YYYY-MM-DD): %w", from, err)
	}
	toT, err := time.Parse(dateLayout, to)
	if err != nil {
		return nil, fmt.Errorf("invalid to date %q (expected YYYY-MM-DD): %w", to, err)
	}
	if !toT.After(fromT) {
		return nil, fmt.Errorf("to (%s) must be after from (%s)", to, from)
	}

	var step func(time.Time) time.Time
	var nameFormat string
	switch interval {
	case "daily":
		step = func(t time.Time) time.Time { return t.AddDate(0, 0, 1) }
		nameFormat = "20060102"
	case "monthly":
		step = func(t time.Time) time.Time { return t.AddDate(0, 1, 0) }
		nameFormat = "200601"
	case "yearly":
		step = func(t time.Time) time.Time { return t.AddDate(1, 0, 0) }
		nameFormat = "2006"
	default:
		return nil, fmt.Errorf("unknown partition interval %q — must be one of: daily, monthly, yearly", interval)
	}

	var bounds []PartitionBound
	for cur := fromT; cur.Before(toT); cur = step(cur) {
		next := step(cur)
		if next.After(toT) {
			// The final partition may be a short/partial interval if
			// `to` doesn't land exactly on an interval boundary — e.g.
			// monthly from 2024-01-01 to 2024-01-20 produces one
			// partition covering just those 19 days, rather than either
			// silently extending past `to` or dropping the remainder.
			next = toT
		}
		if len(bounds) >= maxGeneratedPartitions {
			return nil, fmt.Errorf("this rule would generate more than %d partitions — check your from/to dates and interval", maxGeneratedPartitions)
		}
		bounds = append(bounds, PartitionBound{
			Name: fmt.Sprintf("%s_%s", namePrefix, cur.Format(nameFormat)),
			From: cur.Format(dateLayout),
			To:   next.Format(dateLayout),
		})
	}
	return bounds, nil
}
