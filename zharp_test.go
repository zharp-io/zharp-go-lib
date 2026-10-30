package zharp

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

const (
	traceID  = "4bf92f3577b34da6a3ce929d0e0e4736"
	parentID = "00f067aa0ba902b7"
)

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

func (m *memoryLogs) all() []sdklog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]sdklog.Record(nil), m.records...)
}

func attrs(r sdklog.Record) map[string]string {
	out := map[string]string{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value.Emit()
		return true
	})
	return out
}

func setup(t *testing.T) (*tracetest.InMemoryExporter, *memoryLogs) {
	t.Helper()
	spans := tracetest.NewInMemoryExporter()
	logs := &memoryLogs{}
	shutdown := Init(Config{
		Key: "zpk_test", Service: "go-unit", Environment: "test", Release: "api@1.0.0",
		SpanExporter: spans, LogExporter: logs,
	})
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return spans, logs
}

func request(path string) *http.Request {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("traceparent", "00-"+traceID+"-"+parentID+"-01")
	r.Header.Set("baggage", "enduser.id=test-user,session.id=test-session")
	return r
}

func serverSpan(t *testing.T, spans *tracetest.InMemoryExporter) tracetest.SpanStub {
	t.Helper()
	for _, s := range spans.GetSpans() {
		if s.SpanKind == trace.SpanKindServer {
			return s
		}
	}
	t.Fatalf("no server span in %d spans", len(spans.GetSpans()))
	return tracetest.SpanStub{}
}

func spanAttr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

func errorLogs(logs *memoryLogs) []sdklog.Record {
	var out []sdklog.Record
	for _, r := range logs.all() {
		if _, ok := attrs(r)["exception.type"]; ok {
			out = append(out, r)
		}
	}
	return out
}

func mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("GET /orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		slog.InfoContext(r.Context(), "order viewed", "id", r.PathValue("id"))
		Log.Warn(r.Context(), "from zharp.Log", "order_id", 7)
		fmt.Fprint(w, "ok")
	})
	m.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
		panic(errors.New("boom"))
	})
	m.HandleFunc("GET /caught", func(w http.ResponseWriter, r *http.Request) {
		_, err := fs.Stat(emptyFS{}, "missing.txt")
		CaptureError(r.Context(), fmt.Errorf("loading config: %w", err), "order_id", 9)
		fmt.Fprint(w, "ok")
	})
	m.HandleFunc("GET /who", func(w http.ResponseWriter, r *http.Request) {
		ctx := SetUser(r.Context(), User{ID: "server-user", Email: "a@example.com"})
		Log.Info(ctx, "after SetUser")
	})
	return m
}

type emptyFS struct{}

func (emptyFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func TestRequestContinuesTraceAndCopiesBaggage(t *testing.T) {
	spans, logs := setup(t)
	w := httptest.NewRecorder()
	Handler(mux()).ServeHTTP(w, request("/orders/42"))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}

	s := serverSpan(t, spans)
	if got := s.SpanContext.TraceID().String(); got != traceID {
		t.Errorf("trace id %s, want %s", got, traceID)
	}
	if got := s.Parent.SpanID().String(); got != parentID {
		t.Errorf("parent %s, want %s", got, parentID)
	}
	if s.Name != "GET /orders/{id}" || spanAttr(s, "http.route") != "/orders/{id}" {
		t.Errorf("name %q route %q", s.Name, spanAttr(s, "http.route"))
	}
	if spanAttr(s, "enduser.id") != "test-user" || spanAttr(s, "session.id") != "test-session" {
		t.Errorf("span user/session: %v", s.Attributes)
	}

	bodies := map[string]sdklog.Record{}
	for _, r := range logs.all() {
		bodies[r.Body().Emit()] = r
	}
	for _, body := range []string{"order viewed", "from zharp.Log"} {
		r, ok := bodies[body]
		if !ok {
			t.Fatalf("no log %q in %v", body, bodies)
		}
		if r.TraceID().String() != traceID {
			t.Errorf("%q trace id %s", body, r.TraceID())
		}
		a := attrs(r)
		if a["enduser.id"] != "test-user" || a["session.id"] != "test-session" {
			t.Errorf("%q attributes %v", body, a)
		}
	}
	if attrs(bodies["from zharp.Log"])["order_id"] != "7" {
		t.Errorf("order_id missing: %v", attrs(bodies["from zharp.Log"]))
	}
}

