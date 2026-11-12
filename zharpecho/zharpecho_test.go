package zharpecho

import (
	"context"
	"errors"
	"log/slog"
	"io"
	"net/http"
	"strings"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
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

func setup(t *testing.T) (*tracetest.InMemoryExporter, *memoryLogs, *echo.Echo) {
	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "echo-unit", SpanExporter: spans, LogExporter: logs})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	e := echo.New()
	e.Use(middleware.Recover(), Middleware())
	e.GET("/orders/:id", func(c echo.Context) error {
		slog.InfoContext(c.Request().Context(), "order viewed")
		return c.String(200, "ok")
	})
	e.GET("/boom", func(c echo.Context) error { panic("boom") })
	e.GET("/fail", func(c echo.Context) error { return errors.New("database down") })
	e.GET("/missing", func(c echo.Context) error { return echo.NewHTTPError(404, "no such order") })
	return spans, logs, e
}

func serve(e *echo.Echo, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	r.Header.Set("baggage", "enduser.id=test-user,session.id=test-session")
	w := httptest.NewRecorder()
	e.ServeHTTP(w, r)
	return w
}

func server(t *testing.T, spans *tracetest.InMemoryExporter) tracetest.SpanStub {
	for _, s := range spans.GetSpans() {
		if s.SpanKind == trace.SpanKindServer {
			return s
		}
	}
	t.Fatal("no server span")
	return tracetest.SpanStub{}
}

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
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

func TestRequest(t *testing.T) {
	spans, logs, e := setup(t)
	if w := serve(e, "/orders/7"); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	s := server(t, spans)
	if s.SpanContext.TraceID().String() != traceID || attr(s, "http.route") != "/orders/:id" {
		t.Errorf("trace %s route %q", s.SpanContext.TraceID(), attr(s, "http.route"))
	}
	if attr(s, "enduser.id") != "test-user" || attr(s, "session.id") != "test-session" {
		t.Errorf("attributes %v", s.Attributes)
	}
	if len(logs.records) != 1 || logs.records[0].TraceID().String() != traceID {
		t.Errorf("logs %v", logs.records)
	}
}

func TestPanic(t *testing.T) {
	spans, logs, e := setup(t)
	if w := serve(e, "/boom"); w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d", w.Code)
	}
	if server(t, spans).Status.Code.String() != "Error" {
		t.Error("span not failed")
	}
	errs := errorLogs(logs)
	if len(errs) != 1 || errs[0]["exception.message"] != "boom" || errs[0]["zharp.handled"] != "false" || errs[0]["enduser.id"] != "test-user" {
		t.Fatalf("errors %v", errs)
	}
}

func TestReturnedErrors(t *testing.T) {
	_, logs, e := setup(t)
	if w := serve(e, "/fail"); w.Code != 500 {
		t.Fatalf("status %d", w.Code)
	}
	if w := serve(e, "/missing"); w.Code != 404 {
		t.Fatalf("status %d", w.Code)
	}
	errs := errorLogs(logs)
	if len(errs) != 1 || errs[0]["exception.message"] != "database down" || errs[0]["zharp.handled"] != "false" {
		t.Fatalf("a 5xx error is reported, a 4xx isn't: %v", errs)
	}
}

func TestLogsOnly(t *testing.T) {
	t.Setenv("ZHARP_TRACING", "false")
	spans, logs, e := setup(t)
	if w := serve(e, "/orders/7"); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if w := serve(e, "/boom"); w.Code != 500 {
		t.Fatalf("status %d", w.Code)
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
	if len(errs) != 1 || errs[0]["exception.message"] != "boom" || errs[0]["zharp.handled"] != "false" || errs[0]["enduser.id"] != "test-user" {
		t.Fatalf("errors %v", errs)
	}
}

func TestCaptureBodies(t *testing.T) {
	spans := tracetest.NewInMemoryExporter()
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "echo-unit", SpanExporter: spans, LogExporter: &memoryLogs{}, CaptureBodies: &zharp.CaptureBodies{}})
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	e := echo.New()
	e.Use(middleware.Recover(), Middleware())
	e.POST("/signup", func(c echo.Context) error {
		var in map[string]string
		_ = c.Bind(&in)
		return c.JSON(422, map[string]string{"error": "email " + in["email"] + " taken", "sessionToken": "srv-token"})
	})
	e.POST("/ok", func(c echo.Context) error { return c.String(200, "fine") })
	e.POST("/fail", func(c echo.Context) error {
		_, _ = io.ReadAll(c.Request().Body)
		return errors.New("database down")
	})

	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		e.ServeHTTP(w, r)
		return w
	}
	if w := post("/signup", `{"email":"ada@x.test","password":"pw-secret"}`); w.Code != 422 || !strings.Contains(w.Body.String(), "ada@x.test taken") {
		t.Fatalf("the app answers as before: %d %s", w.Code, w.Body)
	}
	post("/ok", `{"n":1}`)
	if w := post("/fail", `{"token":"t","q":2}`); w.Code != 500 {
		t.Fatalf("returned error: %d", w.Code)
	}

	var got []tracetest.SpanStub
	for _, s := range spans.GetSpans() {
		if s.SpanKind == trace.SpanKindServer {
			got = append(got, s)
		}
	}
	if len(got) != 3 {
		t.Fatalf("want 3 server spans, got %d", len(got))
	}
	value := func(s tracetest.SpanStub, key string) string {
		for _, kv := range s.Attributes {
			if string(kv.Key) == key {
				return kv.Value.Emit()
			}
		}
		return ""
	}
	if value(got[0], "http.request.body") != `{"email":"ada@x.test","password":"[Filtered]"}` ||
		value(got[0], "http.response.body") != `{"error":"email ada@x.test taken","sessionToken":"[Filtered]"}` {
		t.Fatalf("signup: %v", got[0].Attributes)
	}
	if value(got[1], "http.request.body") != "" {
		t.Fatalf("errors mode: a 200 carries no bodies: %v", got[1].Attributes)
	}
	// The error response is written by Echo after the span: the request body is there.
	if value(got[2], "http.request.body") != `{"token":"[Filtered]","q":2}` {
		t.Fatalf("returned error keeps the request body: %v", got[2].Attributes)
	}
}
