package zharp

// How bodies are seen without changing what the app reads or writes: request bodies through a
// reader that passes every byte on and keeps a copy of the first ones, responses through the
// ResponseWriter's Write and ReadFrom (or, for outgoing calls, the response body's Read). What is
// kept depends on the body:
//   - text: the first bytes, up to the raw limit
//   - a form upload (multipart/form-data): each text field, and each file as its name, type and
//     size — file bytes are counted as they pass, never kept
//   - anything else (an image, a PDF, an event stream): only its size

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"compress/zlib"
	"context"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"

	"github.com/felixge/httpsnoop"
	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// maxFields is how many fields of one form upload are kept.
const maxFields = 200

func activeBodies() *bodySettings {
	s := active()
	if s == nil || !s.tracing {
		return nil
	}
	return s.bodies
}

// ── multipart/form-data ───────────────────────────────────────────────────────

type part struct {
	name, filename, contentType string
	hasFile                     bool
	size                        int64
	text                        []byte
}

// multipartSummary reads a form upload as it streams past: text fields kept (up to maxText each),
// files counted.
type multipartSummary struct {
	buf       []byte
	state     int // 0 start, 1 after a boundary, 2 headers, 3 body, 4 done
	first     []byte
	delimiter []byte
	maxText   int
	part      *part
	keys      []string
	values    map[string][]string
	count     int
	cut       bool
}

func newMultipartSummary(boundary string, maxText int) *multipartSummary {
	return &multipartSummary{
		first:     []byte("--" + boundary),
		delimiter: []byte("\r\n--" + boundary),
		maxText:   maxText,
		values:    map[string][]string{},
	}
}

func (m *multipartSummary) push(chunk []byte) {
	if m.state == 4 {
		return
	}
	m.buf = append(m.buf, chunk...)
	for {
		switch m.state {
		case 0:
			i := bytes.Index(m.buf, m.first)
			if i < 0 {
				if len(m.buf) > 64*1024 {
					m.state = 4
				}
				return
			}
			m.buf = m.buf[i+len(m.first):]
			m.state = 1
		case 1:
			if len(m.buf) < 2 {
				return
			}
			if m.buf[0] == '-' && m.buf[1] == '-' {
				m.state, m.buf = 4, nil
				return
			}
			if m.buf[0] == '\r' && m.buf[1] == '\n' {
				m.buf = m.buf[2:]
			}
			m.state = 2
		case 2:
			i := bytes.Index(m.buf, []byte("\r\n\r\n"))
			if i < 0 {
				if len(m.buf) > 16*1024 {
					m.state = 4
				}
				return
			}
			m.part = parsePartHeaders(string(m.buf[:i]))
			m.buf = m.buf[i+4:]
			m.state = 3
		case 3:
			if i := bytes.Index(m.buf, m.delimiter); i >= 0 {
				m.consume(m.buf[:i])
				m.finish(false)
				m.buf = m.buf[i+len(m.delimiter):]
				m.state = 1
				if m.count >= maxFields {
					m.state, m.cut = 4, true
					return
				}
				continue
			}
			// Keep what could be the start of the delimiter; the rest belongs to this part.
			keep := len(m.delimiter) - 1
			if len(m.buf) > keep {
				m.consume(m.buf[:len(m.buf)-keep])
				m.buf = append([]byte(nil), m.buf[len(m.buf)-keep:]...)
			}
			return
		default:
			return
		}
	}
}

func (m *multipartSummary) consume(b []byte) {
	p := m.part
	if p == nil || len(b) == 0 {
		return
	}
	p.size += int64(len(b))
	if p.hasFile {
		return // a file: counted, never kept
	}
	if room := m.maxText - len(p.text); room > 0 {
		p.text = append(p.text, b[:min(room, len(b))]...)
	}
}

func (m *multipartSummary) finish(partial bool) {
	p := m.part
	m.part = nil
	if p == nil {
		return
	}
	var value string
	if p.hasFile {
		value = fileLabel(p.filename, p.contentType, p.size, partial)
	} else {
		value = validText(p.text)
		if p.size > int64(len(p.text)) || partial {
			m.cut = true
		}
	}
	if _, ok := m.values[p.name]; !ok {
		m.keys = append(m.keys, p.name)
	}
	m.values[p.name] = append(m.values[p.name], value)
	m.count++
}

