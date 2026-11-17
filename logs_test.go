package zharp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// The pattern Zharp's backend reads a stamp with.
var stampPattern = regexp.MustCompile(`(?i)\s*\btrace_id=([0-9a-f]{32})\s+span_id=([0-9a-f]{16})\s*$`)

// initRaw is Init with test exporters and nothing turned on for the test.
func initRaw(t *testing.T, cfg Config) (*tracetest.InMemoryExporter, *memoryLogs) {
	t.Helper()
	spans, logs := tracetest.NewInMemoryExporter(), &memoryLogs{}
	cfg.Key, cfg.Service, cfg.SpanExporter, cfg.LogExporter = "zpk_test", "go-unit", spans, logs
	shutdown := Init(cfg)
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	return spans, logs
}

// withDefault makes slog's default a text handler on a buffer for the test.
func withDefault(t *testing.T) *bytes.Buffer {
	t.Helper()
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	var buf bytes.Buffer
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	return &buf
}

func TestLogsOffByDefaultSendsOnlyErrorsTracesAndZharpLog(t *testing.T) {
	t.Setenv("ZHARP_LOGS", "")
	stdout, _ := withOutputFiles(t)
	printed := withDefault(t)
	before := os.Stdout
	spans, logs := initRaw(t, Config{CaptureOutput: true})
	if os.Stdout != before {
		t.Error("output captured with logs off")
	}

	var own bytes.Buffer
	wrapped := slog.New(WrapSlogHandler(slog.NewJSONHandler(&own, nil)))
	ctx, span := Tracer().Start(context.Background(), "checkout")
	slog.InfoContext(ctx, "slog default line")
	log.Print("log package line")
	wrapped.InfoContext(ctx, "wrapped handler line")
	fmt.Println("fmt line")
	var r otellog.Record
	r.SetBody(attribute.StringValue("bridge line"))
	LoggerProvider().Logger("bridge").Emit(ctx, r)
	Log.Warn(ctx, "zharp.Log line")
	CaptureError(ctx, errors.New("card declined"))
	span.End()
	_ = Flush(context.Background())

	got := byBody(logs.all())
	for _, body := range []string{"slog default line", "log package line", "wrapped handler line", "fmt line", "bridge line"} {
		if len(got[body]) != 0 {
			t.Errorf("%q sent with logs off", body)
		}
	}
	if len(got["zharp.Log line"]) != 1 {
		t.Error("zharp.Log not sent with logs off")
	}
	sawError := false
	for body := range got {
		if strings.Contains(body, "card declined") {
			sawError = true
		}
	}
	if !sawError {
		t.Errorf("error not sent with logs off: %v", keys(got))
	}
	if len(spans.GetSpans()) == 0 {
		t.Error("no spans with logs off")
	}
	// Still printed — and stamped.
	if !strings.Contains(printed.String(), "slog default line") || !strings.Contains(stdout(), "fmt line") {
		t.Errorf("output lost: %q %q", printed.String(), stdout())
	}
}

