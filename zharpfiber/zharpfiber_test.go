package zharpfiber

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/gofiber/fiber/v2/middleware/recover"
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

func setup(t *testing.T, config zharp.Config) (*tracetest.InMemoryExporter, *memoryLogs, *fiber.App) {
	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	config.Key, config.Service, config.SpanExporter, config.LogExporter, config.Logs = "zpk_test", "fiber-unit", spans, logs, zharp.Bool(true)
	shutdown := zharp.Init(config)
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	app := fiber.New()
	app.Use(recover.New(), Middleware())
	app.Get("/orders/:id", func(c *fiber.Ctx) error {
		slog.InfoContext(c.UserContext(), "order viewed")
		return c.SendString("ok")
	})
	api := app.Group("/api")
	api.Get("/users/:id/orders", func(c *fiber.Ctx) error { return c.SendString("[]") })
	app.Get("/boom", func(c *fiber.Ctx) error { panic("boom") })
	app.Get("/fail", func(c *fiber.Ctx) error { return errors.New("database down") })
	app.Get("/gone", func(c *fiber.Ctx) error { return fiber.NewError(410, "gone") })
	app.Post("/signup", func(c *fiber.Ctx) error {
		var in map[string]string
		_ = c.BodyParser(&in)
		return c.Status(422).JSON(fiber.Map{"error": "email " + in["email"] + " taken", "sessionToken": "srv-token"})
	})
	app.Post("/ok", func(c *fiber.Ctx) error { return c.SendString("fine") })
	return spans, logs, app
}

func serve(t *testing.T, app *fiber.App, method, path, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	r.Header.Set("baggage", "enduser.id=test-user,session.id=test-session")
	r.Header.Set("Authorization", "Bearer sk-live-secret")
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	resp, err := app.Test(r, -1)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestRequest(t *testing.T) {
	spans, logs, app := setup(t, zharp.Config{})
	if code, body := serve(t, app, "GET", "/orders/7?view=full", ""); code != 200 || body != "ok" {
		t.Fatalf("response %d %q", code, body)
	}
	s := server(t, spans)
	if s.Name != "GET /orders/:id" || attr(s, "http.route") != "/orders/:id" {
		t.Errorf("name %q route %q", s.Name, attr(s, "http.route"))
	}
	if s.SpanContext.TraceID().String() != traceID || s.Parent.SpanID().String() != "00f067aa0ba902b7" {
		t.Errorf("trace %s parent %s", s.SpanContext.TraceID(), s.Parent.SpanID())
	}
	for key, want := range map[string]string{
		"http.request.method": "GET", "url.path": "/orders/7", "url.query": "view=full", "http.response.status_code": "200",
		"enduser.id": "test-user", "session.id": "test-session", "http.request.header.authorization": "[Filtered]",
		"http.response.header.content-type": "text/plain; charset=utf-8",
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
}

func TestRoutes(t *testing.T) {
	spans, _, app := setup(t, zharp.Config{})
	serve(t, app, "GET", "/api/users/3/orders", "")
	serve(t, app, "GET", "/nope", "")
	serve(t, app, "DELETE", "/orders/7", "")
	got := serverSpans(spans)
	if len(got) != 3 {
		t.Fatalf("%d spans", len(got))
	}
	if got[0].Name != "GET /api/users/:id/orders" {
		t.Errorf("group route: %q", got[0].Name)
	}
	for i, code := range []string{"404", "405"} {
		s := got[i+1]
		if attr(s, "http.route") != "" || strings.Contains(s.Name, "/") || attr(s, "http.response.status_code") != code {
			t.Errorf("no route matched: name %q route %q status %s", s.Name, attr(s, "http.route"), attr(s, "http.response.status_code"))
		}
	}
}

func TestPanic(t *testing.T) {
	spans, logs, app := setup(t, zharp.Config{})
	if code, _ := serve(t, app, "GET", "/boom", ""); code != 500 {
		t.Fatalf("status %d (recover.New should still answer)", code)
	}
	s := server(t, spans)
	if s.Name != "GET /boom" || s.Status.Code.String() != "Error" || attr(s, "http.response.status_code") != "500" {
		t.Errorf("name %q status %v code %q", s.Name, s.Status, attr(s, "http.response.status_code"))
	}
	errs := errorLogs(logs)
	if len(errs) != 1 || errs[0]["exception.message"] != "boom" || errs[0]["zharp.handled"] != "false" || errs[0]["session.id"] != "test-session" {
		t.Fatalf("errors %v", errs)
	}
	if !strings.Contains(errs[0]["exception.stacktrace"], "zharpfiber.setup") {
		t.Errorf("stack doesn't start at the handler: %s", errs[0]["exception.stacktrace"])
	}
}

func TestHandlerError(t *testing.T) {
	spans, logs, app := setup(t, zharp.Config{})
	if code, _ := serve(t, app, "GET", "/fail", ""); code != 500 {
		t.Fatalf("status %d", code)
	}
	if code, body := serve(t, app, "GET", "/gone", ""); code != 410 || body != "gone" {
		t.Fatalf("the ErrorHandler answers as before: %d %q", code, body)
	}
	got := serverSpans(spans)
	if len(got) != 2 || got[0].Status.Code.String() != "Error" || got[1].Status.Code.String() != "Unset" {
		t.Fatalf("spans %v", got)
	}
	errs := errorLogs(logs)
	if len(errs) != 1 || errs[0]["exception.message"] != "database down" || errs[0]["zharp.handled"] != "false" {
		t.Fatalf("one error for the 500, none for the 410: %v", errs)
	}
}

func TestLogsOnly(t *testing.T) {
	t.Setenv("ZHARP_TRACING", "false")
	spans, logs, app := setup(t, zharp.Config{})
	serve(t, app, "GET", "/orders/7", "")
	if code, _ := serve(t, app, "GET", "/boom", ""); code != 500 {
		t.Fatalf("status %d", code)
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
	spans, _, app := setup(t, zharp.Config{CaptureBodies: &zharp.CaptureBodies{}})
	if code, body := serve(t, app, "POST", "/signup", `{"email":"ada@x.test","password":"pw-secret"}`); code != 422 || !strings.Contains(body, "ada@x.test taken") {
		t.Fatalf("the app answers as before: %d %s", code, body)
	}
	serve(t, app, "POST", "/ok", `{"n":1}`)
	got := serverSpans(spans)
	if len(got) != 2 {
		t.Fatalf("%d spans", len(got))
	}
	if attr(got[0], "http.request.body") != `{"email":"ada@x.test","password":"[Filtered]"}` ||
		attr(got[0], "http.response.body") != `{"error":"email ada@x.test taken","sessionToken":"[Filtered]"}` {
		t.Fatalf("signup: %v", got[0].Attributes)
	}
	if attr(got[1], "http.request.body") != "" {
		t.Fatalf("errors mode: a 200 carries no bodies: %v", got[1].Attributes)
	}
}
