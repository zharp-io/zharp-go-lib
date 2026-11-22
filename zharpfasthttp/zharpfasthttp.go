// Package zharpfasthttp traces fasthttp requests for Zharp. Call zharp.Init first, then wrap the
// server's handler:
//
//	fasthttp.ListenAndServe(":8080", zharpfasthttp.Handler(handler))
//
// Each request gets one SERVER span, continuing the caller's trace from traceparent and carrying
// the user and session from baggage, with the request's and response's headers (secrets
// [Filtered]) and, with zharp.Config.CaptureBodies, its bodies. A panic is reported to Zharp as an
// unhandled error and answered with a 500 — fasthttp has no recovery of its own, so the panic
// stops here rather than ending the process (as zharp.Handler does for net/http).
//
// fasthttp has no router, so the span is named by method and path ("GET /orders/7"). If a router
// knows the route template, pass it with WithRoute and the span is named by it instead
// ("GET /orders/{id}") — what Zharp groups endpoints by.
//
// A *fasthttp.RequestCtx is a context.Context, but not one that carries the span: pass
// Context(ctx) to slog.InfoContext, zharp.CaptureError and outgoing calls so they join the request.
//
// With zharp.Config.DisableTracing (logs only) no span is made or sent; traceparent and baggage are
// still read, so the request's logs and errors carry its trace id, user and session.
//
// It is its own module so the core zharp module doesn't depend on fasthttp.
package zharpfasthttp

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	zharp "github.com/zharp-io/zharp-go-lib"
)

const scope = "github.com/zharp-io/zharp-go-lib/zharpfasthttp"

// Version is this module's version.
const Version = "0.1.0"

// Option configures Handler.
type Option func(*config)

type config struct {
	route func(*fasthttp.RequestCtx) string
}

// WithRoute gives the route template the request matched ("/orders/{id}"), read once the handler
// has run; return "" when there is none. It becomes http.route and names the span. With
// github.com/fasthttp/router, set SaveMatchedRoutePath and read it back:
//
//	r := router.New()
//	r.SaveMatchedRoutePath = true
//	handler := zharpfasthttp.Handler(r.Handler, zharpfasthttp.WithRoute(func(ctx *fasthttp.RequestCtx) string {
//		route, _ := ctx.UserValue(router.MatchedRoutePathParam).(string)
//		return route
//	}))
func WithRoute(route func(ctx *fasthttp.RequestCtx) string) Option {
	return func(c *config) { c.route = route }
}

type contextKey struct{}

// Context is the request's context: its span, trace id, user and session. Use it wherever the
// request's code takes a context.Context. Outside a request traced by Handler it is ctx itself.
func Context(ctx *fasthttp.RequestCtx) context.Context {
	if c, ok := ctx.UserValue(contextKey{}).(context.Context); ok {
		return c
	}
	return ctx
}

// SetContext replaces the request's context, for code that adds to it (zharp.SetUser) and wants
// what runs after to see it.
func SetContext(ctx *fasthttp.RequestCtx, c context.Context) {
	ctx.SetUserValue(contextKey{}, c)
}

// Handler traces every request to h.
func Handler(h fasthttp.RequestHandler, opts ...Option) fasthttp.RequestHandler {
	cfg := &config{}
	for _, opt := range opts {
		opt(cfg)
	}
	return func(rc *fasthttp.RequestCtx) {
		req := httpRequest(rc)
		// The parent is Background, not rc: rc is reused once the handler returns, and a context
		// handed to a goroutine must not outlive it.
		ctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.HeaderCarrier(req.Header))
		ctx, span := otel.Tracer(scope, trace.WithInstrumentationVersion(Version)).Start(ctx, req.Method+" "+req.URL.Path,
			trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(requestAttributes(rc, req)...))
		rc.SetUserValue(contextKey{}, ctx)
		bodies := zharp.CaptureRequestBodies(req)

		defer func() {
			failed := false
			if value := recover(); value != nil {
				zharp.ReportPanic(Context(rc), value)
				failed = true
				rc.Error(http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
			}
			end(rc, span, req, bodies, cfg.route, failed)
			rc.RemoveUserValue(contextKey{})
		}()
		h(rc)
	}
}

