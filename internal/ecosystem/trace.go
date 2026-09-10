package ecosystem

import (
	"context"
	"strings"
)

// ParseTraceparent parses a W3C Trace Context "traceparent" header
// (https://www.w3.org/TR/trace-context/#traceparent-header) —
// "{version}-{trace-id}-{parent-id}-{trace-flags}", e.g.
// "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01" — into this
// package's own Trace type. Returns ok=false for anything that doesn't
// match the expected 4-segment shape; deliberately lenient beyond that
// (this package doesn't validate hex-ness or exact segment lengths) —
// this is best-effort observability data, not a security boundary, so
// a slightly malformed header should degrade to "no trace attached"
// rather than rejecting the whole request that carried it.
func ParseTraceparent(header string) (trace Trace, ok bool) {
	parts := strings.Split(header, "-")
	if len(parts) != 4 {
		return Trace{}, false
	}
	traceID, spanID := parts[1], parts[2]
	if traceID == "" || spanID == "" {
		return Trace{}, false
	}
	return Trace{TraceID: traceID, SpanID: spanID}, true
}

type traceContextKey struct{}

// WithTrace returns a copy of ctx carrying trace — the ONE mechanism
// this package uses to attach W3C trace context to the NEXT event
// published using the returned context, without persisting a Trace
// field on state.Job or upgrade.Job at all.
//
// This is a deliberate scope choice, not an oversight: a traceparent
// header belongs to the ONE HTTP request that carried it — propagating
// the same parent span across a job's entire background lifecycle
// (which internal/engines/postgresql/ddlflow/internal/engines/postgresql/shadowflow/internal/engines/postgresql/upgrade.Flow all
// run under their own context.Background(), specifically so a canceled
// HTTP request context can't abort a long-running operation — see
// orchestrator.StartMigrationAsync's own doc comment) wouldn't match
// real distributed-tracing semantics anyway. So only the FIRST event a
// request's own context is used to publish (Create/CreateJob, called
// synchronously within the request) ever carries Trace; every later
// event, published from a background goroutine's own
// context.Background(), naturally carries none — exactly matching
// Store.publishForJob's own existing "empty correlation fields for a
// standalone job" precedent for the identical reasoning.
func WithTrace(ctx context.Context, trace Trace) context.Context {
	return context.WithValue(ctx, traceContextKey{}, trace)
}

// traceFromContext retrieves what WithTrace attached, or nil if nothing
// was ever attached to this context.
func traceFromContext(ctx context.Context) *Trace {
	t, ok := ctx.Value(traceContextKey{}).(Trace)
	if !ok {
		return nil
	}
	return &t
}
