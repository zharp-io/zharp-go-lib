package zharp

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/embedded"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// Log sends logs to Zharp only (not the console), with the trace, user and session of ctx.
// Arguments after the message are slog-style key-value pairs:
//
//	zharp.Log.Info(ctx, "Order placed", "order_id", 42)
//
// slog is bridged too: after Init, slog.InfoContext(ctx, …) (and the log package) goes to the
// console as before and to Zharp. Use the *Context variants so logs carry the request's trace.
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

// installSlogDefault makes slog.Default write to the console as before and to Zharp. slog's
// built-in default handler can't be wrapped (it writes through the log package, which SetDefault
// points back at slog), so the console side is then a text handler on stderr.
func installSlogDefault(zharpHandler slog.Handler) {
	console := slog.Default().Handler()
	if previous, ok := console.(fanout); ok && len(previous) == 2 { // Init called again
		console = previous[1]
	}
	if fmt.Sprintf("%T", console) == "*slog.defaultHandler" {
		console = slog.NewTextHandler(originalStderr(), nil)
	}
	slog.SetDefault(slog.New(fanout{zharpHandler, console}))
}

// WrapSlogHandler returns a handler that sends each record to Zharp and then to h, unchanged — for
// a logger of your own, or a slog.SetDefault made after Init:
//
//	slog.SetDefault(slog.New(zharp.WrapSlogHandler(slog.NewJSONHandler(os.Stdout, nil))))
//
// It may be made before Init: records go to whichever Init is active when they are logged (none
// before Init). Records keep the trace and span of the context they are logged with
// (slog.InfoContext). With Config.CaptureOutput, what h prints is not sent a second time.
func WrapSlogHandler(h slog.Handler) slog.Handler {
	if h == nil {
		return &liveHandler{cache: &liveCache{}}
	}
	return fanout{&liveHandler{cache: &liveCache{}}, h}
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
	if s == nil {
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
// when a record is emitted (nothing before Init) — for logging bridges such as otelzap and
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
	if s == nil {
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
