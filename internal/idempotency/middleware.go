package idempotency

import (
	"bytes"
	"errors"
	"log"
	"net/http"
)

// Middleware wraps next with Idempotency-Key handling: a request
// carrying that header is checked against store first — if a record
// already exists, the ORIGINAL response is replayed verbatim and next
// is never called again; otherwise next runs normally and its response
// is captured and stored for any future retry with the same key.
//
// A request with NO Idempotency-Key header passes through completely
// unaffected — this header is optional (AC-PF-003/AOL-STD-API-001 both
// list it as "recommended," not required), so a caller that doesn't
// send one gets exactly today's existing behavior, no different from
// before this middleware existed.
func Middleware(store Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if store == nil {
			// Idempotency support was never configured for this
			// instance — unlike RequireBearer's own nil-Service check
			// (which returns 503, since a caller EXPECTING
			// authentication to be enforced needs to know it isn't),
			// idempotency is a best-effort ADDITIONAL guarantee on top
			// of a request that already works without it — a caller
			// not getting idempotency protection should see their
			// request succeed normally, not an error about a header
			// they may not even be sending.
			next.ServeHTTP(w, r)
			return
		}
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}

		if existing, err := store.Get(r.Context(), key); err == nil {
			replay(w, existing)
			return
		} else if !errors.Is(err, ErrNotFound) {
			// A genuine store failure (not just "never seen this key
			// before") — fail open rather than blocking the request
			// entirely over an idempotency-tracking problem; the
			// underlying action itself is more important than this
			// header's own guarantee holding perfectly under a
			// database outage.
			log.Printf("idempotency: store lookup failed for key %s, proceeding without idempotency protection: %v", key, err)
			next.ServeHTTP(w, r)
			return
		}

		rec := &responseRecorder{ResponseWriter: w, statusCode: http.StatusOK}
		next.ServeHTTP(rec, r)

		record := &Record{Key: key, StatusCode: rec.statusCode, ResponseBody: rec.body.Bytes()}
		if err := store.Put(r.Context(), record); err != nil {
			if errors.Is(err, ErrAlreadyExists) {
				// Lost a race against a concurrent request carrying the
				// SAME brand-new key — see Store.Put's own doc comment.
				// The response THIS request's own handler already
				// produced and already sent to the client stands; there
				// is nothing further to correct here (the client that
				// lost the race already got a real, valid response to
				// its own request — just not necessarily byte-identical
				// to what the winning concurrent request got back).
				return
			}
			log.Printf("idempotency: failed to store record for key %s: %v", key, err)
		}
	})
}

// replay writes a previously stored Record back out verbatim.
func replay(w http.ResponseWriter, record *Record) {
	w.Header().Set("Idempotency-Replayed", "true")
	w.WriteHeader(record.StatusCode)
	_, _ = w.Write(record.ResponseBody)
}

// responseRecorder captures a handler's response (status code + body)
// while still writing it through to the real http.ResponseWriter — the
// request's own caller sees a completely normal response; this
// recorder exists purely so Middleware can ALSO persist a copy for a
// future retry.
type responseRecorder struct {
	http.ResponseWriter
	statusCode  int
	body        bytes.Buffer
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.statusCode = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}