func TestLinesInARequestAreStamped(t *testing.T) {
	printed := withDefault(t)
	initRaw(t, Config{})

	var jsonOut bytes.Buffer
	jsonLogger := slog.New(WrapSlogHandler(slog.NewJSONHandler(&jsonOut, nil)))
	ctx, span := Tracer().Start(context.Background(), "checkout")
	tid, sid := span.SpanContext().TraceID().String(), span.SpanContext().SpanID().String()
	slog.InfoContext(ctx, "in a request", "order_id", 42)
	slog.InfoContext(ctx, "already has one", "trace_id", "mine")
	slog.Info("outside a request")
	slog.With("region", "eu").InfoContext(ctx, "with attrs")
	jsonLogger.InfoContext(ctx, "json in a request", "order_id", 42)
	jsonLogger.Info("json outside a request")
	span.End()

	lines := strings.Split(strings.TrimSpace(printed.String()), "\n")
	if len(lines) != 4 {
		t.Fatalf("lines %q", lines)
	}
	for _, i := range []int{0, 3} {
		m := stampPattern.FindStringSubmatch(lines[i])
		if m == nil || m[1] != tid || m[2] != sid {
			t.Errorf("line not stamped with %s/%s: %q", tid, sid, lines[i])
		}
	}
	if !strings.HasSuffix(lines[0], ` order_id=42 trace_id=`+tid+` span_id=`+sid) {
		t.Errorf("stamp not the last attributes: %q", lines[0])
	}
	if strings.Count(lines[1], "trace_id") != 1 || !strings.HasSuffix(lines[1], "trace_id=mine") {
		t.Errorf("line with its own trace_id changed: %q", lines[1])
	}
	if strings.Contains(lines[2], "trace_id") {
		t.Errorf("line outside a request stamped: %q", lines[2])
	}

	jsonLines := strings.Split(strings.TrimSpace(jsonOut.String()), "\n")
	var in, out map[string]any
	_ = json.Unmarshal([]byte(jsonLines[0]), &in)
	_ = json.Unmarshal([]byte(jsonLines[1]), &out)
	if in["trace_id"] != tid || in["span_id"] != sid || in["order_id"] != float64(42) {
		t.Errorf("json line fields %v", in)
	}
	if _, ok := out["trace_id"]; ok {
		t.Errorf("json line outside a request stamped: %v", out)
	}
}

func TestSwitches(t *testing.T) {
	stampedAndSent := func(t *testing.T, cfg Config) (stamped, sent bool) {
		printed := withDefault(t)
		_, logs := initRaw(t, cfg)
		ctx, span := Tracer().Start(context.Background(), "checkout")
		slog.InfoContext(ctx, "switch line")
		span.End()
		return strings.Contains(printed.String(), "trace_id="), len(byBody(logs.all())["switch line"]) == 1
	}
	for _, c := range []struct {
		name          string
		env           map[string]string
		cfg           Config
		stamped, sent bool
	}{
		{"default", nil, Config{}, true, false},
		{"Logs on", nil, Config{Logs: Bool(true)}, true, true},
		{"ZHARP_LOGS=true", map[string]string{"ZHARP_LOGS": "true"}, Config{}, true, true},
		{"config beats env", map[string]string{"ZHARP_LOGS": "true"}, Config{Logs: Bool(false)}, true, false},
		{"LogCorrelation off", nil, Config{LogCorrelation: Bool(false)}, false, false},
		{"ZHARP_LOG_CORRELATION=false", map[string]string{"ZHARP_LOG_CORRELATION": "false"}, Config{Logs: Bool(true)}, false, true},
		{"correlation config beats env", map[string]string{"ZHARP_LOG_CORRELATION": "false"}, Config{LogCorrelation: Bool(true)}, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("ZHARP_LOGS", "")
			t.Setenv("ZHARP_LOG_CORRELATION", "")
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			stamped, sent := stampedAndSent(t, c.cfg)
			if stamped != c.stamped || sent != c.sent {
				t.Errorf("stamped %v sent %v, want %v %v", stamped, sent, c.stamped, c.sent)
			}
		})
	}
}

func TestStampLine(t *testing.T) {
	const tid, sid = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	for _, c := range []struct{ in, want string }{
		{"hello\n", "hello trace_id=" + tid + " span_id=" + sid + "\n"},
		{"hello  \r\n", "hello trace_id=" + tid + " span_id=" + sid + "  \r\n"},
		{`{"msg":"x"}` + "\n", `{"msg":"x","trace_id":"` + tid + `","span_id":"` + sid + `"}` + "\n"},
		{`{}`, `{"trace_id":"` + tid + `","span_id":"` + sid + `"}`},
		{`tail of a record}`, `tail of a record}`},
		{"has trace_id=abc\n", "has trace_id=abc\n"},
		{"\n", "\n"},
	} {
		if got := string(stampLine([]byte(c.in), tid, sid)); got != c.want {
			t.Errorf("%q → %q, want %q", c.in, got, c.want)
		}
	}
	if !stampPattern.MatchString(strings.TrimSpace(string(stampLine([]byte("x"), tid, sid)))) {
		t.Error("stamp doesn't match the backend's pattern")
	}
}
