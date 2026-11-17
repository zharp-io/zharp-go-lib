// Package zharplogrus sends logrus logs to Zharp, with their level, fields and — when the entry
// carries the request's context — its trace and span.
//
//	shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api"})
//	defer shutdown(context.Background())
//	zharplogrus.Install() // the standard logger; or Install(myLogger)
//
//	logrus.WithContext(r.Context()).WithField("order_id", 42).Info("Order placed")
//
// The logger prints exactly as before; a hook sends each entry its level lets through to Zharp.
// Built on the OpenTelemetry otellogrus bridge.
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

// Hook returns a logrus hook that sends every entry to Zharp. It may be made before zharp.Init;
// entries go to whichever Init is active (nowhere before Init).
func Hook() logrus.Hook {
	return otellogrus.NewHook(scopeName, otellogrus.WithLoggerProvider(zharp.LoggerProvider()), otellogrus.WithVersion(zharp.Version))
}

var exitOnce sync.Once

// Install adds the hook to each logger (the standard logger when none is given), and has
// logrus.Fatal send what is queued before the process exits.
func Install(loggers ...*logrus.Logger) {
	if len(loggers) == 0 {
		loggers = []*logrus.Logger{logrus.StandardLogger()}
	}
	for _, l := range loggers {
		if l != nil {
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
