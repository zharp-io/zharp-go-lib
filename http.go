package zharp

import (
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Handler traces every request to h — one SERVER span each, continuing the caller's trace from
// its traceparent header, named by the ServeMux pattern ("GET /orders/{id}") — and turns a panic
// into an unhandled error report and a 500.
//
//	http.ListenAndServe(":8080", zharp.Handler(mux))
//
// It is otelhttp.NewHandler with Zharp's panic handling and http.route inside it.
func Handler(h http.Handler) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer setRoute(r)
		defer RecoverRequest(w, r)
		h.ServeHTTP(w, r)
	})
	return otelhttp.NewHandler(inner, "http.server")
}

// Transport traces outgoing requests (CLIENT spans) and sends traceparent with them:
//
//	client := &http.Client{Transport: zharp.Transport(nil)}
//
// Use the request's context (http.NewRequestWithContext) so the call joins its trace.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}

// setRoute names the span after the ServeMux pattern that matched, once routing has happened.
func setRoute(r *http.Request) {
	route := routeOf(r.Pattern)
	if route == "" {
		return
	}
	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(attribute.String("http.route", route))
	span.SetName(r.Method + " " + route)
}

// routeOf turns a Go 1.22 pattern ("GET example.com/orders/{id}") into the route ("/orders/{id}").
func routeOf(pattern string) string {
	if pattern == "" {
		return ""
	}
	if i := strings.IndexByte(pattern, ' '); i >= 0 {
		pattern = strings.TrimSpace(pattern[i+1:])
	}
	if i := strings.IndexByte(pattern, '/'); i > 0 {
		pattern = pattern[i:] // drop a host
	}
	return pattern
}
