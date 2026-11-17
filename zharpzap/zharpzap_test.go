package zharpzap

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	zharp "github.com/zharp-io/zharp-go-lib"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type memoryLogs struct {
	mu      sync.Mutex
	records []sdklog.Record
}

func (m *memoryLogs) Export(_ context.Context, records []sdklog.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range records {
		m.records = append(m.records, r.Clone())
	}
	return nil
}
func (m *memoryLogs) Shutdown(context.Context) error   { return nil }
func (m *memoryLogs) ForceFlush(context.Context) error { return nil }

func (m *memoryLogs) byBody() map[string][]sdklog.Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string][]sdklog.Record{}
	for _, r := range m.records {
		out[r.Body().AsString()] = append(out[r.Body().AsString()], r)
	}
	return out
}

func attrs(r sdklog.Record) map[string]string {
	out := map[string]string{}
	r.WalkAttributes(func(kv attribute.KeyValue) bool {
		out[string(kv.Key)] = kv.Value.Emit()
		return true
	})
	return out
}

func jsonCore(w zapcore.WriteSyncer) zapcore.Core {
	return zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), w, zapcore.InfoLevel)
}

func TestWrapSendsWithLevelFieldsAndTraceOnce(t *testing.T) {
	// The core is made before Init: it follows whichever Init is active.
	var printed bytes.Buffer
	toBuffer := Wrap(zap.New(jsonCore(zapcore.AddSync(&printed))))

	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "zap-unit", SpanExporter: spans, LogExporter: logs, CaptureOutput: true})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// Printing to the captured stdout: the printed line must not be sent again.
	logger := Wrap(zap.New(jsonCore(zapcore.Lock(os.Stdout))))
	ctx, span := zharp.Tracer().Start(context.Background(), "checkout")
	logger.Info("zap info once", zap.Int("order_id", 42), Context(ctx))
	logger.Debug("zap debug below the logger's level")
	logger.With(zap.String("region", "eu")).Error("zap error once", zap.Error(errors.New("card declined")))
	toBuffer.Warn("zap to a buffer", Context(ctx))
	span.End()
	_ = zharp.Flush(context.Background())

	got := logs.byBody()
	for body, want := range map[string]string{"zap info once": "info", "zap error once": "error", "zap to a buffer": "warn"} {
		rs := got[body]
		if len(rs) != 1 {
			t.Fatalf("%q sent %d times", body, len(rs))
		}
		if rs[0].SeverityText() != want {
			t.Errorf("%q level %q", body, rs[0].SeverityText())
		}
	}
	if len(got["zap debug below the logger's level"]) != 0 {
		t.Error("debug entry sent although the logger is at info")
	}
	info := got["zap info once"][0]
	if info.TraceID() != span.SpanContext().TraceID() || info.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("trace %s span %s, want %s %s", info.TraceID(), info.SpanID(), span.SpanContext().TraceID(), span.SpanContext().SpanID())
	}
	if a := attrs(info); a["order_id"] != "42" {
		t.Errorf("attrs %v", a)
	}
	if a := attrs(got["zap error once"][0]); a["region"] != "eu" || !strings.Contains(a["exception.message"], "card declined") {
		t.Errorf("error attrs %v", a)
	}
	for body, rs := range got {
		for _, r := range rs {
			if _, ok := attrs(r)["log.iostream"]; ok && strings.Contains(body, "zap") {
				t.Errorf("printed zap line sent again: %q", body)
			}
		}
	}
	if !strings.Contains(printed.String(), `"msg":"zap to a buffer"`) || strings.Contains(printed.String(), "context") {
		t.Errorf("printed output changed: %s", printed.String())
	}
}
