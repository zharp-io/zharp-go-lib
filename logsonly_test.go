package zharp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func setupLogsOnly(t *testing.T, cfg Config) (*tracetest.InMemoryExporter, *memoryLogs) {
	t.Helper()
	spans := tracetest.NewInMemoryExporter()
	logs := &memoryLogs{}
	cfg.Key, cfg.Service = "zpk_test", "go-unit"
	cfg.SpanExporter, cfg.LogExporter = spans, logs
	shutdown := Init(cfg)
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return spans, logs
}

func TestLogsOnlySendsLogsAndErrorsButNoSpans(t *testing.T) {
	spans, logs := setupLogsOnly(t, Config{DisableTracing: true})

	h := Handler(mux())
	h.ServeHTTP(httptest.NewRecorder(), request("/orders/42"))
	h.ServeHTTP(httptest.NewRecorder(), request("/caught"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, request("/boom"))
	if w.Code != 500 {
		t.Errorf("panic status %d", w.Code)
	}
	ctx, span := Tracer().Start(context.Background(), "render invoice")
	Log.Info(ctx, "inside own span")
	span.End()
	if err := Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	if n := len(spans.GetSpans()); n != 0 {
		t.Fatalf("%d spans exported with tracing off", n)
	}
	var sawLog bool
	for _, r := range logs.all() {
		if r.Body().Emit() == "order viewed" {
			sawLog = true
			a := attrs(r)
			if r.TraceID().String() != traceID || a["enduser.id"] != "test-user" || a["session.id"] != "test-session" {
				t.Errorf("log trace %s attrs %v", r.TraceID(), a)
			}
		}
		if r.Body().Emit() == "inside own span" && r.TraceID().IsValid() {
			t.Errorf("log outside a request has trace id %s", r.TraceID())
		}
	}
	if !sawLog {
		t.Error("request log not exported")
	}
	errs := errorLogs(logs)
	if len(errs) != 2 {
		t.Fatalf("%d error logs, want 2", len(errs))
	}
	handled := map[string]bool{}
	for _, e := range errs {
		a := attrs(e)
		handled[a["zharp.handled"]] = true
		if e.TraceID().String() != traceID || a["exception.type"] == "" || a["exception.message"] == "" {
			t.Errorf("error trace %s attrs %v", e.TraceID(), a)
		}
	}
	if !handled["true"] || !handled["false"] {
		t.Errorf("handled flags %v", handled)
	}
}

func TestLogsOnlyInjectsNothing(t *testing.T) {
	setupLogsOnly(t, Config{DisableTracing: true})
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()

	// A context carrying an incoming trace and baggage, as inside Handler.
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), propagation.HeaderCarrier(request("/").Header))
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := (&http.Client{Transport: Transport(nil)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("traceparent") != "" || got.Get("baggage") != "" {
		t.Errorf("injected traceparent %q baggage %q", got.Get("traceparent"), got.Get("baggage"))
	}

	// Nor through the global propagator other instrumentation uses.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) != 0 {
		t.Errorf("global propagator injected %v", carrier)
	}
}

func TestTracingOnInjectsTraceparent(t *testing.T) {
	setupLogsOnly(t, Config{})
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header.Clone() }))
	defer srv.Close()
	ctx, span := Tracer().Start(context.Background(), "outer")
	defer span.End()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := (&http.Client{Transport: Transport(nil)}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got.Get("traceparent") == "" {
		t.Error("no traceparent with tracing on")
	}
}

func TestZharpTracingEnvTurnsTracingOff(t *testing.T) {
	t.Setenv("ZHARP_TRACING", "false")
	spans, logs := setupLogsOnly(t, Config{})
	Handler(mux()).ServeHTTP(httptest.NewRecorder(), request("/orders/42"))
	if n := len(spans.GetSpans()); n != 0 {
		t.Errorf("%d spans with ZHARP_TRACING=false", n)
	}
	if len(logs.all()) == 0 {
		t.Error("no logs with ZHARP_TRACING=false")
	}
}
