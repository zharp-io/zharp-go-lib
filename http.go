package zharp

import (
	"context"
	"io"
	"net/http"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Handler traces every request to h — one SERVER span each, continuing the caller's trace from
// its traceparent header, named by the ServeMux pattern ("GET /orders/{id}") — and turns a panic
// into an unhandled error report and a 500.
//
//	http.ListenAndServe(":8080", zharp.Handler(mux))
//
// It is otelhttp.NewHandler with Zharp's panic handling and http.route inside it. With
// Config.DisableTracing it makes no span: it only reads traceparent and baggage, so the request's
// logs and errors carry its trace_id, user and session, and still reports panics.
func Handler(h http.Handler) http.Handler {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer setRoute(r)
		// Config.CaptureBodies: the handler reads and writes through a copy-keeping layer, and the
		// bodies go on the span once the response (a recovered panic's 500 included) is written.
		if bodies := CaptureRequestBodies(r); bodies != nil {
			w = bodies.ResponseWriter(w)
			r.Body = bodies.Request(r.Body)
			defer func() { bodies.End(trace.SpanFromContext(r.Context()), bodies.Status()) }()
		}
		defer RecoverRequest(w, r)
		h.ServeHTTP(w, r)
	})
	traced := otelhttp.NewHandler(inner, "http.server")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tracingEnabled() {
			traced.ServeHTTP(w, r)
			return
		}
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		inner.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Transport traces outgoing requests (CLIENT spans) and sends traceparent with them:
//
//	client := &http.Client{Transport: zharp.Transport(nil)}
//
// Use the request's context (http.NewRequestWithContext) so the call joins its trace. With
// Config.DisableTracing requests go straight to base: no span, no traceparent, no baggage.
func Transport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	traced := otelhttp.NewTransport(captureTransport{base})
	return roundTripper(func(r *http.Request) (*http.Response, error) {
		if !tracingEnabled() {
			return base.RoundTrip(r)
		}
		s := activeBodies()
		if s == nil || r.URL == nil || !s.allowed(r.URL.String()) {
			return traced.RoundTrip(r)
		}
		// Config.CaptureBodies: captureTransport, under otelhttp, finds this by the context.
		c := newClientBodies(s, r)
		resp, err := traced.RoundTrip(r.WithContext(context.WithValue(r.Context(), clientBodiesKey{}, c)))
		if err == nil && resp != nil && resp.Body != nil && resp.Body != http.NoBody {
			// A protocol switch's body is also a Writer; that one is left as it is.
			if _, upgraded := resp.Body.(io.ReadWriteCloser); !upgraded {
				resp.Body = &closeFirst{ReadCloser: resp.Body, c: c}
			}
		}
		return resp, err
	})
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

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
