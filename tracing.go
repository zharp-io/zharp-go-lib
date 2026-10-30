package zharp

import (
	"context"

	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"
)

// Logs only (Config.DisableTracing): no span is created or exported and nothing is injected into
// outgoing requests, but an incoming traceparent and baggage are still read, so the logs and
// errors of a request carry its trace_id, user and session.

// passthroughTracerProvider hands out tracers whose spans are the span already in the context —
// the extracted remote parent, or none. Starting one costs nothing and records nothing; logs
// emitted under it carry the incoming trace_id.
type passthroughTracerProvider struct {
	embedded.TracerProvider
}

func (passthroughTracerProvider) Tracer(string, ...trace.TracerOption) trace.Tracer {
	return passthroughTracer{}
}

type passthroughTracer struct {
	embedded.Tracer
}

func (passthroughTracer) Start(ctx context.Context, _ string, _ ...trace.SpanStartOption) (context.Context, trace.Span) {
	span := trace.SpanFromContext(ctx)
	return trace.ContextWithSpan(ctx, span), span
}

// extractOnly reads traceparent and baggage from incoming requests and writes nothing into
// outgoing ones.
type extractOnly struct {
	propagation.TextMapPropagator
}

func (extractOnly) Inject(context.Context, propagation.TextMapCarrier) {}
