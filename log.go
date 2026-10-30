package zharp

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/contrib/bridges/otelslog"
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
	if previous, ok := console.(fanout); ok && len(previous) > 0 { // Init called again
		console = previous[0]
	}
	if fmt.Sprintf("%T", console) == "*slog.defaultHandler" {
		console = slog.NewTextHandler(os.Stderr, nil)
	}
	slog.SetDefault(slog.New(fanout{console, zharpHandler}))
}

// fanout sends each record to every handler that wants it.
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
