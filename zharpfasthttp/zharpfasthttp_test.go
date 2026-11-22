package zharpfasthttp

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	zharp "github.com/zharp-io/zharp-go-lib"
)

const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

type memoryLogs struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (m *memoryLogs) Export(_ context.Context, records []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range records {
		m.records = append(m.records, r.Clone())
	}
	return nil
}
func (m *memoryLogs) Shutdown(context.Context) error   { return nil }
func (m *memoryLogs) ForceFlush(context.Context) error { return nil }

func attrs(r sdklog.Record) map[string]string {
	out := map[string]string{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value.Emit()
		return true
	})
	return out
}

func errorLogs(logs *memoryLogs) []map[string]string {
	var out []map[string]string
	for _, r := range logs.records {
		if a := attrs(r); a["exception.type"] != "" {
			out = append(out, a)
		}
	}
	return out
}

func serverSpans(spans *tracetest.InMemoryExporter) []tracetest.SpanStub {
	var out []tracetest.SpanStub
	for _, s := range spans.GetSpans() {
		if s.SpanKind == trace.SpanKindServer {
			out = append(out, s)
		}
	}
	return out
}

func server(t *testing.T, spans *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	got := serverSpans(spans)
	if len(got) != 1 {
		t.Fatalf("want one server span, got %d", len(got))
	}
	return got[0]
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

// app routes by hand, the way a plain fasthttp server does.
func app(rc *fasthttp.RequestCtx) {
	path := string(rc.Path())
	switch {
	case strings.HasPrefix(path, "/orders/"):
		slog.InfoContext(Context(rc), "order viewed")
		rc.SetUserValue("route", "/orders/{id}")
		rc.SetBodyString("ok")
	case path == "/boom":
		panic("boom")
	case path == "/fail":
		zharp.CaptureError(Context(rc), errFail)
		rc.SetStatusCode(503)
	case path == "/signup":
		rc.SetContentType("application/json")
		rc.SetStatusCode(422)
		rc.SetBodyString(`{"error":"taken","sessionToken":"srv-token"}`)
	default:
		rc.SetStatusCode(404)
	}
}

type failError struct{}

func (failError) Error() string { return "database down" }

var errFail = failError{}

func setup(t *testing.T, config zharp.Config) (*tracetest.InMemoryExporter, *memoryLogs) {
	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	config.Key, config.Service, config.SpanExporter, config.LogExporter, config.Logs = "zpk_test", "fasthttp-unit", spans, logs, zharp.Bool(true)
	shutdown := zharp.Init(config)
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return spans, logs
}

func serve(h fasthttp.RequestHandler, method, uri, body string) *fasthttp.RequestCtx {
	var req fasthttp.Request
	req.Header.SetMethod(method)
	req.SetRequestURI(uri)
	req.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	req.Header.Set("baggage", "enduser.id=test-user,session.id=test-session")
	req.Header.Set("Authorization", "Bearer sk-live-secret")
	if body != "" {
		req.Header.SetContentType("application/json")
		req.SetBodyString(body)
	}
	rc := &fasthttp.RequestCtx{}
	rc.Init(&req, &net.TCPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5000}, nil)
	h(rc)
	return rc
}

