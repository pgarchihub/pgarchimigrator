// Package monitor implements Architecture Doc Section 3.3 "Performance
// Monitor" and "Lock Detector". Works with the concrete thresholds from
// Requirements Doc FR-05/FR-06.
package monitor

import "context"

// Thresholds match the default values from FR-05; all are configurable.
type Thresholds struct {
	MaxCPUPercent        float64 // default: 70.0
	MaxIOWaitMillis      float64 // default: 500.0
	MaxConnectionPercent float64 // default: 80.0 (relative to max_connections)
}

func DefaultThresholds() Thresholds {
	return Thresholds{
		MaxCPUPercent:        70.0,
		MaxIOWaitMillis:      500.0,
		MaxConnectionPercent: 80.0,
	}
}

// Signal is the throttle signal sent to the SyncEngine/Apply Engine
// (ddlflow, shadowflow).
type Signal string

const (
	SignalProceed  Signal = "PROCEED"
	SignalSlowDown Signal = "SLOW_DOWN"
	SignalPause    Signal = "PAUSE"
)

// Watcher periodically collects system metrics and produces a Signal based
// on Thresholds. It also performs the "Lock Detector" role via pg_locks.
type Watcher interface {
	// Start begins collecting metrics in the background and returns the
	// Signal channel.
	Start(ctx context.Context) (<-chan Signal, error)
	// CheckLockWait checks via pg_locks whether the migration is currently
	// blocking on another query (complements the lock_timeout mechanism in
	// swap.go for FR-06 — this covers general sync steps; lock handling
	// during the swap itself lives separately in
	// internal/shadowflow/swap.go).
	CheckLockWait(ctx context.Context) (bool, error)
}
