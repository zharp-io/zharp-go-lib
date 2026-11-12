package zharp

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
)

func gzipWriter(w io.Writer) *gzip.Writer { return gzip.NewWriter(w) }

func attrMap(kvs []attribute.KeyValue) map[string]string {
	out := map[string]string{}
	for _, kv := range kvs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

func setupBodies(t *testing.T, bodies *CaptureBodies) *tracetest.InMemoryExporter {
	t.Helper()
	spans := tracetest.NewInMemoryExporter()
	shutdown := Init(Config{Key: "zpk_test", Service: "go-bodies", SpanExporter: spans, LogExporter: &memoryLogs{}, CaptureBodies: bodies})
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return spans
}

func spansOf(spans *tracetest.InMemoryExporter, kind trace.SpanKind) []map[string]string {
	var out []map[string]string
	for _, s := range spans.GetSpans() {
		if s.SpanKind == kind {
			out = append(out, attrMap(s.Attributes))
		}
	}
	return out
}

func serve(t *testing.T, h http.Handler, method, path, contentType string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	if contentType != "" {
		r.Header.Set("Content-Type", contentType)
	}
	w := httptest.NewRecorder()
	Handler(h).ServeHTTP(w, r)
	return w
}

func TestHandlerCapturesFailedRequests(t *testing.T) {
	spans := setupBodies(t, &CaptureBodies{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /signup", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(422)
		_, _ = w.Write([]byte(`{"error":"email ` + in["email"].(string) + ` taken","sessionToken":"srv-token"}`))
	})
	mux.HandleFunc("POST /ok", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		_, _ = w.Write([]byte(`{"saved":true}`))
	})

	got := serve(t, mux, "POST", "/signup", "application/json", []byte(`{"email":"ada@x.test","password":"pw-secret"}`))
	if got.Code != 422 || !strings.Contains(got.Body.String(), "ada@x.test taken") {
		t.Fatalf("the app answers as without the SDK: %d %s", got.Code, got.Body)
	}
	serve(t, mux, "POST", "/ok", "application/json", []byte(`{"name":"ok-body"}`))

	server := spansOf(spans, trace.SpanKindServer)
	if len(server) != 2 {
		t.Fatalf("want 2 server spans, got %d", len(server))
	}
	signup := server[0]
	if signup[attrRequestBody] != `{"email":"ada@x.test","password":"[Filtered]"}` ||
		signup[attrRequestType] != "application/json" ||
		signup[attrResponseBody] != `{"error":"email ada@x.test taken","sessionToken":"[Filtered]"}` ||
		signup[attrResponseType] != "application/json" {
		t.Fatalf("signup span: %v", signup)
	}
	if _, ok := server[1][attrRequestBody]; ok {
		t.Fatalf("errors mode: a 200 carries no bodies: %v", server[1])
	}
}

func TestHandlerUploadsBinaryAndPanics(t *testing.T) {
	spans := setupBodies(t, &CaptureBodies{Mode: BodiesAll})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Error(err)
		}
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
	})
	mux.HandleFunc("GET /thumb", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 3000)...)) // no Content-Type: sniffed, as net/http does
	})
	mux.HandleFunc("POST /later", func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Millisecond)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.Copy(w, strings.NewReader("read ")) // through ReadFrom
		_, _ = w.Write([]byte(string(rune('0'+len(body)/10)) + "0 bytes"))
	})
	mux.HandleFunc("POST /boom", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		panic("boom")
	})

	boundary := "zb42"
	serve(t, mux, "POST", "/upload", "multipart/form-data; boundary="+boundary, multipartBody(boundary, bytes.Repeat([]byte{1}, 100*1024)))
	serve(t, mux, "GET", "/thumb", "", nil)
	serve(t, mux, "POST", "/later", "text/plain", []byte(strings.Repeat("y", 50)))
	if got := serve(t, mux, "POST", "/boom", "application/json", []byte(`{"cvv":"123","n":1}`)); got.Code != 500 {
		t.Fatalf("panic still answers 500: %d", got.Code)
	}

	server := spansOf(spans, trace.SpanKindServer)
	if len(server) != 4 {
		t.Fatalf("want 4 server spans, got %d", len(server))
	}
	if got := server[0][attrRequestBody]; got != `{"title":"Holiday","password":"[Filtered]","avatar":"[file me.png · image/png · 100 KB]","tag":["a","b"]}` {
		t.Fatalf("upload: %s", got)
	}
	if got := server[1][attrResponseBody]; got != "[binary · image/png · 3 KB]" {
		t.Fatalf("binary response: %v", server[1])
	}
	if server[2][attrRequestBody] != strings.Repeat("y", 50) || server[2][attrResponseBody] != "read 50 bytes" {
		t.Fatalf("late read and ReadFrom: %v", server[2])
	}
	if server[3][attrRequestBody] != `{"cvv":"[Filtered]","n":1}` || !strings.Contains(server[3][attrResponseBody], "Internal Server Error") {
		t.Fatalf("panic: %v", server[3])
	}
}

