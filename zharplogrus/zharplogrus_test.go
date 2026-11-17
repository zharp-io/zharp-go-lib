package zharplogrus

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/sirupsen/logrus"
	zharp "github.com/zharp-io/zharp-go-lib"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
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

func TestHookSendsWithLevelFieldsAndTraceOnce(t *testing.T) {
	var printed bytes.Buffer
	toBuffer := logrus.New()
	toBuffer.SetOutput(&printed)
	toBuffer.SetFormatter(&logrus.JSONFormatter{})
	Install(toBuffer) // before Init: follows whichever Init is active

	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "logrus-unit", SpanExporter: spans, LogExporter: logs, CaptureOutput: true, Logs: zharp.Bool(true)})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// Text output to the captured stdout: the printed line must not be sent again.
	logger := logrus.New()
	logger.SetOutput(os.Stdout)
	Install(logger)

	ctx, span := zharp.Tracer().Start(context.Background(), "checkout")
	logger.WithContext(ctx).WithField("order_id", 42).Info("logrus info once")
	logger.Debug("logrus debug below the logger's level")
	logger.WithError(errors.New("card declined")).Error("logrus error once")
	toBuffer.WithContext(ctx).Warn("logrus to a buffer")
	span.End()
	_ = zharp.Flush(context.Background())

	got := logs.byBody()
	for body, want := range map[string]string{"logrus info once": "info", "logrus error once": "error", "logrus to a buffer": "warning"} {
		rs := got[body]
		if len(rs) != 1 {
			t.Fatalf("%q sent %d times", body, len(rs))
		}
		if rs[0].SeverityText() != want {
			t.Errorf("%q level %q", body, rs[0].SeverityText())
		}
	}
	if len(got["logrus debug below the logger's level"]) != 0 {
		t.Error("debug entry sent although the logger is at info")
	}
	info := got["logrus info once"][0]
	if info.TraceID() != span.SpanContext().TraceID() || info.SpanID() != span.SpanContext().SpanID() {
		t.Errorf("trace %s span %s", info.TraceID(), info.SpanID())
	}
	if a := attrs(info); a["order_id"] != "42" {
		t.Errorf("attrs %v", a)
	}
	if a := attrs(got["logrus error once"][0]); !strings.Contains(a["exception.message"], "card declined") {
		t.Errorf("error attrs %v", a)
	}
	for body, rs := range got {
		for _, r := range rs {
			if _, ok := attrs(r)["log.iostream"]; ok && strings.Contains(body, "logrus") {
				t.Errorf("printed logrus line sent again: %q", body)
			}
		}
	}
	if !strings.Contains(printed.String(), `"msg":"logrus to a buffer"`) {
		t.Errorf("printed output changed: %s", printed.String())
	}
}

func TestLogsOffStillStamps(t *testing.T) {
	t.Setenv("ZHARP_LOGS", "")
	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "logrus-unit", SpanExporter: spans, LogExporter: logs})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	var text, js bytes.Buffer
	textLogger := logrus.New()
	textLogger.SetOutput(&text)
	textLogger.SetFormatter(&logrus.TextFormatter{DisableColors: true})
	jsonLogger := logrus.New()
	jsonLogger.SetOutput(&js)
	jsonLogger.SetFormatter(&logrus.JSONFormatter{})
	Install(textLogger, jsonLogger)
	Install(textLogger) // twice: still one hook, one stamp

	ctx, span := zharp.Tracer().Start(context.Background(), "checkout")
	textLogger.WithContext(ctx).WithField("order_id", 42).Info("logrus in a request")
	textLogger.Info("logrus outside a request")
	jsonLogger.WithContext(ctx).Info("logrus json in a request")
	span.End()
	_ = zharp.Flush(context.Background())

	if n := len(logs.byBody()); n != 0 {
		t.Errorf("%d records sent with logs off", n)
	}
	tid, sid := span.SpanContext().TraceID().String(), span.SpanContext().SpanID().String()
	lines := strings.Split(strings.TrimSpace(text.String()), "\n")
	if !strings.HasSuffix(lines[0], "order_id=42 trace_id="+tid+" span_id="+sid) || strings.Contains(lines[1], "trace_id") {
		t.Errorf("text lines:\n%s\n%s", lines[0], lines[1])
	}
	if !strings.HasSuffix(strings.TrimSpace(js.String()), `"trace_id":"`+tid+`","span_id":"`+sid+`"}`) {
		t.Errorf("json line: %s", js.String())
	}
}
