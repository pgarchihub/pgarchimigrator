package idempotency

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeStore is a minimal in-memory Store — real behavior, no database.
type fakeStore struct {
	mu         sync.Mutex
	records    map[string]*Record
	callsToPut int
}

func newFakeStore() *fakeStore {
	return &fakeStore{records: make(map[string]*Record)}
}

func (f *fakeStore) Get(ctx context.Context, key string) (*Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.records[key]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *r
	return &cp, nil
}

func (f *fakeStore) Put(ctx context.Context, record *Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.callsToPut++
	if _, exists := f.records[record.Key]; exists {
		return ErrAlreadyExists
	}
	cp := *record
	f.records[record.Key] = &cp
	return nil
}

func countingHandler() (http.Handler, *int) {
	calls := 0
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"id":"job-%d"}`, calls)))
	})
	return h, &calls
}

func TestMiddleware_NilStore_PassesThroughUnaffected(t *testing.T) {
	inner, calls := countingHandler()
	handler := Middleware(nil, inner)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Idempotency-Key", "key-1") // present, but nil store means it's ignored
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if *calls != 1 {
		t.Errorf("expected the handler to run normally with a nil store, got %d calls", *calls)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected the real handler's own response, got %d", rec.Code)
	}
}

func TestMiddleware_NoIdempotencyKey_PassesThroughUnaffected(t *testing.T) {
	store := newFakeStore()
	inner, calls := countingHandler()
	handler := Middleware(store, inner)

	req := httptest.NewRequest(http.MethodPost, "/", nil) // no Idempotency-Key
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if *calls != 1 {
		t.Errorf("expected the inner handler to be called once, got %d", *calls)
	}
	if rec.Header().Get("Idempotency-Replayed") != "" {
		t.Error("expected no Idempotency-Replayed header for a request without the key")
	}
}

// TestMiddleware_FirstRequest_CallsHandlerAndStoresResponse confirms
// the first request with a given key runs normally AND the response
// gets persisted for a future retry.
func TestMiddleware_FirstRequest_CallsHandlerAndStoresResponse(t *testing.T) {
	store := newFakeStore()
	inner, calls := countingHandler()
	handler := Middleware(store, inner)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if *calls != 1 {
		t.Errorf("expected the inner handler to be called once, got %d", *calls)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected 201, got %d", rec.Code)
	}

	stored, err := store.Get(context.Background(), "key-1")
	if err != nil {
		t.Fatalf("expected a record to have been stored: %v", err)
	}
	if stored.StatusCode != http.StatusCreated {
		t.Errorf("expected the stored status code to be 201, got %d", stored.StatusCode)
	}
}

// TestMiddleware_RepeatedKey_ReplaysWithoutCallingHandlerAgain is the
// direct regression test for this whole package's entire reason to
// exist — a retry with the same key must NOT re-run the handler (i.e.
// must not start a second, duplicate migration/upgrade job), and must
// get back the exact same response the first request produced.
func TestMiddleware_RepeatedKey_ReplaysWithoutCallingHandlerAgain(t *testing.T) {
	store := newFakeStore()
	inner, calls := countingHandler()
	handler := Middleware(store, inner)

	req1 := httptest.NewRequest(http.MethodPost, "/", nil)
	req1.Header.Set("Idempotency-Key", "key-2")
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	req2 := httptest.NewRequest(http.MethodPost, "/", nil)
	req2.Header.Set("Idempotency-Key", "key-2") // same key
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	if *calls != 1 {
		t.Errorf("expected the inner handler to be called exactly ONCE across both requests, got %d", *calls)
	}
	if rec2.Code != rec1.Code {
		t.Errorf("expected the replayed status code to match the original (%d), got %d", rec1.Code, rec2.Code)
	}
	if rec2.Body.String() != rec1.Body.String() {
		t.Errorf("expected the replayed body to match the original (%q), got %q", rec1.Body.String(), rec2.Body.String())
	}
	if rec2.Header().Get("Idempotency-Replayed") != "true" {
		t.Error("expected the replayed response to carry Idempotency-Replayed: true")
	}
}

func TestMiddleware_DifferentKeys_BothCallHandler(t *testing.T) {
	store := newFakeStore()
	inner, calls := countingHandler()
	handler := Middleware(store, inner)

	for _, key := range []string{"key-a", "key-b"} {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
	}

	if *calls != 2 {
		t.Errorf("expected the inner handler to be called once per distinct key (2 total), got %d", *calls)
	}
}

// TestMiddleware_StoreLookupFailure_FailsOpen confirms a Store.Get
// error (a genuine failure, not "key not found") doesn't block the
// request entirely — see Middleware's own doc comment on "fail open"
// for the reasoning.
func TestMiddleware_StoreLookupFailure_FailsOpen(t *testing.T) {
	inner, calls := countingHandler()
	handler := Middleware(failingGetStore{}, inner)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Idempotency-Key", "key-1")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if *calls != 1 {
		t.Errorf("expected the handler to still run despite the store lookup failing, got %d calls", *calls)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("expected the real handler's own response to go through, got %d", rec.Code)
	}
}

type failingGetStore struct{}

func (failingGetStore) Get(ctx context.Context, key string) (*Record, error) {
	return nil, errors.New("database connection lost")
}
func (failingGetStore) Put(ctx context.Context, record *Record) error { return nil }

// TestMiddleware_ConcurrentPutLosesRace_DoesNotFailTheRequest confirms
// the second of two concurrent first-time requests (which loses the
// Store.Put race — see Store.Put's own doc comment) still succeeds from
// its OWN caller's perspective; it already sent a real, valid response
// before Put was even called.
func TestMiddleware_ConcurrentPutLosesRace_DoesNotFailTheRequest(t *testing.T) {
	inner, _ := countingHandler()
	handler := Middleware(alwaysAlreadyExistsStore{}, inner)

	req := httptest.NewRequest(http.MethodPost, "/", nil)
	req.Header.Set("Idempotency-Key", "key-race")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Errorf("expected the request's own response to succeed despite losing the Put race, got %d", rec.Code)
	}
}

type alwaysAlreadyExistsStore struct{}

func (alwaysAlreadyExistsStore) Get(ctx context.Context, key string) (*Record, error) {
	return nil, ErrNotFound
}
func (alwaysAlreadyExistsStore) Put(ctx context.Context, record *Record) error {
	return ErrAlreadyExists
}

func TestResponseRecorder_ImplicitWriteHeader_DefaultsTo200(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("no explicit WriteHeader call"))
	})
	rec := &responseRecorder{ResponseWriter: httptest.NewRecorder(), statusCode: http.StatusOK}
	inner.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.statusCode != http.StatusOK {
		t.Errorf("expected an implicit 200 when WriteHeader is never called, got %d", rec.statusCode)
	}
	if !strings.Contains(rec.body.String(), "no explicit WriteHeader call") {
		t.Errorf("expected the body to be captured, got %q", rec.body.String())
	}
}