func TestTransportCapturesFailedCalls(t *testing.T) {
	spans := setupBodies(t, &CaptureBodies{})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/charge" {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(502)
			zw := gzip.NewWriter(w)
			_, _ = zw.Write([]byte(`{"error":"card declined","cardNumber":"4242424242424242"}`))
			_ = zw.Close()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"fine":true}`))
	}))
	defer downstream.Close()
	client := &http.Client{Transport: Transport(nil)}

	// The app asked for gzip itself, so the body arrives compressed; it decodes JSON and closes
	// without reading to EOF.
	req, _ := http.NewRequest("POST", downstream.URL+"/charge?token=tok_secret&page=2", strings.NewReader(`{"amount":5,"cvv":"123"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	zr, _ := gzip.NewReader(resp.Body)
	var out map[string]any
	_ = json.NewDecoder(zr).Decode(&out)
	_ = resp.Body.Close()
	if out["error"] != "card declined" {
		t.Fatalf("the app reads its response as before: %v", out)
	}

	resp, err = client.Post(downstream.URL+"/fine", "application/json", strings.NewReader(`{"outgoing":"ok-outgoing-body"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	client2 := spansOf(spans, trace.SpanKindClient)
	if len(client2) != 2 {
		t.Fatalf("want 2 client spans, got %d", len(client2))
	}
	charge := client2[0]
	if charge[attrRequestBody] != `{"amount":5,"cvv":"[Filtered]"}` ||
		charge[attrResponseBody] != `{"error":"card declined","cardNumber":"[Filtered]"}` {
		t.Fatalf("charge: %v", charge)
	}
	if !strings.HasSuffix(charge["url.full"], "/charge?token=[Filtered]&page=2") {
		t.Fatalf("secret query values are filtered: %s", charge["url.full"])
	}
	if _, ok := client2[1][attrRequestBody]; ok {
		t.Fatalf("errors mode: a 200 carries no bodies: %v", client2[1])
	}
}

func TestTransportReadToEOFAndNoResponse(t *testing.T) {
	spans := setupBodies(t, &CaptureBodies{Mode: BodiesAll})
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("pong"))
	}))
	client := &http.Client{Transport: Transport(nil)}
	resp, err := client.Get(downstream.URL + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if string(body) != "pong" {
		t.Fatal(string(body))
	}
	downstream.Close()
	_, err = client.Post(downstream.URL+"/gone", "application/json", strings.NewReader(`{"password":"x","q":1}`))
	if err == nil {
		t.Fatal("want a connection error")
	}

	got := spansOf(spans, trace.SpanKindClient)
	if len(got) != 2 || got[0][attrResponseBody] != "pong" {
		t.Fatalf("read to EOF: %v", got)
	}
	if got[1][attrRequestBody] != `{"password":"[Filtered]","q":1}` {
		t.Fatalf("no response: the request body is kept: %v", got[1])
	}
}

func TestURLsAreMaskedWithoutCaptureBodies(t *testing.T) {
	spans := setupBodies(t, nil)
	_, span := Tracer().Start(context.Background(), "manual")
	span.SetAttributes(attribute.String("url.full", "https://user:pw@x.test/a?api_key=k&page=1"), attribute.String("url.query", "?session=abc&q=1"), attribute.String("http.target", "/a?sig=s"))
	span.End()
	got := attrMap(spans.GetSpans()[0].Attributes)
	if got["url.full"] != "https://x.test/a?api_key=[Filtered]&page=1" || got["url.query"] != "?session=[Filtered]&q=1" || got["http.target"] != "/a?sig=[Filtered]" {
		t.Fatalf("%v", got)
	}
}

func TestOffByDefault(t *testing.T) {
	spans := setupBodies(t, nil)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /fail", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "no", 500) })
	serve(t, mux, "POST", "/fail", "application/json", []byte(`{"a":1}`))
	for _, s := range spansOf(spans, trace.SpanKindServer) {
		if _, ok := s[attrRequestBody]; ok {
			t.Fatalf("no bodies unless CaptureBodies is set: %v", s)
		}
	}
}
