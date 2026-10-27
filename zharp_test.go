package zharp

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeZharp stands in for Zharp's ingest endpoint: it records each request and answers with status.
type fakeZharp struct {
	*httptest.Server
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
	status  int
}

func newFake(t *testing.T) *fakeZharp {
	f := &fakeZharp{status: 201}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		f.mu.Lock()
		f.bodies = append(f.bodies, decoded)
		f.headers = append(f.headers, r.Header.Clone())
		status := f.status
		f.mu.Unlock()
		w.WriteHeader(status)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeZharp) logs() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []map[string]any
	for _, b := range f.bodies {
		for _, l := range b["logs"].([]any) {
			all = append(all, l.(map[string]any))
		}
	}
	return all
}

func (f *fakeZharp) waitFor(t *testing.T, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(f.logs()) < n && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	logs := f.logs()
	if len(logs) < n {
		t.Fatalf("got %d logs, want %d", len(logs), n)
	}
	return logs
}

func start(t *testing.T, f *fakeZharp, cfg Config) {
	t.Helper()
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "ZHARP_") {
			t.Setenv(strings.SplitN(kv, "=", 2)[0], "")
		}
	}
	if cfg.APIKey == "" {
		cfg.APIKey = "test-key"
	}
	if cfg.Service == "" {
		cfg.Service = "checkout-api"
	}
	cfg.Endpoint = f.URL + "/api/v1/"
	if cfg.FlushInterval == 0 {
		cfg.FlushInterval = 50 * time.Millisecond
	}
	Init(cfg)
	SetContext(nil)
	t.Cleanup(Close)
}

func meta(log map[string]any) map[string]any { return log["metadata"].(map[string]any) }