func TestRequest(t *testing.T) {
	spans, logs := setup(t, zharp.Config{})
	rc := serve(Handler(app), "GET", "http://shop.test/orders/7?view=full", "")
	if rc.Response.StatusCode() != 200 || string(rc.Response.Body()) != "ok" {
		t.Fatalf("response %d %q", rc.Response.StatusCode(), rc.Response.Body())
	}
	s := server(t, spans)
	if s.Name != "GET /orders/7" || attr(s, "http.route") != "" {
		t.Errorf("name %q route %q: no router, no route", s.Name, attr(s, "http.route"))
	}
	if s.SpanContext.TraceID().String() != traceID || s.Parent.SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("trace %s parent %s", s.SpanContext.TraceID(), s.Parent.SpanID())
	}
	for key, want := range map[string]string{
		"http.request.method": "GET", "url.path": "/orders/7", "url.query": "view=full", "server.address": "shop.test",
		"http.response.status_code": "200", "client.address": "10.0.0.1", "enduser.id": "test-user", "session.id": "test-session",
		"http.request.header.authorization": "[Filtered]", "http.response.header.content-type": "text/plain; charset=utf-8",
	} {
		if got := attr(s, key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if s.Status.Code.String() != "Unset" {
		t.Errorf("status %v", s.Status)
	}
	if len(logs.records) != 1 || logs.records[0].TraceID().String() != traceID || logs.records[0].SpanID() != s.SpanContext.SpanID() {
		t.Errorf("logs %v", logs.records)
	}
	if rc.UserValue(contextKey{}) != nil {
		t.Error("the context is left on the reused RequestCtx")
	}
}

func TestWithRoute(t *testing.T) {
	spans, _ := setup(t, zharp.Config{})
	h := Handler(app, WithRoute(func(rc *fasthttp.RequestCtx) string {
		route, _ := rc.UserValue("route").(string)
		return route
	}))
	serve(h, "GET", "/orders/7", "")
	serve(h, "GET", "/nope", "")
	got := serverSpans(spans)
	if len(got) != 2 {
		t.Fatalf("%d spans", len(got))
	}
	if got[0].Name != "GET /orders/{id}" || attr(got[0], "http.route") != "/orders/{id}" {
		t.Errorf("name %q route %q", got[0].Name, attr(got[0], "http.route"))
	}
	if got[1].Name != "GET /nope" || attr(got[1], "http.route") != "" {
		t.Errorf("no route: name %q route %q", got[1].Name, attr(got[1], "http.route"))
	}
}

func TestPanic(t *testing.T) {
	spans, logs := setup(t, zharp.Config{})
	rc := serve(Handler(app), "GET", "/boom", "")
	if rc.Response.StatusCode() != 500 {
		t.Fatalf("status %d", rc.Response.StatusCode())
	}
	s := server(t, spans)
	if s.Status.Code.String() != "Error" || attr(s, "http.response.status_code") != "500" {
		t.Errorf("status %v code %q", s.Status, attr(s, "http.response.status_code"))
	}
	errs := errorLogs(logs)
	if len(errs) != 1 || errs[0]["exception.message"] != "boom" || errs[0]["zharp.handled"] != "false" || errs[0]["session.id"] != "test-session" {
		t.Fatalf("errors %v", errs)
	}
	if !strings.Contains(errs[0]["exception.stacktrace"], "zharpfasthttp.app") {
		t.Errorf("stack doesn't start at the handler: %s", errs[0]["exception.stacktrace"])
	}
}

func TestServerError(t *testing.T) {
	spans, logs := setup(t, zharp.Config{})
	if rc := serve(Handler(app), "GET", "/fail", ""); rc.Response.StatusCode() != 503 {
		t.Fatalf("status %d", rc.Response.StatusCode())
	}
	if s := server(t, spans); s.Status.Code.String() != "Error" {
		t.Errorf("a 5xx fails the span: %v", s.Status)
	}
	if errs := errorLogs(logs); len(errs) != 1 || errs[0]["exception.message"] != "database down" {
		t.Fatalf("errors %v", errs)
	}
}

func TestLogsOnly(t *testing.T) {
	t.Setenv("ZHARP_TRACING", "false")
	spans, logs := setup(t, zharp.Config{})
	serve(Handler(app), "GET", "/orders/7", "")
	if rc := serve(Handler(app), "GET", "/boom", ""); rc.Response.StatusCode() != 500 {
		t.Fatalf("status %d", rc.Response.StatusCode())
	}
	if n := len(spans.GetSpans()); n != 0 {
		t.Fatalf("%d spans with tracing off", n)
	}
	var sawLog bool
	for _, r := range logs.records {
		if r.Body().Emit() == "order viewed" {
			sawLog = r.TraceID().String() == traceID && attrs(r)["session.id"] == "test-session"
		}
	}
	if !sawLog {
		t.Errorf("request log missing or without trace/session: %v", logs.records)
	}
	errs := errorLogs(logs)
	if len(errs) != 1 || errs[0]["exception.message"] != "boom" || errs[0]["enduser.id"] != "test-user" {
		t.Fatalf("errors %v", errs)
	}
}

func TestCaptureBodies(t *testing.T) {
	spans, _ := setup(t, zharp.Config{CaptureBodies: &zharp.CaptureBodies{}})
	rc := serve(Handler(app), "POST", "/signup", `{"email":"ada@x.test","password":"pw-secret"}`)
	if rc.Response.StatusCode() != 422 || !strings.Contains(string(rc.Response.Body()), "srv-token") {
		t.Fatalf("the app answers as before: %d %s", rc.Response.StatusCode(), rc.Response.Body())
	}
	serve(Handler(app), "POST", "/orders/1", `{"n":1}`)
	got := serverSpans(spans)
	if len(got) != 2 {
		t.Fatalf("%d spans", len(got))
	}
	if attr(got[0], "http.request.body") != `{"email":"ada@x.test","password":"[Filtered]"}` ||
		attr(got[0], "http.response.body") != `{"error":"taken","sessionToken":"[Filtered]"}` {
		t.Fatalf("signup: %v", got[0].Attributes)
	}
	if attr(got[1], "http.request.body") != "" {
		t.Fatalf("errors mode: a 200 carries no bodies: %v", got[1].Attributes)
	}
}
