package zharpzerolog

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/rs/zerolog"
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

func TestWriterSendsWithLevelFieldsAndTraceOnce(t *testing.T) {
	var printed bytes.Buffer
	toBuffer := zerolog.New(Writer(&printed)).Hook(TraceHook{}) // before Init: follows the active Init

	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	shutdown := zharp.Init(zharp.Config{Key: "zpk_test", Service: "zerolog-unit", SpanExporter: spans, LogExporter: logs, CaptureOutput: true})
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// JSON to the captured stdout, and a ConsoleWriter on it: neither printed line is sent again.
	logger := zerolog.New(Writer(os.Stdout)).Hook(TraceHook{}).Level(zerolog.InfoLevel).With().Timestamp().Logger()
	console := zerolog.New(Writer(zerolog.ConsoleWriter{Out: os.Stdout, NoColor: true})).Hook(TraceHook{})

	ctx, span := zharp.Tracer().Start(context.Background(), "checkout")
	logger.Info().Ctx(ctx).Int("order_id", 42).Dict("cart", zerolog.Dict().Int("items", 3)).Msg("zerolog info once")
	logger.Debug().Msg("zerolog debug below the logger's level")
	logger.Error().Err(errors.New("card declined")).Msg("zerolog error once")
	console.Warn().Ctx(ctx).Str("region", "eu").Msg("zerolog console once")
	toBuffer.Info().Ctx(ctx).Msg("zerolog to a buffer")
	span.End()
	_ = zharp.Flush(context.Background())

	got := logs.byBody()
	for body, want := range map[string]string{"zerolog info once": "info", "zerolog error once": "error", "zerolog console once": "warn", "zerolog to a buffer": "info"} {
		rs := got[body]
		if len(rs) != 1 {
			t.Fatalf("%q sent %d times", body, len(rs))
		}
		if rs[0].SeverityText() != want {
			t.Errorf("%q level %q", body, rs[0].SeverityText())
		}
	}
	if len(got["zerolog debug below the logger's level"]) != 0 {
		t.Error("debug event sent although the logger is at info")
	}
	for _, body := range []string{"zerolog info once", "zerolog console once", "zerolog to a buffer"} {
		r := got[body][0]
		if r.TraceID() != span.SpanContext().TraceID() || r.SpanID() != span.SpanContext().SpanID() {
			t.Errorf("%q trace %s span %s", body, r.TraceID(), r.SpanID())
		}
	}
	if a := attrs(got["zerolog info once"][0]); a["order_id"] != "42" || a["cart"] != `{"items":3}` {
		t.Errorf("attrs %v", a)
	}
	if a := attrs(got["zerolog error once"][0]); !strings.Contains(a["exception.message"], "card declined") {
		t.Errorf("error attrs %v", a)
	}
	for body, rs := range got {
		for _, r := range rs {
			if _, ok := attrs(r)["log.iostream"]; ok && strings.Contains(body, "zerolog") {
				t.Errorf("printed zerolog line sent again: %q", body)
			}
		}
	}
	want := `{"level":"info","trace_id":"` + span.SpanContext().TraceID().String() + `","span_id":"` + span.SpanContext().SpanID().String() + `","message":"zerolog to a buffer"}` + "\n"
	if printed.String() != want {
		t.Errorf("printed %q\nwant    %q", printed.String(), want)
	}
}