// result is the fields so far as JSON, including a part the stream stopped inside.
func (m *multipartSummary) result() (string, bool) {
	if m.part != nil {
		if m.state == 3 {
			m.consume(m.buf)
		}
		m.buf = nil
		m.finish(true)
		m.cut = true
	}
	obj := &jsonObject{index: map[string]int{}}
	for _, k := range m.keys {
		vs := m.values[k]
		if len(vs) == 1 {
			obj.set(k, vs[0])
			continue
		}
		arr := make([]any, len(vs))
		for i, v := range vs {
			arr[i] = v
		}
		obj.set(k, arr)
	}
	return stringifyJS(obj), m.cut
}

var (
	dispositionHeader = regexp.MustCompile(`(?im)^content-disposition:(.*)$`)
	partNameQuoted    = regexp.MustCompile(`(?i)\bname="([^"]*)"`)
	partNameBare      = regexp.MustCompile(`(?i)\bname=([^;\s]+)`)
	partFilename      = regexp.MustCompile(`(?i)\bfilename="([^"]*)"`)
	partFilenameStar  = regexp.MustCompile(`(?i)\bfilename\*=(?:UTF-8'')?([^;\s]+)`)
	partContentType   = regexp.MustCompile(`(?im)^content-type:\s*(.*)$`)
	boundaryParam     = regexp.MustCompile(`(?i)boundary=(?:"([^"]+)"|([^;\s]+))`)
)

func parsePartHeaders(head string) *part {
	p := &part{}
	disposition := ""
	if m := dispositionHeader.FindStringSubmatch(head); m != nil {
		disposition = m[1]
	}
	if m := partNameQuoted.FindStringSubmatch(disposition); m != nil {
		p.name = m[1]
	} else if m := partNameBare.FindStringSubmatch(disposition); m != nil {
		p.name = m[1]
	}
	if m := partFilename.FindStringSubmatch(disposition); m != nil {
		p.filename, p.hasFile = m[1], true
	}
	if m := partFilenameStar.FindStringSubmatch(disposition); m != nil {
		p.hasFile = true
		if decoded, ok := jsDecodeURIComponent(m[1]); ok {
			p.filename = decoded
		} else {
			p.filename = m[1]
		}
	}
	if p.hasFile {
		if m := partContentType.FindStringSubmatch(head); m != nil {
			p.contentType = strings.TrimSpace(strings.TrimRight(m[1], "\r"))
		}
	}
	return p
}

func boundaryOf(contentType string) string {
	m := boundaryParam.FindStringSubmatch(contentType)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// ── taps ──────────────────────────────────────────────────────────────────────

const (
	modeUndecided = iota
	modeText
	modeCount
	modeMultipart
)

// tap keeps what passes through one side of a request. Safe for concurrent use: an outgoing
// request's body is read by the transport's own goroutine.
type tap struct {
	mu        sync.Mutex
	settings  *bodySettings
	typeOf    func(first []byte) string
	mode      int
	detected  string // the content type typeOf gave
	kept      []byte
	total     int64
	cut       bool
	multipart *multipartSummary
}

func newTap(s *bodySettings, typeOf func(first []byte) string) *tap {
	return &tap{settings: s, typeOf: typeOf}
}

func (t *tap) add(p []byte) {
	if t == nil || len(p) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += int64(len(p))
	if t.mode == modeUndecided {
		contentType := t.typeOf(p)
		t.detected = contentType
		switch {
		case isMultipart(contentType) && boundaryOf(contentType) != "":
			t.mode = modeMultipart
			t.multipart = newMultipartSummary(boundaryOf(contentType), t.settings.maxSize)
		case isTextual(contentType):
			t.mode = modeText
		default:
			t.mode = modeCount
		}
	}
	switch t.mode {
	case modeMultipart:
		t.multipart.push(p)
	case modeText:
		if t.cut {
			return
		}
		room := t.settings.rawLimit() - len(t.kept)
		if len(p) > room {
			t.cut = true
			p = p[:max(0, room)]
		}
		t.kept = append(t.kept, p...)
	}
}

// sniff is the content type net/http gave a response sent without one.
func (t *tap) sniff() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.detected
}

