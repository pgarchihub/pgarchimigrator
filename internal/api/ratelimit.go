package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// loginRateLimiter is a simple in-memory sliding-window limiter, keyed by
// client IP, applied only to POST /api/auth/login — the one endpoint an
// internet-facing deployment genuinely needs brute-force protection on
// (every other endpoint already requires a valid session cookie, which a
// rate limiter can't meaningfully add to).
//
// In-memory (not shared/distributed) is a deliberate, accepted trade-off:
// this whole application is already single-instance by design (see
// TR-13 and internal/state's SQLite-backed store — there is no
// multi-replica deployment story yet), so a distributed limiter would be
// pointless complexity ahead of the actual multi-instance work that
// would require anyway. If/when this becomes a multi-instance service,
// this limiter needs to move to something shared (e.g. backed by the
// same PostgreSQL-based state store TR-13 already calls for).
type loginRateLimiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limit    int
	window   time.Duration
}

func newLoginRateLimiter(limit int, window time.Duration) *loginRateLimiter {
	return &loginRateLimiter{
		attempts: make(map[string][]time.Time),
		limit:    limit,
		window:   window,
	}
}

// allow reports whether another attempt is permitted for this key right
// now, and — if so — records it. Expired attempts (older than `window`)
// are pruned on every call, so memory use for a given key never grows
// past `limit` entries and naturally shrinks once that key stops making
// requests, without needing a separate background sweep goroutine.
func (l *loginRateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-l.window)

	// Filter in place: safe because the write index never exceeds the
	// read index in this loop, so no entry is overwritten before it's
	// been read.
	kept := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}

	if len(kept) >= l.limit {
		l.attempts[key] = kept
		return false
	}
	l.attempts[key] = append(kept, now)
	return true
}

// clientIP extracts the originating client's address, preferring
// X-Forwarded-For (set by a reverse proxy — Caddy/nginx/a cloud load
// balancer — which any real internet-facing deployment of this server
// sits behind for TLS termination) over r.RemoteAddr, which would
// otherwise just be the proxy's own address for every request, making
// the rate limiter key everyone together as a single "client".
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// The first entry is the original client; anything after it
		// reflects intermediate proxies, which don't matter for this
		// purpose.
		if idx := strings.Index(xff, ","); idx != -1 {
			return strings.TrimSpace(xff[:idx])
		}
		return strings.TrimSpace(xff)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
