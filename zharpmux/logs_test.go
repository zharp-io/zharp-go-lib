package zharpmux

import (
	"context"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

type nopLogs struct{}

func (nopLogs) Export(context.Context, []sdklog.Record) error { return nil }
func (nopLogs) Shutdown(context.Context) error                { return nil }
func (nopLogs) ForceFlush(context.Context) error              { return nil }