func (t *tap) empty() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.total == 0
}

// text is the kept text, inflated if it was compressed; ok is false when it can't be read as text.
func (t *tap) text(encoding string) (string, bool) {
	raw := t.kept
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "identity":
	case "gzip", "x-gzip":
		r, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return "", false
		}
		raw, _ = io.ReadAll(r) // a cut-off stream still inflates up to where it stops
	case "deflate":
		if r, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
			raw, _ = io.ReadAll(r)
		} else {
			raw, _ = io.ReadAll(flate.NewReader(bytes.NewReader(raw)))
		}
	default:
		return "", false // br and others: described
	}
	return validText(raw), true
}

// attributes are one side's span attributes, whatever kind of body it was.
func (t *tap) attributes(side, contentType, encoding string) []attribute.KeyValue {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.total == 0 {
		return nil
	}
	if t.multipart != nil {
		json, cut := t.multipart.result()
		return bodyAttributes(side, json, contentType, t.settings, cut)
	}
	if !isTextual(contentType) {
		return describedAttributes(side, contentType, t.total, false)
	}
	text, ok := t.text(encoding)
	if !ok {
		return describedAttributes(side, contentType, t.total, false)
	}
	return bodyAttributes(side, text, contentType, t.settings, t.cut)
}

// tapReader passes every byte on and keeps a copy in the tap.
type tapReader struct {
	io.ReadCloser
	tap  *tap
	done func() // called once at EOF
	once sync.Once
}

func (r *tapReader) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.tap.add(p[:n])
	if err == io.EOF && r.done != nil {
		r.once.Do(r.done)
	}
	return n, err
}

// ── incoming requests ─────────────────────────────────────────────────────────

// RequestBodies records one incoming request's bodies for its span, when Config.CaptureBodies is
// on. It is for router integrations (zharpgin, zharpecho); Handler uses it already. A nil
// *RequestBodies (bodies off, or the URL not in CaptureBodies.URLs) does nothing.
type RequestBodies struct {
	settings *bodySettings
	request  *http.Request
	req, res *tap
	header   func() http.Header
	mu       sync.Mutex
	status   int
	wrote    bool
	hijacked bool
	ended    bool
}

// CaptureRequestBodies starts recording r's bodies. It returns nil when there is nothing to do.
func CaptureRequestBodies(r *http.Request) *RequestBodies {
	s := activeBodies()
	if s == nil || r == nil || !s.allowed(incomingURL(r)) {
		return nil
	}
	b := &RequestBodies{settings: s, request: r}
	b.req = newTap(s, func([]byte) string { return r.Header.Get("Content-Type") })
	b.res = newTap(s, func(first []byte) string {
		if h := b.responseHeader(); h != nil {
			if t := h.Get("Content-Type"); t != "" {
				return t
			}
		}
		// net/http will sniff it from the same bytes.
		return http.DetectContentType(first)
	})
	return b
}

func incomingURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}

func (b *RequestBodies) responseHeader() http.Header {
	b.mu.Lock()
	h := b.header
	b.mu.Unlock()
	if h == nil {
		return nil
	}
	return h()
}

// Request returns the body the handler should read: every byte as before, with a copy of the
// first ones kept.
func (b *RequestBodies) Request(body io.ReadCloser) io.ReadCloser {
	if b == nil || body == nil || body == http.NoBody {
		return body
	}
	return &tapReader{ReadCloser: body, tap: b.req}
}

