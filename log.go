package zharp

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

// Log sends logs to Zharp only (not the console), with the trace, user and session of ctx.
// Arguments after the message are slog-style key-value pairs:
//
//	zharp.Log.Info(ctx, "Order placed", "order_id", 42)
//
// It is sent whatever Config.Logs says. slog's default is stamped after Init — slog.InfoContext
// prints with the request's trace_id and span_id at the end — and, with Config.Logs on, sent too.
var Log logger

type logger struct{}

func (logger) Debug(ctx context.Context, msg string, args ...any) {
	emit(ctx, slog.LevelDebug, msg, args)
}
func (logger) Info(ctx context.Context, msg string, args ...any) {
	emit(ctx, slog.LevelInfo, msg, args)
}
func (logger) Warn(ctx context.Context, msg string, args ...any) {
	emit(ctx, slog.LevelWarn, msg, args)
}
func (logger) Error(ctx context.Context, msg string, args ...any) {
	emit(ctx, slog.LevelError, msg, args)
}

// Fatal logs at fatal severity. Unlike log.Fatal it does not exit.
func (logger) Fatal(ctx context.Context, msg string, args ...any) {
	emit(ctx, slog.LevelError+4, msg, args)
}

func emit(ctx context.Context, level slog.Level, msg string, args []any) {
	s := active()
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.slog.Log(ctx, level, msg, args...)
}

// SlogHandler returns a slog.Handler that sends to Zharp, for building your own logger — e.g. to
// send one logger's records without changing slog's default. Nil before Init.
func SlogHandler() slog.Handler {
	s := active()
	if s == nil {
		return nil
	}
	return s.slog.Handler()
}

func newOtelSlogHandler(lp *sdklog.LoggerProvider) slog.Handler {
	return otelslog.NewHandler(scopeName, otelslog.WithLoggerProvider(lp), otelslog.WithVersion(Version))
}

// installSlogDefault makes slog.Default print as before, stamped with the request's ids
// (correlation), and send to Zharp (logs). slog's built-in default handler can't be wrapped (it
// writes through the log package, which SetDefault points back at slog), so the console side is
// then a text handler on stderr.
func installSlogDefault(logs, correlation bool) {
	console := unwrapOurs(slog.Default().Handler()) // Init called again
	if !logs && !correlation {
		if console != slog.Default().Handler() {
			slog.SetDefault(slog.New(console))
		}
		return
	}
	if fmt.Sprintf("%T", console) == "*slog.defaultHandler" {
		console = slog.NewTextHandler(originalStderr(), nil)
	}
	slog.SetDefault(slog.New(WrapSlogHandler(console)))
}

func unwrapOurs(h slog.Handler) slog.Handler {
	for {
		switch x := h.(type) {
		case fanout:
			if len(x) != 2 {
				return h
			}
			if _, ok := x[0].(*liveHandler); !ok {
				return h
			}
			h = x[1]
		case stampHandler:
			h = x.next
		default:
			return h
		}
	}
}

// WrapSlogHandler returns h stamped and sending: each record logged during a request is printed
// by h with trace_id and span_id as its last attributes (unless LogCorrelation is off or it has a
// trace_id already), and, with Config.Logs on, sent to Zharp first. For a logger of your own, or a
// slog.SetDefault made after Init:
//
//	slog.SetDefault(slog.New(zharp.WrapSlogHandler(slog.NewJSONHandler(os.Stdout, nil))))
//
// It may be made before Init: it follows whichever Init is active when a record is logged (and
// does nothing extra before Init). Records keep the trace and span of the context they are logged
// with (slog.InfoContext). A nil h only sends.
func WrapSlogHandler(h slog.Handler) slog.Handler {
	if h == nil {
		return &liveHandler{cache: &liveCache{}}
	}
	return fanout{&liveHandler{cache: &liveCache{}}, stampHandler{next: h}}
}

// LogIDs returns the trace and span ids to stamp on a line logged with ctx: ok is false outside a
// request, before Init, or with LogCorrelation off. For logging integrations.
func LogIDs(ctx context.Context) (traceID, spanID string, ok bool) {
	s := active()
	if s == nil || !s.correlation || ctx == nil {
		return "", "", false
	}
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return "", "", false
	}
	return sc.TraceID().String(), sc.SpanID().String(), true
}

// StampLine returns a formatted log line ending with " trace_id=… span_id=…" — or, for a JSON
// record, with "trace_id" and "span_id" fields — when ctx is in a request (see LogIDs). A line
// that already mentions trace_id, a blank line and the tail of a JSON record are returned as they
// are; a trailing newline is kept. For logging integrations whose formatter can't add fields.
func StampLine(ctx context.Context, line []byte) []byte {
	traceID, spanID, ok := LogIDs(ctx)
	if !ok {
		return line
	}
	return stampLine(line, traceID, spanID)
}

const trailingSpace = " \t\r\n"

