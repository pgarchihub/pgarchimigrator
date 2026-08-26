package api

import (
	"net/http"
	"testing"
	"time"
)

func TestLoginRateLimiter_AllowsUpToLimitThenBlocks(t *testing.T) {
	l := newLoginRateLimiter(3, time.Minute)

	for i := 0; i < 3; i++ {
		if !l.allow("1.2.3.4") {
			t.Fatalf("expected attempt %d to be allowed (limit is 3)", i+1)
		}
	}
	if l.allow("1.2.3.4") {
		t.Error("expected the 4th attempt within the window to be blocked")
	}
}

func TestLoginRateLimiter_TracksEachKeyIndependently(t *testing.T) {
	l := newLoginRateLimiter(1, time.Minute)

	if !l.allow("1.2.3.4") {
		t.Fatal("expected the first attempt from 1.2.3.4 to be allowed")
	}
	if l.allow("1.2.3.4") {
		t.Error("expected the second attempt from 1.2.3.4 to be blocked")
	}
	// A different key (e.g. a different client IP) must not be affected
	// by another key's exhausted limit — this isn't a global rate limit,
	// it's per-source.
	if !l.allow("5.6.7.8") {
		t.Error("expected the first attempt from a DIFFERENT key to still be allowed")
	}
}

func TestLoginRateLimiter_ExpiredAttemptsAreForgotten(t *testing.T) {
	l := newLoginRateLimiter(1, 10*time.Millisecond)

	if !l.allow("1.2.3.4") {
		t.Fatal("expected the first attempt to be allowed")
	}
	if l.allow("1.2.3.4") {
		t.Fatal("expected the immediate second attempt to be blocked")
	}

	time.Sleep(20 * time.Millisecond) // let the window fully expire

	if !l.allow("1.2.3.4") {
		t.Error("expected an attempt after the window expired to be allowed again")
	}
}

func TestClientIP_PrefersXForwardedFor(t *testing.T) {
	r := &http.Request{Header: make(http.Header), RemoteAddr: "10.0.0.1:12345"}
	r.Header.Set("X-Forwarded-For", "203.0.113.7")

	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("expected 203.0.113.7, got %q", got)
	}
}

func TestClientIP_TakesOnlyTheFirstEntryInAChain(t *testing.T) {
	r := &http.Request{Header: make(http.Header), RemoteAddr: "10.0.0.1:12345"}
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 10.0.0.5, 10.0.0.1")

	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("expected only the first (original client) entry, got %q", got)
	}
}

func TestClientIP_FallsBackToRemoteAddrWhenNoProxyHeader(t *testing.T) {
	r := &http.Request{Header: make(http.Header), RemoteAddr: "198.51.100.9:54321"}

	if got := clientIP(r); got != "198.51.100.9" {
		t.Errorf("expected the host portion of RemoteAddr, got %q", got)
	}
}