func TestSendsTheShapeEveryZharpSDKSends(t *testing.T) {
	f := newFake(t)
	start(t, f, Config{Environment: "production", AppVersion: "2.4.0"})
	SetContext(Fields{"region": "eu"})
	Info("User signed in", Fields{"user_id": 42})
	log := f.waitFor(t, 1)[0]

	if log["service"] != "checkout-api" || log["level"] != "info" || log["message"] != "User signed in" {
		t.Fatalf("entry: %v", log)
	}
	if !regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d\.\d{3}Z$`).MatchString(log["timestamp"].(string)) {
		t.Fatalf("timestamp %v", log["timestamp"])
	}
	if log["environment"] != "production" || log["version"] != "2.4.0" {
		t.Fatalf("tags: %v", log)
	}
	if _, has := log["source"]; has {
		t.Fatal("source sent though unset")
	}
	m := meta(log)
	if m["user_id"] != float64(42) || m["sessionId"] != SessionID() || m["environment"] != "production" {
		t.Fatalf("metadata: %v", m)
	}
	ctx := m["context"].(map[string]any)
	for _, k := range []string{"hostname", "pid", "goVersion", "platform", "arch"} {
		if _, ok := ctx[k]; !ok {
			t.Errorf("context lacks %s", k)
		}
	}
	if ctx["region"] != "eu" || ctx["appVersion"] != "2.4.0" {
		t.Fatalf("context: %v", ctx)
	}
	h := f.headers[0]
	if h.Get("X-API-KEY") != "test-key" || h.Get("X-Zharp-SDK") != "go-logs/"+Version {
		t.Fatalf("headers: %v", h)
	}
}

type chargeError struct{ code string }

func (e *chargeError) Error() string { return "card declined: " + e.code }

func charge() error { return fmt.Errorf("charge order 99: %w", &chargeError{"insufficient_funds"}) }

func TestAnErrorBringsItsTypeAndWhereItWasLogged(t *testing.T) {
	f := newFake(t)
	start(t, f, Config{FlushInterval: time.Minute})
	err := charge()
	Error(err)
	CaptureError(err, Fields{"order_id": 99})
	Warn("Retrying", Fields{"err": &chargeError{"x"}})
	Flush(5 * time.Second)
	logs := f.waitFor(t, 3)

	if logs[0]["message"] != "charge order 99: card declined: insufficient_funds" {
		t.Fatalf("message %v", logs[0]["message"])
	}
	if meta(logs[0])["name"] != "error" {
		t.Fatalf("a wrapped error's name: %v", meta(logs[0])["name"])
	}
	stack := meta(logs[0])["stack"].(string)
	if !strings.Contains(stack, "TestAnErrorBringsItsTypeAndWhereItWasLogged") || strings.Contains(stack, "zharp-go-lib.logAt") {
		t.Fatalf("stack should start at the caller:\n%s", stack)
	}
	if meta(logs[1])["order_id"] != float64(99) || meta(logs[1])["stack"] == nil {
		t.Fatalf("captured: %v", meta(logs[1]))
	}
	warned := meta(logs[2])
	if logs[2]["level"] != "warn" || warned["name"] != "*zharp.chargeError" {
		t.Fatalf("warn with error field: %v", warned)
	}
	if warned["err"].(map[string]any)["message"] != "card declined: x" {
		t.Fatalf("error field: %v", warned["err"])
	}
}

type stackErr struct{}

func (stackErr) Error() string { return "with its own stack" }
func (e stackErr) Format(s fmt.State, verb rune) {
	if verb == 'v' && s.Flag('+') {
		_, _ = io.WriteString(s, "with its own stack\nmain.where()\n\tmain.go:7")
		return
	}
	_, _ = io.WriteString(s, e.Error())
}

func TestAnErrorThatCarriesAStackKeepsIt(t *testing.T) {
	stack := stackOf(fmt.Errorf("wrapped: %w", stackErr{}))
	if !strings.Contains(stack, "main.go:7") {
		t.Fatalf("%s", stack)
	}
}

func TestAStackTheCallerGaveWins(t *testing.T) {
	s := &setup{service: "s"}
	entry := buildEntry(s, LevelError, "boom", Fields{"stack": "my own"}, "", time.Unix(0, 0))
	if meta(entry)["stack"] != "my own" || entry["timestamp"] != "1970-01-01T00:00:00.000Z" {
		t.Fatalf("%v", entry)
	}
}

func TestOneLogCanGoUnderAnotherService(t *testing.T) {
	f := newFake(t)
	start(t, f, Config{})
	LogService(" payments ", "warning", "Slow", nil)
	log := f.waitFor(t, 1)[0]
	if log["service"] != "payments" || log["level"] != "warn" {
		t.Fatalf("%v", log)
	}
}

func TestEveryMessageBecomesText(t *testing.T) {
	cases := map[string]any{
		"(empty message)": nil,
		"42":              42,
		`{"a":1}`:         Fields{"a": 1},
		"boom":            errors.New("boom"),
	}
	for want, in := range cases {
		if got := toMessage(in); got != want {
			t.Errorf("toMessage(%v) = %q, want %q", in, got, want)
		}
	}
	if toMessage("  ") != "(empty message)" {
		t.Error("blank message")
	}
}

func TestFieldsJSONCannotHoldBecomeText(t *testing.T) {
	ch := make(chan int)
	out := toJSONMap(map[string]any{
		"nan":  math.NaN(),
		"when": time.Date(2026, 10, 26, 9, 0, 0, 0, time.UTC),
		"ch":   ch,
		"took": 1500 * time.Millisecond,
	})
	if _, err := json.Marshal(out); err != nil {
		t.Fatal(err)
	}
	if out["nan"] != "NaN" || out["when"] != "2026-10-26T09:00:00.000Z" || out["took"] != "1.5s" {
		t.Fatalf("%v", out)
	}
}

func TestSlogRecordsGoThroughTheHandler(t *testing.T) {
	f := newFake(t)
	start(t, f, Config{})
	logger := slog.New(NewSlogHandler(nil)).With("request_id", "r-1").WithGroup("order")
	logger.Debug("not sent")
	logger.Warn("Stock low", "sku", "SKU-1")
	logger.Error("Pricing failed", "err", &chargeError{"x"})
	logs := f.waitFor(t, 2)
	time.Sleep(100 * time.Millisecond)
	if len(f.logs()) != 2 {
		t.Fatalf("debug was sent: %v", f.logs())
	}
	if logs[0]["level"] != "warn" || logs[0]["message"] != "Stock low" {
		t.Fatalf("%v", logs[0])
	}
	m := meta(logs[0])
	if m["request_id"] != "r-1" || m["order"].(map[string]any)["sku"] != "SKU-1" {
		t.Fatalf("attributes and groups: %v", m)
	}
	if logs[1]["level"] != "error" || meta(logs[1])["name"] != "*zharp.chargeError" {
		t.Fatalf("%v", logs[1])
	}
}

func TestMiddlewareSendsAPanicAndRepanics(t *testing.T) {
	f := newFake(t)
	start(t, f, Config{})
	handler := Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("nil map write")
	}))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("the panic should go on to net/http")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/checkout", nil))
	}()
	log := f.waitFor(t, 1)[0]
	m := meta(log)
	if log["level"] != "error" || log["message"] != "panic: nil map write" || m["path"] != "/checkout" || m["method"] != "POST" {
		t.Fatalf("%v", log)
	}
	if !strings.Contains(m["stack"].(string), "goroutine") {
		t.Fatalf("stack: %v", m["stack"])
	}
}

func TestNothingStartsWithoutAKeyOrWhenDisabled(t *testing.T) {
	f := newFake(t)
	t.Setenv("ZHARP_API_KEY", "")
	Init(Config{Service: "x", Endpoint: f.URL})
	if IsActive() {
		t.Fatal("started without a key")
	}
	t.Setenv("ZHARP_DISABLED", "true")
	Init(Config{APIKey: "k", Service: "x", Endpoint: f.URL})
	if IsActive() {
		t.Fatal("started though disabled")
	}
	Error("dropped")
	time.Sleep(50 * time.Millisecond)
	if len(f.logs()) != 0 {
		t.Fatal("sent before init")
	}
}

func TestSettingsComeFromTheEnvironment(t *testing.T) {
	f := newFake(t)
	t.Setenv("ZHARP_API_KEY", "env-key")
	t.Setenv("ZHARP_SERVICE", "from-env")
	t.Setenv("ZHARP_ENV", "staging")
	t.Setenv("ZHARP_ENDPOINT", f.URL+"/api/v1")
	Init(Config{FlushInterval: 50 * time.Millisecond})
	t.Cleanup(Close)
	Info("hello")
	log := f.waitFor(t, 1)[0]
	if log["service"] != "from-env" || log["environment"] != "staging" || f.headers[0].Get("X-API-KEY") != "env-key" {
		t.Fatalf("%v", log)
	}
}

func TestServerErrorsAreRetriedAndRefusalsAreNot(t *testing.T) {
	f := newFake(t)
	tr := &transport{apiKey: "k", url: f.URL + "/agent/logs", client: http.DefaultClient, retryDelay: time.Millisecond}
	f.status = 503
	if tr.send([]map[string]any{{}}) || len(f.bodies) != 3 {
		t.Fatalf("5xx: %d attempts", len(f.bodies))
	}
	f.bodies = nil
	f.status = 429
	if !tr.send([]map[string]any{{}}) || len(f.bodies) != 1 {
		t.Fatalf("4xx: %d attempts", len(f.bodies))
	}
}

func TestARefusedKeyStopsSendingForAnHourThenTriesAgain(t *testing.T) {
	f := newFake(t)
	clock := time.Unix(0, 0)
	tr := &transport{apiKey: "k", url: f.URL + "/agent/logs", client: http.DefaultClient, retryDelay: time.Millisecond,
		now: func() time.Time { return clock }}
	f.status = 401
	if !tr.send([]map[string]any{{}}) || len(f.bodies) != 1 {
		t.Fatalf("401: %d attempts", len(f.bodies))
	}
	// Backed off: dropped without a request, and not retried.
	tr.send([]map[string]any{{}})
	tr.send([]map[string]any{{}})
	if len(f.bodies) != 1 {
		t.Fatalf("sent while backed off: %d", len(f.bodies))
	}
	// An hour on, one body tries again; still refused, the wait starts over.
	clock = clock.Add(rejectedBackoff)
	f.status = 403
	tr.send([]map[string]any{{}, {}})
	if len(f.bodies) != 2 || !tr.backedOff() {
		t.Fatalf("retry after an hour: %d", len(f.bodies))
	}
	// Accepted again: sending resumes.
	clock = clock.Add(rejectedBackoff)
	f.status = 201
	tr.send([]map[string]any{{}})
	tr.send([]map[string]any{{}})
	if len(f.bodies) != 4 || tr.backedOff() {
		t.Fatalf("after recovery: %d", len(f.bodies))
	}
}

func TestBodiesAreCutBySizeAndALargeEntryGoesAlone(t *testing.T) {
	entries := []map[string]any{{"m": strings.Repeat("x", 40)}, {"m": strings.Repeat("x", 40)}, {"m": strings.Repeat("x", 40)}, {"m": strings.Repeat("y", 500)}}
	bodies := splitBySize(entries, 120)
	total := 0
	for _, b := range bodies {
		var decoded struct{ Logs []any }
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		total += len(decoded.Logs)
	}
	if total != 4 {
		t.Fatalf("%d entries", total)
	}
}

func TestTheQueueKeepsTheNewest500(t *testing.T) {
	p := &pipeline{interval: time.Hour, maxBatch: 1000, wake: make(chan struct{}, 1)}
	for i := 0; i < 600; i++ {
		p.push(map[string]any{"i": i}, false)
	}
	if len(p.queue) != maxQueued || p.queue[0]["i"] != 100 {
		t.Fatalf("%d, first %v", len(p.queue), p.queue[0]["i"])
	}
}

func TestEndpointsLoseTrailingSlashesAndDefaultToZharp(t *testing.T) {
	if normalizeEndpoint("") != "https://api.zharp.io/api/v1" || normalizeEndpoint(" https://x.io/api/v1// ") != "https://x.io/api/v1" {
		t.Fatal("endpoint")
	}
}

// A real program: Recover sends the panic, and the program still dies of it.
func TestRecoverSendsAPanicAndTheProgramStillEnds(t *testing.T) {
	if os.Getenv("ZHARP_TEST_PANIC") == "1" {
		Init(Config{APIKey: "k", Service: "crasher", Endpoint: os.Getenv("ZHARP_TEST_ENDPOINT")})
		defer Close()
		defer Recover()
		var orders map[string]int
		orders["99"] = 1 // panics: assignment to entry in nil map
		return
	}
	f := newFake(t)
	cmd := exec.Command(os.Args[0], "-test.run=TestRecoverSendsAPanicAndTheProgramStillEnds")
	cmd.Env = append(os.Environ(), "ZHARP_TEST_PANIC=1", "ZHARP_TEST_ENDPOINT="+f.URL)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("the program should have died of the panic:\n%s", out)
	}
	if !strings.Contains(string(out), "assignment to entry in nil map") {
		t.Fatalf("Go should still print the panic:\n%s", out)
	}
	logs := f.waitFor(t, 1)
	m := meta(logs[0])
	if logs[0]["level"] != "fatal" || m["type"] != "panic" || !strings.Contains(m["stack"].(string), "TestRecoverSendsAPanic") {
		t.Fatalf("%v", logs[0])
	}
}