func stampLine(line []byte, traceID, spanID string) []byte {
	body := bytes.TrimRight(line, trailingSpace)
	tail := line[len(body):]
	if len(bytes.TrimSpace(body)) == 0 || bytes.Contains(body, []byte("trace_id")) {
		return line
	}
	out := make([]byte, 0, len(line)+70)
	if body[len(body)-1] == '}' {
		trimmed := bytes.TrimLeft(body, " \t")
		if trimmed[0] != '{' {
			return line
		}
		inner := bytes.TrimSpace(body[bytes.IndexByte(body, '{')+1 : len(body)-1])
		out = append(out, body[:len(body)-1]...)
		if len(inner) > 0 {
			out = append(out, ',')
		}
		out = append(out, `"trace_id":"`+traceID+`","span_id":"`+spanID+`"}`...)
	} else {
		out = append(out, body...)
		out = append(out, " trace_id="+traceID+" span_id="+spanID...)
	}
	return append(out, tail...)
}

// stampHandler adds trace_id and span_id as a record's last attributes when it is logged in a
// request, so a text handler ends the line with them and a JSON handler has them as fields.
type stampHandler struct {
	next    slog.Handler
	grouped bool // attributes now land in a group: a stamp there would not be found
}

func (h stampHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.next.Enabled(ctx, level)
}

func (h stampHandler) Handle(ctx context.Context, r slog.Record) error {
	if traceID, spanID, ok := LogIDs(ctx); ok && !h.grouped {
		has := false
		r.Attrs(func(a slog.Attr) bool {
			has = a.Key == "trace_id"
			return !has
		})
		if !has {
			r = r.Clone()
			r.AddAttrs(slog.String("trace_id", traceID), slog.String("span_id", spanID))
		}
	}
	return h.next.Handle(ctx, r)
}

func (h stampHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return stampHandler{next: h.next.WithAttrs(attrs), grouped: h.grouped}
}

func (h stampHandler) WithGroup(name string) slog.Handler {
	return stampHandler{next: h.next.WithGroup(name), grouped: h.grouped || name != ""}
}

// liveHandler is the active Init's Zharp handler, with the attributes and groups given to it.
type liveHandler struct {
	ops   []func(slog.Handler) slog.Handler
	cache *liveCache
}

type liveCache struct {
	mu      sync.Mutex
	state   *state
	handler slog.Handler
}

func (l *liveHandler) resolve() slog.Handler {
	s := active()
	if s == nil || !s.logs {
		return nil
	}
	l.cache.mu.Lock()
	defer l.cache.mu.Unlock()
	if l.cache.state != s {
		h := s.slog.Handler()
		for _, op := range l.ops {
			h = op(h)
		}
		l.cache.state, l.cache.handler = s, h
	}
	return l.cache.handler
}

func (l *liveHandler) Enabled(ctx context.Context, level slog.Level) bool {
	h := l.resolve()
	return h != nil && h.Enabled(ctx, level)
}

func (l *liveHandler) Handle(ctx context.Context, r slog.Record) error {
	if h := l.resolve(); h != nil {
		return h.Handle(ctx, r)
	}
	return nil
}

func (l *liveHandler) with(op func(slog.Handler) slog.Handler) slog.Handler {
	ops := append(append([]func(slog.Handler) slog.Handler(nil), l.ops...), op)
	return &liveHandler{ops: ops, cache: &liveCache{}}
}

func (l *liveHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return l.with(func(h slog.Handler) slog.Handler { return h.WithAttrs(attrs) })
}

func (l *liveHandler) WithGroup(name string) slog.Handler {
	return l.with(func(h slog.Handler) slog.Handler { return h.WithGroup(name) })
}

// fanout sends each record to every handler that wants it, in order. Zharp's handler comes first,
// so a record is sent (and remembered) before the console prints it.
type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range f {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (f fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(attrs)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}

// LoggerProvider returns an OpenTelemetry LoggerProvider that sends to whichever Init is active
// when a record is emitted, if its Config.Logs is on (nothing before Init or with Logs off) — for logging bridges such as otelzap and
// otellogrus. Unlike global.GetLoggerProvider, loggers made from it before Init, or before Init
// is called again, follow the current one. The zharpzap, zharplogrus and zharpzerolog modules use
// it.
func LoggerProvider() otellog.LoggerProvider { return liveProvider{} }

type liveProvider struct{ embedded.LoggerProvider }

func (liveProvider) Logger(name string, opts ...otellog.LoggerOption) otellog.Logger {
	return &liveLogger{name: name, opts: opts}
}

type liveLogger struct {
	embedded.Logger
	name string
	opts []otellog.LoggerOption

	mu     sync.Mutex
	state  *state
	logger otellog.Logger
}

func (l *liveLogger) resolve() otellog.Logger {
	s := active()
	if s == nil || !s.logs {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.state != s {
		l.state, l.logger = s, s.loggerProvider.Logger(l.name, l.opts...)
	}
	return l.logger
}

func (l *liveLogger) Emit(ctx context.Context, r otellog.Record) {
	if logger := l.resolve(); logger != nil {
		if ctx == nil {
			ctx = context.Background()
		}
		logger.Emit(ctx, r)
	}
}

func (l *liveLogger) Enabled(ctx context.Context, p otellog.EnabledParameters) bool {
	logger := l.resolve()
	if ctx == nil {
		ctx = context.Background()
	}
	return logger != nil && logger.Enabled(ctx, p)
}
