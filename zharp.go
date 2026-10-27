// Package zharp sends your Go app's logs and panics to Zharp.
//
// Start it once in main, and close it on the way out so the last logs are sent:
//
//	func main() {
//		zharp.Init(zharp.Config{APIKey: "your-ingest-only-key", Service: "checkout-api"})
//		defer zharp.Close()
//
//		zharp.Info("User signed in", zharp.Fields{"user_id": 42})
//	}
//
// Logs are queued and sent in batches from a background goroutine; Error and Fatal go at once.
// Logging never blocks on the network and never panics. To send log/slog records, use
// NewSlogHandler; to capture panics, Recover and Middleware.
package zharp

import (
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Version is this package's version.
const Version = "0.1.1"

// sdkName is what requests from this SDK say they are, in the X-Zharp-SDK header.
const sdkName = "go-logs/" + Version

// Fields are extra data kept with a log. Values are copied when the log is made, so changing the
// map afterwards doesn't change a log waiting to be sent. An error among them brings its stack.
type Fields map[string]any

// Level is how bad a log is.
type Level string

const (
	LevelDebug Level = "debug"
	LevelInfo  Level = "info"
	LevelWarn  Level = "warn"
	LevelError Level = "error"
	LevelFatal Level = "fatal"
)

// Config holds the settings for Init. Every field is optional: anything left empty is read from
// the environment (ZHARP_API_KEY, ZHARP_SERVICE then OTEL_SERVICE_NAME, ZHARP_ENV, ZHARP_VERSION,
// ZHARP_SOURCE, ZHARP_ENDPOINT), then defaulted.
type Config struct {
	// APIKey is an Ingest only key from Integrations & API → Main API keys. Without one, nothing
	// starts.
	APIKey string
	// Service is the name logs are filed under. Defaults to the last part of the main module's
	// path (github.com/acme/checkout-api → checkout-api), else "unknown-service".
	Service string
	// Environment tags every log, e.g. "production". Not guessed when unset.
	Environment string
	// AppVersion tags every log, so you can filter to one release.
	AppVersion string
	// Source tags every log with what wrote it.
	Source string
	// Endpoint is where logs are sent. Defaults to https://api.zharp.io/api/v1.
	Endpoint string
	// FlushInterval is how long logs wait to be sent together. 5 seconds when zero.
	FlushInterval time.Duration
	// MaxBatchSize is the most logs sent in one request. 50 when zero.
	MaxBatchSize int
	// HTTPClient sends the requests. One with a 15-second timeout when nil.
	HTTPClient *http.Client
}

type setup struct {
	service     string
	environment string
	appVersion  string
	source      string
	pipeline    *pipeline
}

var (
	mu           sync.RWMutex
	current      *setup
	sessionID    = newSessionID()
	extraContext = map[string]any{}
	runtimeCtx   map[string]any
	runtimeOnce  sync.Once
)

// Init starts sending logs. Call it once, as early as possible in main, and defer Close.
//
// It prints one line to stderr:
//
//	[Zharp] Logging active — service "checkout-api", environment "production"
//
// With no API key anywhere it prints how to give it one and starts nothing; ZHARP_DISABLED=true
// turns it off whatever the code says. It never returns an error or panics. Calling it again
// replaces the running setup; what the old one had queued is still sent.
func Init(cfg Config) {
	mu.Lock()
	previous := current
	current = nil
	mu.Unlock()
	if previous != nil {
		go previous.pipeline.close(5 * time.Second)
	}

	if isTrue(envValue("ZHARP_DISABLED")) {
		announce("Logging disabled by ZHARP_DISABLED — nothing will be sent")
		return
	}
	key := pick(cfg.APIKey, envValue("ZHARP_API_KEY"))
	if key == "" {
		announce("No API key found — set Config.APIKey in zharp.Init(), or set ZHARP_API_KEY")
		return
	}
	service := pick(cfg.Service, envValue("ZHARP_SERVICE"), envValue("OTEL_SERVICE_NAME"), moduleName())
	if service == "" {
		// A warning, not an error: a missing name shouldn't stop the app, and the logs are still
		// worth having under a placeholder.
		announce("No service name — set Config.Service or ZHARP_SERVICE. Using 'unknown-service'.")
		service = "unknown-service"
	}

	interval := cfg.FlushInterval
	if interval <= 0 {
		interval = 5 * time.Second
	}
	batch := cfg.MaxBatchSize
	if batch <= 0 {
		batch = 50
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	tr := &transport{
		apiKey:     key,
		url:        normalizeEndpoint(pick(cfg.Endpoint, envValue("ZHARP_ENDPOINT"))) + "/agent/logs",
		client:     client,
		retryDelay: time.Second,
	}
	next := &setup{
		service:     service,
		environment: pick(cfg.Environment, envValue("ZHARP_ENV")),
		appVersion:  pick(cfg.AppVersion, envValue("ZHARP_VERSION")),
		source:      pick(cfg.Source, envValue("ZHARP_SOURCE")),
		pipeline:    newPipeline(tr, interval, batch),
	}
	mu.Lock()
	current = next
	mu.Unlock()

	env := "no environment set"
	if next.environment != "" {
		env = fmt.Sprintf("environment %q", next.environment)
	}
	announce(fmt.Sprintf("Logging active — service %q, %s", next.service, env))
}

// IsActive reports whether Init succeeded and logs are being sent.
func IsActive() bool {
	mu.RLock()
	defer mu.RUnlock()
	return current != nil
}

// Debug logs at debug. fields is optional.
func Debug(message any, fields ...Fields) { logAt(LevelDebug, message, merge(fields)) }

// Info logs at info. fields is optional.
func Info(message any, fields ...Fields) { logAt(LevelInfo, message, merge(fields)) }

// Warn logs at warn. fields is optional.
func Warn(message any, fields ...Fields) { logAt(LevelWarn, message, merge(fields)) }

// Error logs at error and sends at once. An error as the message, or among the fields, brings
// its stack.
func Error(message any, fields ...Fields) { logAt(LevelError, message, merge(fields)) }

// Fatal logs at fatal and sends at once. Unlike log.Fatal it doesn't end the program.
func Fatal(message any, fields ...Fields) { logAt(LevelFatal, message, merge(fields)) }

// CaptureError logs err at error with its type and stack. A nil err does nothing.
//
//	if err := charge(order); err != nil {
//		zharp.CaptureError(err, zharp.Fields{"order_id": order.ID})
//	}
func CaptureError(err error, fields ...Fields) {
	if err == nil {
		return
	}
	logAt(LevelError, err, merge(fields))
}

// Log logs at any level.
func Log(level Level, message any, fields Fields) { logAt(level, message, fields) }

// LogService logs at any level under another service than the one set in Init.
func LogService(service string, level Level, message any, fields Fields) {
	s := active()
	if s == nil {
		return
	}
	defer func() { _ = recover() }() // logging never takes the app down
	s.pipeline.push(buildEntry(s, level, message, fields, service, time.Now()), urgent(level))
}

func logAt(level Level, message any, fields Fields) {
	s := active()
	if s == nil {
		return
	}
	defer func() { _ = recover() }() // logging never takes the app down
	s.pipeline.push(buildEntry(s, level, message, fields, "", time.Now()), urgent(level))
}

func urgent(level Level) bool { return level == LevelError || level == LevelFatal }

// SessionID is the ID every log of this run carries, so one run's logs can be found together.
func SessionID() string {
	mu.RLock()
	defer mu.RUnlock()
	return sessionID
}

// ResetSessionID starts a new session ID, e.g. per job in a long-running worker.
func ResetSessionID() {
	mu.Lock()
	sessionID = newSessionID()
	mu.Unlock()
}

// SetContext sets extra fields added to every log's context, e.g. a deployment's region.
// Replaces what was set before; pass nil to clear it.
func SetContext(fields Fields) {
	copied := map[string]any{}
	for k, v := range fields {
		copied[k] = toJSONValue(v)
	}
	mu.Lock()
	extraContext = copied
	mu.Unlock()
}

// Flush sends everything queued and waits up to timeout for it. It reports whether all of it
// went. Normally not needed: logs go out every few seconds and errors at once — but call it before
// a serverless function returns.
func Flush(timeout time.Duration) bool {
	s := active()
	if s == nil {
		return true
	}
	return s.pipeline.flush(timeout)
}

// Close sends everything queued (waiting at most 5 seconds) and stops. Defer it in main: Go
// runs nothing at exit by itself, so logs still queued when main returns would be lost.
func Close() {
	mu.Lock()
	s := current
	current = nil
	mu.Unlock()
	if s != nil {
		s.pipeline.close(5 * time.Second)
	}
}

func active() *setup {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

func merge(fields []Fields) Fields {
	switch len(fields) {
	case 0:
		return nil
	case 1:
		return fields[0]
	}
	all := Fields{}
	for _, f := range fields {
		for k, v := range f {
			all[k] = v
		}
	}
	return all
}

// buildEntry makes the entry as Zharp takes it — the same shape every Zharp SDK sends.
func buildEntry(s *setup, level Level, message any, fields Fields, service string, at time.Time) map[string]any {
	svc := strings.TrimSpace(service)
	if svc == "" {
		svc = s.service
	}
	entry := map[string]any{
		"service":   svc,
		"level":     string(normalizeLevel(level)),
		"message":   toMessage(message),
		"timestamp": iso(at),
	}
	// The tags the log viewer filters by, top level where the backend reads them; absent rather
	// than empty when not configured.
	if s.environment != "" {
		entry["environment"] = s.environment
	}
	if s.appVersion != "" {
		entry["version"] = s.appVersion
	}
	if s.source != "" {
		entry["source"] = s.source
	}

	meta := map[string]any{}
	err, _ := message.(error)
	if err == nil {
		err = firstError(fields)
	}
	if err != nil {
		// Under the keys the log viewer shows; a stack the caller gave wins.
		meta["name"] = typeName(err)
		meta["stack"] = stackOf(err)
	}
	for k, v := range fields {
		meta[k] = toJSONValue(v)
	}

	mu.RLock()
	meta["sessionId"] = sessionID
	context := map[string]any{}
	for k, v := range runtimeContext() {
		context[k] = v
	}
	for k, v := range extraContext {
		context[k] = v
	}
	mu.RUnlock()
	if s.environment != "" {
		meta["environment"] = s.environment
	}
	if s.appVersion != "" {
		context["appVersion"] = s.appVersion
	}
	meta["context"] = context
	entry["metadata"] = meta
	return entry
}

func normalizeLevel(level Level) Level {
	switch strings.ToLower(strings.TrimSpace(string(level))) {
	case "debug", "trace":
		return LevelDebug
	case "warn", "warning":
		return LevelWarn
	case "error", "err":
		return LevelError
	case "fatal", "critical", "panic":
		return LevelFatal
	default:
		return LevelInfo
	}
}

// runtimeContext is what is known about where the program runs; it doesn't change while it runs.
func runtimeContext() map[string]any {
	runtimeOnce.Do(func() {
		ctx := map[string]any{
			"pid":       fmt.Sprint(os.Getpid()),
			"goVersion": runtime.Version(),
			"platform":  runtime.GOOS,
			"arch":      runtime.GOARCH,
		}
		if host := envValue("HOSTNAME"); host != "" {
			ctx["hostname"] = host
		} else if host, err := os.Hostname(); err == nil {
			ctx["hostname"] = host
		}
		runtimeCtx = ctx
	})
	return runtimeCtx
}

func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func iso(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}