// end puts the response on the span and ends it.
func end(rc *fasthttp.RequestCtx, span trace.Span, req *http.Request, bodies *zharp.RequestBodies, route func(*fasthttp.RequestCtx) string, reported bool) {
	defer span.End()
	if !span.IsRecording() {
		return
	}
	defer func() { _ = recover() }() // never break a request over its telemetry
	status := rc.Response.StatusCode()
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	if route != nil {
		if r := route(rc); r != "" {
			span.SetAttributes(attribute.String("http.route", r))
			span.SetName(req.Method + " " + r)
		}
	}
	if status >= 500 && !reported {
		span.SetStatus(codes.Error, "")
	}
	response := responseHeader(&rc.Response.Header)
	if bodies != nil {
		// fasthttp has the whole request body in memory: it goes through the copy-keeping reader
		// now, once the handler is done with it.
		// A streamed body (Server.StreamRequestBody, SetBodyStream) is left alone, on either
		// side: reading it here would consume it.
		if !rc.Request.IsBodyStream() {
			readAll(bodies.Request(io.NopCloser(bytes.NewReader(rc.Request.Body()))))
		}
		if !rc.Response.IsBodyStream() {
			bodies.Wrote(rc.Response.Body(), response)
		}
		bodies.End(span, status)
	}
	zharp.RecordHeaders(span, req.Header, response)
}

func readAll(r io.Reader) { _, _ = io.Copy(io.Discard, r) }

// httpRequest is the request as net/http would see it, for the propagator, the header and body
// capture. Every value is copied: fasthttp reuses its buffers.
func httpRequest(rc *fasthttp.RequestCtx) *http.Request {
	header := make(http.Header)
	rc.Request.Header.VisitAll(func(k, v []byte) {
		header.Add(string(k), string(v))
	})
	u := &url.URL{Path: string(rc.Path()), RawQuery: string(rc.URI().QueryString())}
	req := &http.Request{
		Method:     string(rc.Method()),
		URL:        u,
		Proto:      string(rc.Request.Header.Protocol()),
		Header:     header,
		Host:       string(rc.Host()),
		RemoteAddr: rc.RemoteAddr().String(),
		RequestURI: string(rc.RequestURI()),
	}
	if rc.IsTLS() {
		req.TLS = rc.TLSConnectionState()
	}
	return req
}

func responseHeader(h *fasthttp.ResponseHeader) http.Header {
	header := make(http.Header)
	h.VisitAll(func(k, v []byte) {
		header.Add(string(k), string(v))
	})
	return header
}

// requestAttributes are the HTTP server attributes OpenTelemetry's net/http instrumentation sets.
func requestAttributes(rc *fasthttp.RequestCtx, req *http.Request) []attribute.KeyValue {
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", req.Method),
		attribute.String("url.scheme", scheme),
		attribute.String("url.path", req.URL.Path),
	}
	if host, port := splitHostPort(req.Host); host != "" {
		attrs = append(attrs, attribute.String("server.address", host))
		if port > 0 {
			attrs = append(attrs, attribute.Int("server.port", port))
		}
	}
	if req.URL.RawQuery != "" {
		attrs = append(attrs, attribute.String("url.query", req.URL.RawQuery))
	}
	peer, peerPort := splitHostPort(req.RemoteAddr)
	if peer != "" {
		attrs = append(attrs, attribute.String("network.peer.address", peer))
		if peerPort > 0 {
			attrs = append(attrs, attribute.Int("network.peer.port", peerPort))
		}
	}
	client := peer
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		client = strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if client != "" {
		attrs = append(attrs, attribute.String("client.address", client))
	}
	if ua := req.Header.Get("User-Agent"); ua != "" {
		attrs = append(attrs, attribute.String("user_agent.original", ua))
	}
	if v := strings.TrimPrefix(req.Proto, "HTTP/"); v != "" && v != req.Proto {
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
