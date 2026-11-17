// Package zharpzap sends zap logs to Zharp, with their level, fields and — when the log is given
// the request's context — its trace and span.
//
//	shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api"})
//	defer shutdown(context.Background())
//
//	logger := zharpzap.Wrap(zap.Must(zap.NewProduction()))
//	logger.Info("Order placed", zap.Int("order_id", 42), zharpzap.Context(r.Context()))
//
// Wrap keeps everything the logger already does. An entry logged with the request's context ends
// with trace_id and span_id fields — the last fields of a JSON line — so the Zharp agent links the
// printed line to its request (zharp.Config.LogCorrelation). With zharp.Config.Logs on, each entry
// the logger's level lets through is also sent to Zharp. Built on the OpenTelemetry otelzap bridge.
package zharpzap

import (
	"context"

	zharp "github.com/zharp-io/zharp-go-lib"
	"go.opentelemetry.io/contrib/bridges/otelzap"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const scopeName = "github.com/zharp-io/zharp-go-lib/zharpzap"

// Core returns a zapcore.Core that sends every entry to Zharp — to tee with your own cores. It may
// be made before zharp.Init; entries go to whichever Init is active, when its Logs is on.
func Core() zapcore.Core {
	return otelzap.NewCore(scopeName, otelzap.WithLoggerProvider(zharp.LoggerProvider()), otelzap.WithVersion(zharp.Version))
}

// Wrap returns logger with its lines stamped with the request's ids and, with Logs on, sending to
// Zharp as well, at the levels logger already logs. A nil logger gives a logger that only sends.
func Wrap(logger *zap.Logger) *zap.Logger {
	if logger == nil {
		return zap.New(Core())
	}
	return logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		// Zharp first: an entry is sent before it is printed, so Config.CaptureOutput knows the
		// printed line is the same entry.
		return zapcore.NewTee(gated{Core: Core(), enabler: c}, stamped{Core: c})
	}))
}

// Context is a field that gives the entry the request's trace and span (and its user and
// session), and stamps the printed line with them. Encoders skip the field itself.
//
//	logger.Info("Order placed", zharpzap.Context(ctx))
func Context(ctx context.Context) zap.Field {
	return zap.Field{Key: "context", Type: zapcore.SkipType, Interface: ctx}
}

// stamped adds trace_id and span_id as the last fields of an entry logged with a request's
// context (a Context field, on the entry or from With), unless it has a trace_id already.
type stamped struct {
	zapcore.Core
	ctx context.Context
}

func contextOf(fields []zapcore.Field) context.Context {
	var ctx context.Context
	for _, f := range fields {
		if c, ok := f.Interface.(context.Context); ok {
			ctx = c
		}
	}
	return ctx
}

func (s stamped) With(fields []zapcore.Field) zapcore.Core {
	ctx := s.ctx
	if c := contextOf(fields); c != nil {
		ctx = c
	}
	return stamped{Core: s.Core.With(fields), ctx: ctx}
}

func (s stamped) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if s.Core.Enabled(e.Level) {
		return ce.AddCore(e, s)
	}
	return ce
}

func (s stamped) Write(e zapcore.Entry, fields []zapcore.Field) error {
	ctx := s.ctx
	if c := contextOf(fields); c != nil {
		ctx = c
	}
	if traceID, spanID, ok := zharp.LogIDs(ctx); ok && !hasTraceID(fields) {
		fields = append(fields[:len(fields):len(fields)], zap.String("trace_id", traceID), zap.String("span_id", spanID))
	}
	// Through the core's own Check, so its sampling and level rules apply as before.
	if checked := s.Core.Check(e, nil); checked != nil {
		checked.Write(fields...)
	}
	return nil
}

func hasTraceID(fields []zapcore.Field) bool {
	for _, f := range fields {
		if f.Key == "trace_id" {
			return true
		}
	}
	return false
}

// gated sends only what the wrapped core would log.
type gated struct {
	zapcore.Core
	enabler zapcore.LevelEnabler
}

func (g gated) Enabled(level zapcore.Level) bool {
	return g.enabler.Enabled(level) && g.Core.Enabled(level)
}

func (g gated) With(fields []zapcore.Field) zapcore.Core {
	return gated{Core: g.Core.With(fields), enabler: g.enabler}
}

func (g gated) Check(e zapcore.Entry, ce *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if !g.enabler.Enabled(e.Level) {
		return ce
	}
	return g.Core.Check(e, ce)
}