// ResponseWriter returns w with what is written to it recorded; every interface w implements
// (Flusher, Hijacker, Pusher, ReaderFrom) is kept.
func (b *RequestBodies) ResponseWriter(w http.ResponseWriter) http.ResponseWriter {
	if b == nil {
		return w
	}
	b.mu.Lock()
	b.header = w.Header
	b.mu.Unlock()
	return httpsnoop.Wrap(w, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) {
				b.noteStatus(code)
				next(code)
			}
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(p []byte) (int, error) {
				b.noteStatus(http.StatusOK)
				n, err := next(p)
				b.res.add(p[:n])
				return n, err
			}
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(src io.Reader) (int64, error) {
				b.noteStatus(http.StatusOK)
				return next(io.TeeReader(src, tapWriter{b.res}))
			}
		},
		Hijack: func(next httpsnoop.HijackFunc) httpsnoop.HijackFunc {
			return func() (net.Conn, *bufio.ReadWriter, error) {
				b.mu.Lock()
				b.hijacked = true
				b.mu.Unlock()
				return next()
			}
		},
	})
}

// Wrote records response bytes written some other way (a router's own ResponseWriter type).
// header is the response's header, for its content type.
func (b *RequestBodies) Wrote(p []byte, header http.Header) {
	if b == nil {
		return
	}
	b.mu.Lock()
	if b.header == nil && header != nil {
		b.header = func() http.Header { return header }
	}
	b.mu.Unlock()
	b.noteStatus(http.StatusOK)
	b.res.add(p)
}

func (b *RequestBodies) noteStatus(code int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.wrote && code >= 200 {
		b.status, b.wrote = code, true
	}
}

// Status is the status the response was sent with (200 if the handler set none).
func (b *RequestBodies) Status() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.wrote {
		return http.StatusOK
	}
	return b.status
}

