package zharp

import (
	"net/http"
	"regexp"
	"sort"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Request and response headers on HTTP spans, as OpenTelemetry names them
// (http.request.header.user-agent, http.response.header.content-type), so a failed request shows
// what was sent and what came back — and what wasn't sent (a 401 with no Authorization header).
// Secret headers are recorded as "[Filtered]": you see one was sent, never its value. On unless
// Config.DisableHeaders or ZHARP_CAPTURE_HEADERS=false.

// secretHeaders are filtered whatever the key rules say.
var secretHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true, "set-cookie": true,
	"x-api-key": true, "x-auth-token": true, "x-csrf-token": true, "x-xsrf-token": true,
}

const (
	maxHeaders     = 40
	maxHeaderValue = 512
)

// HeaderAttributes are the http.<side>.header.* attributes for h, secrets filtered. side is
// "request" or "response". For router integrations; Handler and Transport record them already.
func HeaderAttributes(side string, h http.Header) []attribute.KeyValue {
	if len(h) == 0 {
		return nil
	}
	names := make([]string, 0, len(h))
	for name := range h {
		names = append(names, name)
	}
	sort.Strings(names)
	var extra []string
	var patterns []*regexp.Regexp
	if s := activeBodies(); s != nil {
		extra, patterns = s.extraKeys, s.extraRegexp
	}
	out := make([]attribute.KeyValue, 0, len(names))
	for _, raw := range names {
		if len(out) >= maxHeaders {
			break
		}
		name := strings.ToLower(raw)
		value := strings.Join(h[raw], ", ")
		if secretHeaders[name] || isSecretKey(name, extra, patterns) {
			value = filtered
		} else if len(value) > maxHeaderValue {
			value = value[:maxHeaderValue] + "…"
		}
		out = append(out, attribute.String("http."+side+".header."+name, value))
	}
	return out
}

// RecordHeaders puts a request's and its response's headers on span, when header capture is on.
func RecordHeaders(span trace.Span, request, response http.Header) {
	defer func() { _ = recover() }() // never break a request over its telemetry
	if !headersEnabled() || !span.IsRecording() {
		return
	}
	span.SetAttributes(HeaderAttributes("request", request)...)
	span.SetAttributes(HeaderAttributes("response", response)...)
}

func headersEnabled() bool {
	s := active()
	return s != nil && s.tracing && s.headers
}
