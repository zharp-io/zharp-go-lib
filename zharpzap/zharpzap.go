// Package zharpzap sends zap logs to Zharp, with their level, fields and — when the log is given
// the request's context — its trace and span.
//
//	shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api"})
//	defer shutdown(context.Background())
//
//	logger := zharpzap.Wrap(zap.Must(zap.NewProduction()))
//	logger.Info("Order placed", zap.Int("order_id", 42), zharpzap.Context(r.Context()))
//
// Wrap keeps everything the logger already does (it prints exactly as before) and sends each entry
// its own level lets through to Zharp too. Built on the OpenTelemetry otelzap bridge.
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
// be made before zharp.Init; entries go to whichever Init is active (nowhere before Init).
func Core() zapcore.Core {
	return otelzap.NewCore(scopeName, otelzap.WithLoggerProvider(zharp.LoggerProvider()), otelzap.WithVersion(zharp.Version))
}

// Wrap returns logger sending to Zharp as well, at the levels logger already logs. A nil logger
// gives a logger that sends to Zharp only.
func Wrap(logger *zap.Logger) *zap.Logger {
	if logger == nil {
		return zap.New(Core())
	}
	return logger.WithOptions(zap.WrapCore(func(c zapcore.Core) zapcore.Core {
		// Zharp first: an entry is sent before it is printed, so Config.CaptureOutput knows the
		// printed line is the same entry.
		return zapcore.NewTee(gated{Core: Core(), enabler: c}, c)
	}))
}

// Context is a field that gives the entry the request's trace and span (and its user and
// session). Encoders skip it, so it isn't printed.
//
//	logger.Info("Order placed", zharpzap.Context(ctx))
func Context(ctx context.Context) zap.Field {
	return zap.Field{Key: "context", Type: zapcore.SkipType, Interface: ctx}
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
