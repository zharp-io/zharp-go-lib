package zharp

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.opentelemetry.io/otel/trace"
)

func TestHeadersOnTheRequestSpanSecretsFiltered(t *testing.T) {
	spans, _ := setup(t)
	handler := Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "sid=1")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	r := httptest.NewRequest("GET", "/orders", nil)
	r.Header.Set("User-Agent", "okhttp/4.12.0")
	r.Header.Set("Authorization", "Bearer abc")
	r.Header.Set("X-Session-Token", "t")
	handler.ServeHTTP(httptest.NewRecorder(), r)

	got := map[string]string{}
	for _, s := range spans.GetSpans() {
		if s.SpanKind == trace.SpanKindServer {
			for _, kv := range s.Attributes {
				got[string(kv.Key)] = kv.Value.Emit()
			}
		}
	}
	want := map[string]string{
		"http.request.header.user-agent":      "okhttp/4.12.0",
		"http.request.header.authorization":   "[Filtered]",
		"http.request.header.x-session-token": "[Filtered]",
		"http.response.header.content-type":   "application/json",
		"http.response.header.set-cookie":     "[Filtered]",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}
