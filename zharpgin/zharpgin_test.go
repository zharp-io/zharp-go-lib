package zharpgin

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
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

func setup(t *testing.T) (*tracetest.InMemoryExporter, *memoryLogs, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "gin-unit", SpanExporter: spans, LogExporter: logs})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	router := gin.New()
	router.Use(gin.Recovery(), Middleware())
	router.GET("/orders/:id", func(c *gin.Context) {
		slog.InfoContext(c.Request.Context(), "order viewed")
		c.String(200, "ok")
	})
	router.GET("/boom", func(c *gin.Context) { panic("boom") })
	router.GET("/fail", func(c *gin.Context) {
		_ = c.AbortWithError(503, errors.New("database down"))
	})
	return spans, logs, router
}

func serve(router *gin.Engine, path string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	r.Header.Set("baggage", "enduser.id=test-user,session.id=test-session")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, r)
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
	spans, logs, router := setup(t)
	if w := serve(router, "/orders/7"); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	s := server(t, spans)
	if s.SpanContext.TraceID().String() != traceID || attr(s, "http.route") != "/orders/:id" {
		t.Errorf("trace %s route %q", s.SpanContext.TraceID(), attr(s, "http.route"))
	}
	if attr(s, "enduser.id") != "test-user" || attr(s, "session.id") != "test-session" {
		t.Errorf("attributes %v", s.Attributes)
	}
	if len(logs.records) != 1 || logs.records[0].TraceID().String() != traceID || attrs(logs.records[0])["session.id"] != "test-session" {
		t.Errorf("logs %v", logs.records)
	}
}

func TestPanic(t *testing.T) {
	spans, logs, router := setup(t)
	if w := serve(router, "/boom"); w.Code != 500 {
		t.Fatalf("status %d (gin.Recovery should still answer)", w.Code)
	}
	s := server(t, spans)
	if s.Status.Code.String() != "Error" || attr(s, "http.response.status_code") != "500" {
		t.Errorf("status %v code %q", s.Status, attr(s, "http.response.status_code"))
	}
	errs := errorLogs(logs)
	if len(errs) != 1 || errs[0]["exception.message"] != "boom" || errs[0]["zharp.handled"] != "false" || errs[0]["session.id"] != "test-session" {
		t.Fatalf("errors %v", errs)
	}
}

func TestHandlerError(t *testing.T) {
	spans, logs, router := setup(t)
	if w := serve(router, "/fail"); w.Code != 503 {
		t.Fatalf("status %d", w.Code)
	}
	if server(t, spans).Status.Code.String() != "Error" {
		t.Error("span not failed")
	}
	errs := errorLogs(logs)
	if len(errs) != 1 || errs[0]["exception.message"] != "database down" || errs[0]["zharp.handled"] != "false" {
		t.Fatalf("errors %v", errs)
	}
	_ = http.StatusOK
}

func TestLogsOnly(t *testing.T) {
	t.Setenv("ZHARP_TRACING", "false")
	spans, logs, router := setup(t)
	if w := serve(router, "/orders/7"); w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	if w := serve(router, "/boom"); w.Code != 500 {
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
