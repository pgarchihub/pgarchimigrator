package monitor

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// HostMetricsProvider is an optional interface that supplies real
// OS-level CPU usage. Genuine CPU% cannot be obtained from plain PostgreSQL
// SQL queries alone (that requires a host-level agent — e.g. node_exporter,
// gopsutil). If this interface is not wired up (nil), PgxWatcher only works
// with metrics that are genuinely measurable from the DB (connection
// percentage, IO-waiting backend ratio, lock waits) and the
// Thresholds.MaxCPUPercent check is skipped.
type HostMetricsProvider interface {
	CPUPercent(ctx context.Context) (float64, error)
}

// Snapshot carries the metrics collected in a single polling round —
// useful for tests and the audit log.
type Snapshot struct {
	ConnectionPercent  float64  // real: pg_stat_activity / max_connections
	IOWaitBackendRatio float64  // proxy: ratio of wait_event_type='IO' among state != 'idle' backends (%)
	CPUPercent         *float64 // only populated when a HostMetricsProvider is wired up
	Signal             Signal
}

// PgxWatcher is the pgx-based concrete implementation of the Watcher interface.
type PgxWatcher struct {
	Pool         *pgxpool.Pool
	Thresholds   Thresholds
	PollInterval time.Duration       // default: 5 * time.Second
	HostMetrics  HostMetricsProvider // optional, see comment above
}

var _ Watcher = (*PgxWatcher)(nil)

// NewPgxWatcher creates a PgxWatcher with the given dependencies.
func NewPgxWatcher(pool *pgxpool.Pool, thresholds Thresholds) *PgxWatcher {
	return &PgxWatcher{
		Pool:         pool,
		Thresholds:   thresholds,
		PollInterval: 5 * time.Second,
	}
}

// Start satisfies the Watcher interface: periodically collects metrics in
// the background and produces a Signal based on Thresholds. The channel is
// closed when ctx is cancelled.
func (w *PgxWatcher) Start(ctx context.Context) (<-chan Signal, error) {
	if w.PollInterval <= 0 {
		w.PollInterval = 5 * time.Second
	}

	ch := make(chan Signal, 1) // buffered so slow consumers don't block the poll loop

	go func() {
		defer close(ch)
		ticker := time.NewTicker(w.PollInterval)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				snapshot, err := w.collect(ctx)
				if err != nil {
					// If metric collection fails, err on the side of caution: PAUSE.
					// Rationale: the throttle signal exists for safety, and
					// "no data means proceed" would be a risky assumption.
					select {
					case ch <- SignalPause:
					default:
					}
					continue
				}
				select {
				case ch <- snapshot.Signal:
				default:
					// If the channel is full (slow consumer), drop the stale
					// signal and push the fresh one instead — the throttle
					// decision should stay current.
					select {
					case <-ch:
					default:
					}
					ch <- snapshot.Signal
				}
			}
		}
	}()

	return ch, nil
}

// collect runs a single polling round and produces a Signal based on Thresholds.
func (w *PgxWatcher) collect(ctx context.Context) (*Snapshot, error) {
	connPct, err := w.connectionPercent(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get connection percentage: %w", err)
	}

	ioRatio, err := w.ioWaitBackendRatio(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get IO wait ratio: %w", err)
	}

	snapshot := &Snapshot{
		ConnectionPercent:  connPct,
		IOWaitBackendRatio: ioRatio,
	}

	if w.HostMetrics != nil {
		cpu, err := w.HostMetrics.CPUPercent(ctx)
		if err != nil {
			return nil, fmt.Errorf("failed to get CPU metric: %w", err)
		}
		snapshot.CPUPercent = &cpu
	}

	snapshot.Signal = w.decide(snapshot)
	return snapshot, nil
}

// decide produces a Signal based on the FR-05 thresholds.
// PAUSE (strictest) if any threshold is exceeded, SLOW_DOWN if a threshold
// is approached (>80% of it), PROCEED otherwise.
func (w *PgxWatcher) decide(s *Snapshot) Signal {
	pause := false
	slowDown := false

	if s.ConnectionPercent >= w.Thresholds.MaxConnectionPercent {
		pause = true
	} else if s.ConnectionPercent >= w.Thresholds.MaxConnectionPercent*0.8 {
		slowDown = true
	}

	// IOWaitBackendRatio is a proxy metric (a % of IO-waiting backends, not
	// a literal ms value) — its unit does not directly match
	// MaxIOWaitMillis. When HostMetrics is not wired up, this is the only
	// "IO pressure" indicator available, and an IO-waiting backend ratio of
	// >50% is treated as roughly "high IO pressure".
	if s.IOWaitBackendRatio >= 50 {
		pause = true
	} else if s.IOWaitBackendRatio >= 30 {
		slowDown = true
	}

	if s.CPUPercent != nil {
		if *s.CPUPercent >= w.Thresholds.MaxCPUPercent {
			pause = true
		} else if *s.CPUPercent >= w.Thresholds.MaxCPUPercent*0.8 {
			slowDown = true
		}
	}

	switch {
	case pause:
		return SignalPause
	case slowDown:
		return SignalSlowDown
	default:
		return SignalProceed
	}
}

// connectionPercent computes the ratio of active connections to
// max_connections (FR-05, a real/accurate metric — not a proxy).
func (w *PgxWatcher) connectionPercent(ctx context.Context) (float64, error) {
	var pct float64
	query := `
		SELECT (count(*)::float / current_setting('max_connections')::float) * 100
		FROM pg_stat_activity
	`
	if err := w.Pool.QueryRow(ctx, query).Scan(&pct); err != nil {
		return 0, err
	}
	return pct, nil
}

// ioWaitBackendRatio computes the share of backends with
// wait_event_type='IO' among those with state != 'idle'. This is NOT an
// "average IO wait in ms" — real IO latency in ms cannot be measured with
// plain SQL; this is a pressure indicator instead.
func (w *PgxWatcher) ioWaitBackendRatio(ctx context.Context) (float64, error) {
	var ratio float64
	query := `
		SELECT
			COALESCE(
				(count(*) FILTER (WHERE wait_event_type = 'IO'))::float
				/ GREATEST(count(*), 1)::float * 100,
				0
			)
		FROM pg_stat_activity
		WHERE state != 'idle'
	`
	if err := w.Pool.QueryRow(ctx, query).Scan(&ratio); err != nil {
		return 0, err
	}
	return ratio, nil
}

// CheckLockWait implements Architecture Doc Section 3.3 "Lock Detector": it
// checks via pg_locks whether any lock request is currently pending
// (granted=false). Note: this is a general "is anything waiting on a lock
// in the system" check; the specific lock_timeout+retry mechanism used
// during the swap itself lives separately in
// internal/shadowflow/swap.go (FR-06 is handled in two layers).
func (w *PgxWatcher) CheckLockWait(ctx context.Context) (bool, error) {
	var waiting bool
	query := `SELECT EXISTS(SELECT 1 FROM pg_locks WHERE NOT granted)`
	if err := w.Pool.QueryRow(ctx, query).Scan(&waiting); err != nil {
		return false, fmt.Errorf("failed to query pg_locks: %w", err)
	}
	return waiting, nil
}
