// Package zharpgin traces Gin requests for Zharp. Call zharp.Init first, then:
//
//	router := gin.Default()
//	router.Use(zharpgin.Middleware())
//
// Each request gets one SERVER span (otelgin), continuing the caller's trace from traceparent and
// carrying the user and session from baggage. A panic, or an error a handler adds with c.Error
// that ends in a 5xx, is reported to Zharp as an unhandled error. Panics still reach gin.Recovery
// (or net/http) as before.
//
// With zharp.Config.DisableTracing (logs only) no span is made or sent; traceparent and baggage are
// still read, so the request's logs and errors carry its trace id, user and session.
//
// It is its own module so the core zharp module doesn't depend on Gin.
package zharpgin

import (
	"context"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"go.opentelemetry.io/contrib/instrumentation/github.com/gin-gonic/gin/otelgin"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/embedded"

	zharp "github.com/zharp-io/zharp-go-lib"
)

// Middleware returns otelgin's middleware with Zharp's error reporting. Options are passed on to
// otelgin.
func Middleware(opts ...otelgin.Option) gin.HandlerFunc {
	tp := &tracerProvider{}
	mw := otelgin.Middleware("", append([]otelgin.Option{otelgin.WithTracerProvider(tp)}, opts...)...)
	return func(c *gin.Context) {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), ginContextKey{}, c))
		mw(c)
	}
}

type ginContextKey struct{}

// otelgin ends its span with `defer span.End()` and has no panic handling: a panicking handler
// leaves the span without an error status, and nothing inside the span can see the panic. So the
// span otelgin gets is wrapped, and its End — which runs as the deferred call while the panic
// unwinds — recovers it, reports it, marks the span, and panics again.

type tracerProvider struct {
	embedded.TracerProvider
}

func (tracerProvider) Tracer(name string, opts ...trace.TracerOption) trace.Tracer {
	return tracer{Tracer: otel.GetTracerProvider().Tracer(name, opts...)}
}

type tracer struct {
	trace.Tracer
}

func (t tracer) Start(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	ctx, span := t.Tracer.Start(ctx, name, opts...)
	c, _ := ctx.Value(ginContextKey{}).(*gin.Context)
	return ctx, &requestSpan{Span: span, ctx: ctx, gin: c}
}

type requestSpan struct {
	trace.Span
	ctx context.Context
	gin *gin.Context
}

func (s *requestSpan) End(options ...trace.SpanEndOption) {
	if value := recover(); value != nil {
		if err, ok := value.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			zharp.ReportPanic(s.ctx, value)
			s.Span.SetAttributes(attribute.Int("http.response.status_code", http.StatusInternalServerError))
		}
		s.Span.End(options...)
		panic(value)
	}
	if c := s.gin; c != nil && len(c.Errors) > 0 && c.Writer.Status() >= 500 {
		zharp.ReportUnhandledError(s.ctx, c.Errors.Last().Err)
	}
	s.Span.End(options...)
}