// End puts the bodies on span, if the status says they are wanted. Call it once, before the span
// ends.
func (b *RequestBodies) End(span trace.Span, status int) {
	if b == nil || span == nil || !span.IsRecording() {
		return
	}
	b.mu.Lock()
	if b.ended {
		b.mu.Unlock()
		return
	}
	b.ended = true
	hijacked := b.hijacked
	b.mu.Unlock()
	if !b.settings.wanted(status) {
		return
	}
	attrs := b.req.attributes("request", b.request.Header.Get("Content-Type"), b.request.Header.Get("Content-Encoding"))
	if !hijacked {
		var contentType, encoding string
		if h := b.responseHeader(); h != nil {
			contentType, encoding = h.Get("Content-Type"), h.Get("Content-Encoding")
		}
		if contentType == "" {
			contentType = b.res.sniff()
		}
		attrs = append(attrs, b.res.attributes("response", contentType, encoding)...)
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}

type tapWriter struct{ t *tap }

func (w tapWriter) Write(p []byte) (int, error) {
	w.t.add(p)
	return len(p), nil
}

// ── outgoing requests ─────────────────────────────────────────────────────────

type clientBodiesKey struct{}

// clientBodies records one outgoing call. Transport makes it before otelhttp starts the span and
// finds it again inside, where the span is; it is finished at the response body's EOF (inside
// otelhttp's wrapper, before it ends the span) or Close (outside it, before it ends the span).
type clientBodies struct {
	settings *bodySettings
	req, res *tap
	request  *http.Request
	mu       sync.Mutex
	span     trace.Span
	response *http.Response
	err      bool
	finished bool
}

func newClientBodies(s *bodySettings, r *http.Request) *clientBodies {
	c := &clientBodies{settings: s, request: r}
	c.req = newTap(s, func([]byte) string { return r.Header.Get("Content-Type") })
	return c
}

func (c *clientBodies) finish() {
	c.mu.Lock()
	if c.finished || c.span == nil {
		c.mu.Unlock()
		return
	}
	c.finished = true
	span, resp, request := c.span, c.response, c.request
	c.mu.Unlock()

	status := 0
	if resp != nil {
		status = resp.StatusCode
	}
	if !span.IsRecording() || !c.settings.wanted(status) {
		return
	}
	// The transport never read the body (it failed to connect): read a fresh copy, as it would
	// have for a retry.
	if c.req.empty() && request.GetBody != nil && request.ContentLength != 0 {
		if body, err := request.GetBody(); err == nil {
			_, _ = io.Copy(tapWriter{c.req}, io.LimitReader(body, int64(c.settings.rawLimit())+1))
			_ = body.Close()
		}
	}
	attrs := c.req.attributes("request", request.Header.Get("Content-Type"), request.Header.Get("Content-Encoding"))
	if resp != nil {
		attrs = append(attrs, c.res.attributes("response", resp.Header.Get("Content-Type"), resp.Header.Get("Content-Encoding"))...)
	}
	if len(attrs) > 0 {
		span.SetAttributes(attrs...)
	}
}

// captureTransport sits under otelhttp's transport, where the call's span is in the context.
type captureTransport struct{ base http.RoundTripper }

func (t captureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c, _ := r.Context().Value(clientBodiesKey{}).(*clientBodies)
	if c == nil {
		return t.base.RoundTrip(r)
	}
	c.mu.Lock()
	c.span = trace.SpanFromContext(r.Context())
	c.request = r
	c.mu.Unlock()
	if r.Body != nil && r.Body != http.NoBody {
		r = r.Clone(r.Context())
		r.Body = &tapReader{ReadCloser: r.Body, tap: c.req}
	}
	resp, err := t.base.RoundTrip(r)
	if err != nil || resp == nil {
		c.finish() // no response: the request body is all there is
		return resp, err
	}
	c.mu.Lock()
	c.response = resp
	c.mu.Unlock()
	if !c.settings.wanted(resp.StatusCode) {
		c.finish()
		return resp, nil
	}
	c.res = newTap(c.settings, func([]byte) string { return resp.Header.Get("Content-Type") })
	if resp.Body == nil || resp.Body == http.NoBody {
		c.finish()
		return resp, nil
	}
	resp.Body = &tapReader{ReadCloser: resp.Body, tap: c.res, done: c.finish}
	return resp, nil
}

// closeFirst finishes the call's bodies when the app closes the response body without reading
// to its end — before otelhttp's wrapper, which ends the span on Close.
type closeFirst struct {
	io.ReadCloser
	c *clientBodies
}

func (b *closeFirst) Close() error {
	b.c.finish()
	return b.ReadCloser.Close()
}

// ── URLs on every span ────────────────────────────────────────────────────────

// urlRedactingExporter masks secret query values in every span's URL attributes before they are
// sent, whatever instrumentation set them and whether or not CaptureBodies is on.
type urlRedactingExporter struct{ sdktrace.SpanExporter }

func (e urlRedactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := spans
	copied := false
	for i, s := range spans {
		if attrs, changed := redactURLAttributes(s.Attributes()); changed {
			if !copied {
				out = append([]sdktrace.ReadOnlySpan(nil), spans...)
				copied = true
			}
			out[i] = redactedSpan{ReadOnlySpan: s, attrs: attrs}
		}
	}
	return e.SpanExporter.ExportSpans(ctx, out)
}

type redactedSpan struct {
	sdktrace.ReadOnlySpan
	attrs []attribute.KeyValue
}

func (s redactedSpan) Attributes() []attribute.KeyValue { return s.attrs }

var urlKeys = map[attribute.Key]bool{"url.full": true, "http.url": true, "http.target": true}

func redactURLAttributes(attrs []attribute.KeyValue) ([]attribute.KeyValue, bool) {
	var out []attribute.KeyValue
	for i, kv := range attrs {
		if kv.Value.Type() != attribute.STRING {
			continue
		}
		value := kv.Value.AsString()
		redacted := value
		switch {
		case urlKeys[kv.Key]:
			redacted = redactURL(value)
		case kv.Key == "url.query":
			bare := strings.TrimPrefix(value, "?")
			if r := redactQuery(bare); r != bare {
				redacted = strings.TrimSuffix(value, bare) + r
			}
		}
		if redacted != value {
			if out == nil {
				out = append([]attribute.KeyValue(nil), attrs...)
			}
			out[i] = attribute.String(string(kv.Key), redacted)
		}
	}
	if out == nil {
		return attrs, false
	}
	return out, true
}