func TestPanicIsUnhandledErrorAnd500(t *testing.T) {
	spans, logs := setup(t)
	w := httptest.NewRecorder()
	Handler(mux()).ServeHTTP(w, request("/boom"))
	if w.Code != 500 {
		t.Fatalf("status %d", w.Code)
	}
	s := serverSpan(t, spans)
	if s.Status.Code.String() != "Error" {
		t.Errorf("span status %v", s.Status)
	}
	errs := errorLogs(logs)
	if len(errs) != 1 {
		t.Fatalf("%d error logs", len(errs))
	}
	a := attrs(errs[0])
	if a["exception.type"] != "*errors.errorString" || a["exception.message"] != "boom" || a["zharp.handled"] != "false" {
		t.Errorf("attributes %v", a)
	}
	stack := a["exception.stacktrace"]
	if !strings.HasPrefix(stack, "*errors.errorString: boom\n") || !strings.Contains(stack, "goroutine ") {
		t.Errorf("stack:\n%s", stack)
	}
	firstFrame := strings.SplitN(strings.SplitN(stack, "[running]:\n", 2)[1], "\n", 2)[0]
	if !strings.Contains(firstFrame, "mux.func2") {
		t.Errorf("first frame %q, want the panicking handler\n%s", firstFrame, stack)
	}
	if errs[0].TraceID().String() != traceID || a["session.id"] != "test-session" {
		t.Errorf("trace %s attrs %v", errs[0].TraceID(), a)
	}
	if errs[0].Severity().String() != "ERROR" {
		t.Errorf("severity %v", errs[0].Severity())
	}
}

func TestCaptureErrorIsHandled(t *testing.T) {
	_, logs := setup(t)
	Handler(mux()).ServeHTTP(httptest.NewRecorder(), request("/caught"))
	errs := errorLogs(logs)
	if len(errs) != 1 {
		t.Fatalf("%d error logs", len(errs))
	}
	a := attrs(errs[0])
	if a["exception.type"] != "*fs.PathError" || a["zharp.handled"] != "true" || a["order_id"] != "9" {
		t.Errorf("attributes %v", a)
	}
	if !strings.HasPrefix(a["exception.message"], "loading config: open missing.txt") {
		t.Errorf("message %q", a["exception.message"])
	}
	if strings.Contains(a["exception.stacktrace"], scopeName+".CaptureError") {
		t.Errorf("stack has Zharp's own frames:\n%s", a["exception.stacktrace"])
	}
	if errs[0].TraceID().String() != traceID {
		t.Errorf("trace %s", errs[0].TraceID())
	}
}

func TestSetUserOverridesBaggage(t *testing.T) {
	_, logs := setup(t)
	Handler(mux()).ServeHTTP(httptest.NewRecorder(), request("/who"))
	for _, r := range logs.all() {
		if r.Body().Emit() == "after SetUser" {
			a := attrs(r)
			if a["enduser.id"] != "server-user" || a["enduser.email"] != "a@example.com" || a["session.id"] != "test-session" {
				t.Errorf("attributes %v", a)
			}
			return
		}
	}
	t.Fatal("log not found")
}

func TestResourceTagsAndSpans(t *testing.T) {
	spans, logs := setup(t)
	SetTag("plan", "pro")
	defer SetTag("plan", "")
	ctx, span := Tracer().Start(context.Background(), "render invoice")
	Log.Debug(ctx, "inside")
	span.End()

	got := spans.GetSpans()
	if len(got) != 1 || spanAttr(got[0], "plan") != "pro" {
		t.Fatalf("spans %v", got)
	}
	res := map[string]string{}
	for _, kv := range got[0].Resource.Attributes() {
		res[string(kv.Key)] = kv.Value.Emit()
	}
	want := map[string]string{
		"service.name": "go-unit", "deployment.environment.name": "test", "service.version": "api@1.0.0",
		"telemetry.sdk.name": "zharp", "telemetry.sdk.language": "go", "telemetry.sdk.version": Version,
	}
	for k, v := range want {
		if res[k] != v {
			t.Errorf("resource %s = %q, want %q", k, res[k], v)
		}
	}
	r := logs.all()[0]
	if r.TraceID() != got[0].SpanContext.TraceID() || attrs(r)["plan"] != "pro" || r.Severity().String() != "DEBUG" {
		t.Errorf("log %v %v", r.TraceID(), attrs(r))
	}
}

func TestNoKeyIsNoop(t *testing.T) {
	t.Setenv("ZHARP_KEY", "")
	t.Setenv("ZHARP_API_KEY", "")
	shutdown := Init(Config{})
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	CaptureError(context.Background(), errors.New("ignored")) // must not panic
}

func TestRouteOf(t *testing.T) {
	for in, want := range map[string]string{
		"GET /orders/{id}": "/orders/{id}", "/static/": "/static/", "example.com/x": "/x", "POST example.com/y/{z}": "/y/{z}", "": "",
	} {
		if got := routeOf(in); got != want {
			t.Errorf("routeOf(%q) = %q, want %q", in, got, want)
		}
	}
}

var _ sdktrace.SpanExporter = (*tracetest.InMemoryExporter)(nil)
