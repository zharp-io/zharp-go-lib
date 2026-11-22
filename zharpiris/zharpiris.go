// Package zharpiris traces Iris (v12) requests for Zharp. Call zharp.Init first, then:
//
//	app := iris.Default()                 // or iris.New() with app.UseRouter(recover.New())
//	app.UseRouter(zharpiris.Middleware()) // every request, 404s included; after recover
//
// Each request gets one SERVER span named by its route ("GET /orders/{id}"), continuing the
// caller's trace from traceparent and carrying the user and session from baggage, with the
// request's and response's headers (secrets [Filtered]) and, with zharp.Config.CaptureBodies, its
// bodies. A panic, or an error a handler stops with (ctx.StopWithError) that ends in a 5xx, is
// reported to Zharp as an unhandled error. Panics still reach Iris's recover middleware (or
// net/http) as before.
//
// The iris.Context carries the request's span: pass ctx to slog.InfoContext, zharp.CaptureError
// and outgoing calls so they join the request.
//
// With zharp.Config.DisableTracing (logs only) no span is made or sent; traceparent and baggage are
// still read, so the request's logs and errors carry its trace id, user and session.
//
// It is its own module so the core zharp module doesn't depend on Iris.
package zharpiris

import (
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/kataras/iris/v12"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	zharp "github.com/zharp-io/zharp-go-lib"
)

const scope = "github.com/zharp-io/zharp-go-lib/zharpiris"

// Version is this module's version.
const Version = "0.1.0"

// Middleware traces every request that reaches it. Register it with app.UseRouter (every request)
// or app.UseGlobal (matched routes and registered error handlers).
func Middleware() iris.Handler {
	return func(ctx iris.Context) {
		r := ctx.Request()
		parent := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		// Named by the route once routing has happened; the method alone if nothing matched.
		spanCtx, span := otel.Tracer(scope, trace.WithInstrumentationVersion(Version)).Start(parent, r.Method,
			trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(requestAttributes(r)...))
		r = r.WithContext(spanCtx)
		ctx.ResetRequest(r)

		// zharp.Config.CaptureBodies: the handlers read and write through a copy-keeping layer.
		bodies := zharp.CaptureRequestBodies(r)
		if bodies != nil {
			body, writer := r.Body, ctx.ResponseWriter()
			underline := writer.Naive()
			r.Body = bodies.Request(body)
			writer.SetWriter(bodies.ResponseWriter(underline))
			defer func() { r.Body = body; writer.SetWriter(underline) }()
		}

		defer func() {
			if value := recover(); value != nil {
				if err, ok := value.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
					zharp.ReportPanic(spanCtx, value)
				}
				end(ctx, span, r, bodies, http.StatusInternalServerError, true)
				panic(value)
			}
		}()

		ctx.Next()

		status := ctx.GetStatusCode()
		reported := false
		if err := ctx.GetErr(); err != nil && status >= 500 {
			zharp.ReportUnhandledError(spanCtx, err)
			reported = true
		}
		end(ctx, span, r, bodies, status, reported)
	}
}

// end puts the route and the response on the span and ends it.
func end(ctx iris.Context, span trace.Span, r *http.Request, bodies *zharp.RequestBodies, status int, reported bool) {
	defer span.End()
	if !span.IsRecording() {
		return
	}
	defer func() { _ = recover() }() // never break a request over its telemetry
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	if route := ctx.GetCurrentRoute(); route != nil && route.Path() != "" {
		span.SetAttributes(attribute.String("http.route", route.Path()))
		span.SetName(r.Method + " " + route.Path())
	}
	if status >= 500 && !reported {
		span.SetStatus(codes.Error, "")
	}
	bodies.End(span, status)
	zharp.RecordHeaders(span, r.Header, ctx.ResponseWriter().Header())
}

// requestAttributes are the HTTP server attributes OpenTelemetry's net/http instrumentation sets.
func requestAttributes(r *http.Request) []attribute.KeyValue {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", r.Method),
		attribute.String("url.scheme", scheme),
		attribute.String("url.path", r.URL.Path),
	}
	if host, port := splitHostPort(r.Host); host != "" {
		attrs = append(attrs, attribute.String("server.address", host))
		if port > 0 {
			attrs = append(attrs, attribute.Int("server.port", port))
		}
	}
	if r.URL.RawQuery != "" {
		attrs = append(attrs, attribute.String("url.query", r.URL.RawQuery))
	}
	peer, peerPort := splitHostPort(r.RemoteAddr)
	if peer != "" {
		attrs = append(attrs, attribute.String("network.peer.address", peer))
		if peerPort > 0 {
			attrs = append(attrs, attribute.Int("network.peer.port", peerPort))
		}
	}
	client := peer
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		client = strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if client != "" {
		attrs = append(attrs, attribute.String("client.address", client))
	}
	if ua := r.UserAgent(); ua != "" {
		attrs = append(attrs, attribute.String("user_agent.original", ua))
	}
	if r.ProtoMajor > 0 {
		v := strconv.Itoa(r.ProtoMajor)
		if r.ProtoMajor == 1 {
			v += "." + strconv.Itoa(r.ProtoMinor)
		}
		attrs = append(attrs, attribute.String("network.protocol.version", v))
	}
	return attrs
}

func splitHostPort(hostport string) (string, int) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, -1
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return host, -1
	}
	return host, p
}
