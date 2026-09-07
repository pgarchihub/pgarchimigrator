package ecosystem

import (
	"context"
	"testing"
)

func TestParseTraceparent_ValidHeader_Succeeds(t *testing.T) {
	trace, ok := ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	if !ok {
		t.Fatal("expected a valid traceparent header to parse successfully")
	}
	if trace.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("expected traceId '4bf92f3577b34da6a3ce929d0e0e4736', got %q", trace.TraceID)
	}
	if trace.SpanID != "00f067aa0ba902b7" {
		t.Errorf("expected spanId '00f067aa0ba902b7', got %q", trace.SpanID)
	}
}

func TestParseTraceparent_WrongSegmentCount_Fails(t *testing.T) {
	_, ok := ParseTraceparent("00-4bf92f3577b34da6a3ce929d0e0e4736")
	if ok {
		t.Error("expected a header with too few segments to fail parsing")
	}
}

func TestParseTraceparent_EmptySegment_Fails(t *testing.T) {
	_, ok := ParseTraceparent("00--00f067aa0ba902b7-01")
	if ok {
		t.Error("expected a header with an empty traceId segment to fail parsing")
	}
}

func TestParseTraceparent_EmptyString_Fails(t *testing.T) {
	_, ok := ParseTraceparent("")
	if ok {
		t.Error("expected an empty header to fail parsing")
	}
}

func TestWithTrace_RoundTrip(t *testing.T) {
	trace := Trace{TraceID: "abc123", SpanID: "def456"}
	ctx := WithTrace(context.Background(), trace)

	got := traceFromContext(ctx)
	if got == nil {
		t.Fatal("expected a non-nil trace")
	}
	if got.TraceID != "abc123" || got.SpanID != "def456" {
		t.Errorf("expected the exact trace attached, got %+v", got)
	}
}

// TestTraceFromContext_NoTraceAttached_ReturnsNil confirms a context
// that never went through WithTrace behaves as "no trace" — the common
// case (a job started standalone through this product's own CLI, or a
// background context.Background() with nothing to inherit from).
func TestTraceFromContext_NoTraceAttached_ReturnsNil(t *testing.T) {
	if traceFromContext(context.Background()) != nil {
		t.Error("expected nil for a context with no trace attached")
	}
}
