package queue

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// Trace context của caller (vd span MCP call) phải nằm trong operations.trace_context.
func TestEnqueuePropagatesTraceContext(t *testing.T) {
	ctx := context.Background()
	pool := testPool(t)

	rec := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec))
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })

	ctx, parent := tp.Tracer("test").Start(ctx, "mcp call")
	id, _, err := Enqueue(ctx, pool, Job{Kind: "test.trace"})
	parent.End()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Exec(context.Background(), `DELETE FROM operations WHERE id = $1`, id) })

	var traceparent string
	pool.QueryRow(ctx, `SELECT trace_context->>'traceparent' FROM operations WHERE id = $1`, id).Scan(&traceparent)
	traceID := parent.SpanContext().TraceID().String()
	// traceparent = 00-<trace_id>-<span_id của span enqueue>-01
	if len(traceparent) != 55 || traceparent[3:35] != traceID {
		t.Fatalf("traceparent = %q, muốn trace id %s", traceparent, traceID)
	}
	spans := rec.Ended()
	var enqueue sdktrace.ReadOnlySpan
	for _, s := range spans {
		if s.Name() == "enqueue test.trace" {
			enqueue = s
		}
	}
	if enqueue == nil || enqueue.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Fatalf("thiếu span enqueue con của span MCP: %v", spans)
	}
	if traceparent[36:52] != enqueue.SpanContext().SpanID().String() {
		t.Fatalf("traceparent phải trỏ tới span enqueue: %s", traceparent)
	}
}
