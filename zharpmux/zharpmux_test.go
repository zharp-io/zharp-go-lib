package zharpmux

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
	"github.com/urfave/negroni/v3"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	zharp "github.com/zharp-io/zharp-go-lib"
)

const traceID = "4bf92f3577b34da6a3ce929d0e0e4736"

func setup(t *testing.T) *tracetest.InMemoryExporter {
	spans := tracetest.NewInMemoryExporter()
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "mux-unit", SpanExporter: spans, LogExporter: nopLogs{}})
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return spans
}

func serve(h http.Handler, path string) int {
	r := httptest.NewRequest("GET", path, nil)
	r.Header.Set("traceparent", "00-"+traceID+"-00f067aa0ba902b7-01")
	r.Header.Set("Authorization", "Bearer sk-live-secret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w.Code
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

func attr(s tracetest.SpanStub, key string) string {
	for _, kv := range s.Attributes {
		if string(kv.Key) == key {
			return kv.Value.Emit()
		}
	}
	return ""
}

func ok(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) }

func gorilla() *mux.Router {
	router := mux.NewRouter()
	router.Use(Middleware())
	router.HandleFunc("/orders/{id}", ok).Methods("GET")
	router.PathPrefix("/api").Subrouter().HandleFunc("/users/{id:[0-9]+}", ok)
	router.HandleFunc("/boom", func(http.ResponseWriter, *http.Request) { panic("boom") })
	return router
}

// gorilla/mux behind Negroni, the way Negroni's README sets it up.
func TestNegroniGorilla(t *testing.T) {
	spans := setup(t)
	n := negroni.New(negroni.NewLogger())
	n.UseHandler(gorilla())
	h := zharp.Handler(n)
	serve(h, "/orders/7")
	serve(h, "/api/users/3")
	if code := serve(h, "/nope"); code != 404 {
		t.Fatalf("status %d", code)
	}
	if code := serve(h, "/boom"); code != 500 {
		t.Fatalf("panic: status %d", code)
	}
	got := serverSpans(spans)
	if len(got) != 4 {
		t.Fatalf("want 4 server spans, got %d", len(got))
	}
	for i, want := range []string{"GET /orders/{id}", "GET /api/users/{id:[0-9]+}", "GET", "GET /boom"} {
		if got[i].Name != want {
			t.Errorf("span %d: name %q, want %q", i, got[i].Name, want)
		}
	}
	if s := got[0]; s.SpanContext.TraceID().String() != traceID || attr(s, "http.route") != "/orders/{id}" ||
		attr(s, "http.request.header.authorization") != "[Filtered]" {
		t.Errorf("trace %s attributes %v", s.SpanContext.TraceID(), s.Attributes)
	}
	if got[3].Status.Code.String() != "Error" {
		t.Errorf("panic: span status %v", got[3].Status)
	}
}

// Negroni with Go's ServeMux needs nothing more than zharp.Handler.
func TestNegroniServeMux(t *testing.T) {
	spans := setup(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /orders/{id}", ok)
	n := negroni.Classic()
	n.UseHandler(mux)
	serve(zharp.Handler(n), "/orders/7")
	got := serverSpans(spans)
	if len(got) != 1 || got[0].Name != "GET /orders/{id}" || got[0].SpanContext.TraceID().String() != traceID {
		t.Fatalf("spans %v", got)
	}
}
