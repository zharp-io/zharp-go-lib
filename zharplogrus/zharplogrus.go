// Package zharplogrus sends logrus logs to Zharp, with their level, fields and — when the entry
// carries the request's context — its trace and span.
//
//	shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api"})
//	defer shutdown(context.Background())
//	zharplogrus.Install() // the standard logger; or Install(myLogger)
//
//	logrus.WithContext(r.Context()).WithField("order_id", 42).Info("Order placed")
//
// An entry logged with the request's context (WithContext) is printed ending with
// " trace_id=… span_id=…" — or with those fields, for the JSON formatter — so the Zharp agent links
// the line to its request (zharp.Config.LogCorrelation). With zharp.Config.Logs on, a hook also
// sends each entry the logger's level lets through to Zharp. Built on the OpenTelemetry otellogrus
// bridge.
package zharplogrus

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	zharp "github.com/zharp-io/zharp-go-lib"
	"go.opentelemetry.io/contrib/bridges/otellogrus"
)

const scopeName = "github.com/zharp-io/zharp-go-lib/zharplogrus"

// stampFormatter ends a line logged in a request with its trace_id and span_id.
type stampFormatter struct{ next logrus.Formatter }

func (f stampFormatter) Format(e *logrus.Entry) ([]byte, error) {
	b, err := f.next.Format(e)
	if err != nil {
		return b, err
	}
	return zharp.StampLine(e.Context, b), nil
}

// Hook returns a logrus hook that sends every entry to Zharp. It may be made before zharp.Init;
// entries go to whichever Init is active (nowhere before Init).
func Hook() logrus.Hook {
	return otellogrus.NewHook(scopeName, otellogrus.WithLoggerProvider(zharp.LoggerProvider()), otellogrus.WithVersion(zharp.Version))
}

var exitOnce sync.Once

func hooked(l *logrus.Logger) bool {
	for _, hooks := range l.Hooks {
		for _, h := range hooks {
			if _, ok := h.(*otellogrus.Hook); ok {
				return true
			}
		}
	}
	return false
}

// Install stamps each logger's lines and adds the hook (the standard logger when none is given),
// and has logrus.Fatal send what is queued before the process exits. Call Install after setting a
// formatter: SetFormatter replaces the stamping one.
func Install(loggers ...*logrus.Logger) {
	if len(loggers) == 0 {
		loggers = []*logrus.Logger{logrus.StandardLogger()}
	}
	for _, l := range loggers {
		if l == nil {
			continue
		}
		if _, ok := l.Formatter.(stampFormatter); !ok && l.Formatter != nil {
			l.SetFormatter(stampFormatter{next: l.Formatter})
		}
		if !hooked(l) {
			l.AddHook(Hook())
		}
	}
	exitOnce.Do(func() {
		logrus.RegisterExitHandler(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			_ = zharp.Flush(ctx)
		})
	})
}
