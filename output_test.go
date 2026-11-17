package zharp

import (
	"context"
	"fmt"
	"log"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// resetOutput tears the pipes down completely, so the next test starts from its own files.
func resetOutput() {
	stopOutputCapture()
	outMu.Lock()
	for _, s := range streams {
		_ = s.w.Close()
	}
	streams = nil
	outMu.Unlock()
}

// withOutputFiles makes os.Stdout and os.Stderr temp files for the test — the "terminal" the
// capture must keep printing to — and returns readers of what reached them.
func withOutputFiles(t *testing.T) (stdout, stderr func() string) {
	t.Helper()
	dir := t.TempDir()
	out, err := os.Create(dir + "/stdout")
	if err != nil {
		t.Fatal(err)
	}
	errFile, err := os.Create(dir + "/stderr")
	if err != nil {
		t.Fatal(err)
	}
	savedOut, savedErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = out, errFile
	t.Cleanup(func() {
		resetOutput()
		os.Stdout, os.Stderr = savedOut, savedErr
		_ = out.Close()
		_ = errFile.Close()
	})
	read := func(name string) func() string {
		return func() string {
			b, _ := os.ReadFile(dir + "/" + name)
			return string(b)
		}
	}
	return read("stdout"), read("stderr")
}

func byBody(records []sdklog.Record) map[string][]sdklog.Record {
	out := map[string][]sdklog.Record{}
	for _, r := range records {
		out[r.Body().AsString()] = append(out[r.Body().AsString()], r)
	}
	return out
}

func TestCaptureOutputSendsPrintedLines(t *testing.T) {
	stdout, stderr := withOutputFiles(t)
	_, logs := setupLogsOnly(t, Config{CaptureOutput: true})

	fmt.Println("plain stdout line")
	fmt.Fprintln(os.Stderr, "plain stderr line")
	fmt.Println(`{"level":"error","msg":"json failure","trace_id":"` + traceID + `","span_id":"` + parentID + `"}`)
	fmt.Println(`time=2026-11-17T08:00:00Z level=WARN msg="text warn"`)
	fmt.Println("\x1b[31mERROR\x1b[0m coloured failure")
	log.New(os.Stdout, "", 0).Println("[debug] from a log.Logger")
	fmt.Fprintln(os.Stderr, "E1117 08:33:00.123456   42 main.go:10] klog failure")
	fmt.Fprintln(os.Stderr, "[Zharp] the SDK talking about itself")
	fmt.Print("unfinished ")
	fmt.Print("line")

	// The unfinished line goes on its own after 200 ms, without a Flush.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(byBody(logs.all())["unfinished line"]) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if err := Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := byBody(logs.all())
	want := []struct{ body, level, source string }{
		{"plain stdout line", "INFO", "stdout"},
		{"plain stderr line", "WARN", "stderr"},
		{"json failure", "ERROR", "stdout"},
		{`time=2026-11-17T08:00:00Z level=WARN msg="text warn"`, "WARN", "stdout"},
		{"ERROR coloured failure", "ERROR", "stdout"},
		{"[debug] from a log.Logger", "DEBUG", "stdout"},
		{"E1117 08:33:00.123456   42 main.go:10] klog failure", "ERROR", "stderr"},
		{"unfinished line", "INFO", "stdout"},
	}
	for _, w := range want {
		rs := got[w.body]
		if len(rs) != 1 {
			t.Errorf("%q sent %d times; all: %v", w.body, len(rs), keys(got))
			continue
		}
		r := rs[0]
		a := attrs(r)
		if r.SeverityText() != w.level || a["log.source"] != w.source || a["log.iostream"] != w.source {
			t.Errorf("%q: level %s source %s", w.body, r.SeverityText(), a["log.source"])
		}
		if w.body == "json failure" {
			if r.TraceID().String() != traceID || r.SpanID().String() != parentID {
				t.Errorf("json line trace %s span %s", r.TraceID(), r.SpanID())
			}
		} else if r.TraceID().IsValid() {
			t.Errorf("%q has trace %s with no context", w.body, r.TraceID())
		}
	}
	for body := range got {
		if strings.Contains(body, "[Zharp]") {
			t.Errorf("SDK's own line sent: %q", body)
		}
	}

	// Everything still printed, byte for byte.
	wantOut := "plain stdout line\n" +
		`{"level":"error","msg":"json failure","trace_id":"` + traceID + `","span_id":"` + parentID + `"}` + "\n" +
		`time=2026-11-17T08:00:00Z level=WARN msg="text warn"` + "\n" +
		"\x1b[31mERROR\x1b[0m coloured failure\n" +
		"[debug] from a log.Logger\n" +
		"unfinished line"
	if out := stdout(); out != wantOut {
		t.Errorf("stdout changed:\n%q\nwant\n%q", out, wantOut)
	}
	if errOut := stderr(); !strings.Contains(errOut, "plain stderr line\n") ||
		!strings.Contains(errOut, "klog failure\n") || !strings.Contains(errOut, "[Zharp] the SDK talking about itself\n") {
		t.Errorf("stderr changed: %q", errOut)
	}
}

func keys(m map[string][]sdklog.Record) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestCaptureOutputDoesNotResendExactCaptures(t *testing.T) {
	stdout, _ := withOutputFiles(t)
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	_, logs := setupLogsOnly(t, Config{CaptureOutput: true})

	// A JSON handler on (the captured) stdout, set after Init and wrapped.
	slog.SetDefault(slog.New(WrapSlogHandler(slog.NewJSONHandler(os.Stdout, nil))))
	ctx, span := Tracer().Start(context.Background(), "checkout")
	slog.InfoContext(ctx, `wrapped slog says "hi" once`, "order_id", 42)
	slog.ErrorContext(ctx, "wrapped slog error once")
	log.Print("log package once") // the log package goes through slog's default
	span.End()
	fmt.Println("printed with fmt once")
	if err := Flush(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := byBody(logs.all())
	for _, body := range []string{`wrapped slog says "hi" once`, "wrapped slog error once", "log package once", "printed with fmt once"} {
		if n := len(got[body]); n != 1 {
			t.Errorf("%q sent %d times", body, n)
		}
	}
	for body, rs := range got {
		for _, r := range rs {
			if attrs(r)["log.source"] == "stdout" && body != "printed with fmt once" {
				t.Errorf("printed copy of an exact capture sent: %q", body)
			}
		}
	}
	if rs := got[`wrapped slog says "hi" once`]; len(rs) == 1 {
		r := rs[0]
		if r.TraceID() != span.SpanContext().TraceID() || r.SpanID() != span.SpanContext().SpanID() {
			t.Errorf("wrapped slog record trace %s span %s, want %s", r.TraceID(), r.SpanID(), span.SpanContext().TraceID())
		}
		if attrs(r)["order_id"] != "42" {
			t.Errorf("attrs %v", attrs(r))
		}
	}
	if rs := got["wrapped slog error once"]; len(rs) == 1 && rs[0].SeverityText() != "ERROR" {
		t.Errorf("error level %s", rs[0].SeverityText())
	}
	out := stdout()
	for _, s := range []string{`"msg":"wrapped slog says \"hi\" once"`, `"msg":"wrapped slog error once"`, `"msg":"log package once"`, "printed with fmt once\n"} {
		if !strings.Contains(out, s) {
			t.Errorf("stdout lacks %s: %q", s, out)
		}
	}
}

func TestCaptureOutputOffByDefaultAndFromEnv(t *testing.T) {
	withOutputFiles(t)
	before := os.Stdout
	_, logs := setupLogsOnly(t, Config{})
	if os.Stdout != before {
		t.Fatal("os.Stdout swapped with CaptureOutput off")
	}
	fmt.Println("not captured")
	_ = Flush(context.Background())
	if len(byBody(logs.all())["not captured"]) != 0 {
		t.Error("line sent with CaptureOutput off")
	}

	t.Setenv("ZHARP_CAPTURE_OUTPUT", "true")
	_, logs = setupLogsOnly(t, Config{})
	if os.Stdout == before {
		t.Fatal("ZHARP_CAPTURE_OUTPUT=true did not capture")
	}
	fmt.Println("captured from env")
	_ = Flush(context.Background())
	if len(byBody(logs.all())["captured from env"]) != 1 {
		t.Error("line not sent with ZHARP_CAPTURE_OUTPUT=true")
	}
}

func TestShutdownRestoresOutput(t *testing.T) {
	withOutputFiles(t)
	before := os.Stdout
	logs := &memoryLogs{}
	shutdown := Init(Config{Key: "zpk_test", Service: "go-unit", LogExporter: logs, CaptureOutput: true})
	kept := os.Stdout
	fmt.Fprint(os.Stdout, "last words")
	if err := shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if os.Stdout != before {
		t.Error("os.Stdout not restored on shutdown")
	}
	if len(byBody(logs.all())["last words"]) != 1 {
		t.Error("unfinished line not sent on shutdown")
	}
	// Whoever kept hold of the pipe can still print.
	if _, err := fmt.Fprintln(kept, "still printing"); err != nil {
		t.Errorf("write to a kept pipe after shutdown: %v", err)
	}
}

func TestParseOutputLine(t *testing.T) {
	for _, c := range []struct{ line, level, body string }{
		{`{"level":"warn","msg":"zap json"}`, "WARN", "zap json"},
		{`{"level":"error","message":"zerolog json"}`, "ERROR", "zerolog json"},
		{`{"severity":"CRITICAL","message":"gcp"}`, "FATAL", "gcp"},
		{`{"level":50,"msg":"pino"}`, "ERROR", "pino"},
		{`{"level":"info"}`, "INFO", `{"level":"info"}`},
		{`time="2026" level=error msg="logrus text"`, "ERROR", `time="2026" level=error msg="logrus text"`},
		{"2026-11-17T08:00:00.000Z\tWARN\tzap console", "WARN", "2026-11-17T08:00:00.000Z\tWARN\tzap console"},
		{"8:00AM INF zerolog console", "INFO", "8:00AM INF zerolog console"},
		{"8:00AM \x1b[31mERR\x1b[0m zerolog colour", "ERROR", "8:00AM ERR zerolog colour"},
		{"2026/11/17 08:00:00 [warn] std log", "WARN", "2026/11/17 08:00:00 [warn] std log"},
		{"W1117 08:33:00.1 klog", "WARN", "W1117 08:33:00.1 klog"},
		{"panic: something broke", "FATAL", "panic: something broke"},
		{"Listening on :8080", "", "Listening on :8080"},
		{"information desk", "", "information desk"},
	} {
		p := parseOutputLine(c.line)
		level := ""
		if p.level != nil {
			level = p.level.text
		}
		if level != c.level || p.body != c.body {
			t.Errorf("%q → %q %q, want %q %q", c.line, level, p.body, c.level, c.body)
		}
	}
}
