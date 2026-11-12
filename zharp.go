// Package zharp is Zharp Application Monitoring for Go: a thin layer over the official
// OpenTelemetry SDK that sends traces, logs and errors to Zharp.
//
//	func main() {
//		shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api", Environment: "production"})
//		defer shutdown(context.Background())
//
//		mux := http.NewServeMux()
//		mux.HandleFunc("GET /orders/{id}", getOrder)
//		http.ListenAndServe(":8080", zharp.Handler(mux))
//	}
//
// Init configures OpenTelemetry — OTLP/HTTP exporters, resource, W3C tracecontext + baggage
// propagation, and a log/slog bridge — and sets the global providers, so any OpenTelemetry
// instrumentation in the app is sent too. Handler traces net/http requests; the zharpgin and
// zharpecho modules do the same for Gin and Echo.
package zharp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/log/global"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Version is this module's version, sent as telemetry.sdk.version.
const Version = "0.3.0"

// DefaultEndpoint is where telemetry goes unless Config.Endpoint or ZHARP_ENDPOINT says otherwise.
const DefaultEndpoint = "https://api.zharp.io/api/v1/otlp"

const scopeName = "github.com/zharp-io/zharp-go-lib"

// Config holds the settings for Init. Anything left empty is read from the environment —
// ZHARP_KEY, ZHARP_SERVICE (then OTEL_SERVICE_NAME), ZHARP_ENVIRONMENT, ZHARP_RELEASE,
// ZHARP_ENDPOINT, ZHARP_SAMPLE_RATE, ZHARP_TRACING, ZHARP_CAPTURE_BODIES — then defaulted.
type Config struct {
	// Key is the app's key (zpk_…) from Applications → your app → Setup. Without one, nothing
	// is sent.
	Key string
	// Service is the service.name everything is filed under. Defaults to the last part of the
	// main module's path (github.com/acme/checkout-api → checkout-api).
	Service string
	// Environment, e.g. "production".
	Environment string
	// Release, e.g. "api@2.3.0" — the service.version.
	Release string
	// Endpoint is the OTLP base URL. Defaults to https://api.zharp.io/api/v1/otlp.
	Endpoint string
	// SampleRate is the share of traces kept, 0 < rate ≤ 1. Zero means 1 (keep all). Logs and
	// errors are always sent.
	SampleRate float64
	// DisableTracing sends logs and errors only ("logs only"): no spans are created or exported,
	// Handler, Transport and the zharpgin/zharpecho middleware trace nothing, and no traceparent or
	// baggage is sent with outgoing requests. An incoming traceparent and baggage are still read,
	// so the logs and errors of a request carry its trace_id, user and session. Logs, errors,
	// SetUser and SetTag work as usual. ZHARP_TRACING=false does the same.
	DisableTracing bool
	// CaptureBodies puts request and response bodies on HTTP spans (Handler, Transport, zharpgin,
	// zharpecho). Nil, the default, is off; ZHARP_CAPTURE_BODIES=errors|all turns it on without
	// code. See CaptureBodies.
	CaptureBodies *CaptureBodies
	// KeepSlogDefault leaves slog's default logger alone. By default Init makes slog.Default
	// (and so the log package) write to the console as before and also send to Zharp.
	KeepSlogDefault bool

	// SpanExporter and LogExporter replace the OTLP/HTTP exporters to Zharp, and export each span
	// and log synchronously as it ends — for tests (tracetest.NewInMemoryExporter).
	SpanExporter sdktrace.SpanExporter
	LogExporter  sdklog.Exporter
}

type state struct {
	tracing        bool
	bodies         *bodySettings            // nil when CaptureBodies is off
	tracerProvider *sdktrace.TracerProvider // nil with DisableTracing
	loggerProvider *sdklog.LoggerProvider
	logger         otellog.Logger
	slog           *slog.Logger
}

var (
	mu      sync.RWMutex
	current *state
)

func active() *state {
	mu.RLock()
	defer mu.RUnlock()
	return current
}

// Init starts sending to Zharp and returns the function that sends what's left and stops; call
// it on the way out:
//
//	shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api"})
//	defer shutdown(context.Background())
//
// It never panics and never fails the app: without a key, or on a setup error, it writes one line
// to stderr and returns a shutdown that does nothing. ZHARP_DISABLED=true turns it off.
func Init(cfg Config) func(context.Context) error {
	noop := func(context.Context) error { return nil }
	if isTrue(os.Getenv("ZHARP_DISABLED")) {
		return noop
	}
	cfg = resolve(cfg)
	if cfg.Key == "" {
		announce("No key — set ZHARP_KEY or Config.Key. Nothing is sent.")
		return noop
	}

	s, err := start(cfg)
	if err != nil {
		announce("Could not start: " + err.Error())
		return noop
	}
	mu.Lock()
	previous := current
	current = s
	mu.Unlock()
	if previous != nil {
		_ = previous.shutdown(context.Background())
	}

	env := "no environment set"
	if cfg.Environment != "" {
		env = fmt.Sprintf("environment %q", cfg.Environment)
	}
	mode := ""
	if cfg.DisableTracing {
		mode = ", logs only (tracing off)"
	}
	announce(fmt.Sprintf("Application Monitoring active — service %q, %s%s", cfg.Service, env, mode))

	return func(ctx context.Context) error {
		mu.Lock()
		if current == s {
			current = nil
		}
		mu.Unlock()
		return s.shutdown(ctx)
	}
}

func start(cfg Config) (*state, error) {
	ctx := context.Background()
	res, err := newResource(cfg)
	if err != nil {
		return nil, err
	}

	headers := map[string]string{"Authorization": "Bearer " + cfg.Key}
	var tp *sdktrace.TracerProvider
	if !cfg.DisableTracing {
		spanProcessor := sdktrace.SpanProcessor(nil)
		if cfg.SpanExporter != nil {
			spanProcessor = sdktrace.NewSimpleSpanProcessor(urlRedactingExporter{cfg.SpanExporter})
		} else {
			exporter, err := otlptracehttp.New(ctx,
				otlptracehttp.WithEndpointURL(cfg.Endpoint+"/v1/traces"),
				otlptracehttp.WithHeaders(headers),
				otlptracehttp.WithCompression(otlptracehttp.GzipCompression),
			)
			if err != nil {
				return nil, err
			}
			spanProcessor = sdktrace.NewBatchSpanProcessor(urlRedactingExporter{exporter}, sdktrace.WithBatchTimeout(5*time.Second))
		}
		tp = sdktrace.NewTracerProvider(
			sdktrace.WithResource(res),
			sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))),
			sdktrace.WithSpanProcessor(contextSpanProcessor{}),
			sdktrace.WithSpanProcessor(spanProcessor),
		)
	}

	logProcessor := sdklog.Processor(nil)
	if cfg.LogExporter != nil {
		logProcessor = sdklog.NewSimpleProcessor(cfg.LogExporter)
	} else {
		exporter, err := otlploghttp.New(ctx,
			otlploghttp.WithEndpointURL(cfg.Endpoint+"/v1/logs"),
			otlploghttp.WithHeaders(headers),
			otlploghttp.WithCompression(otlploghttp.GzipCompression),
		)
		if err != nil {
			return nil, err
		}
		logProcessor = sdklog.NewBatchProcessor(exporter, sdklog.WithExportInterval(5*time.Second))
	}
	lp := sdklog.NewLoggerProvider(
		sdklog.WithResource(res),
		sdklog.WithProcessor(contextLogProcessor{}),
		sdklog.WithProcessor(logProcessor),
	)

	propagator := propagation.TextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	if tp != nil {
		otel.SetTracerProvider(tp)
	} else {
		otel.SetTracerProvider(passthroughTracerProvider{})
		propagator = extractOnly{propagator}
	}
	global.SetLoggerProvider(lp)
	otel.SetTextMapPropagator(propagator)
	otel.SetErrorHandler(onceErrorHandler())

	s := &state{
		tracing:        tp != nil,
		bodies:         bodySettingsFrom(cfg.CaptureBodies),
		tracerProvider: tp,
		loggerProvider: lp,
		logger:         lp.Logger(scopeName, otellog.WithInstrumentationVersion(Version)),
	}
	s.slog = slog.New(newOtelSlogHandler(lp))
	if !cfg.KeepSlogDefault {
		installSlogDefault(s.slog.Handler())
	}
	return s, nil
}

func (s *state) shutdown(ctx context.Context) error {
	var err error
	if s.tracerProvider != nil {
		err = s.tracerProvider.Shutdown(ctx)
	}
	return errors.Join(err, s.loggerProvider.Shutdown(ctx))
}

// tracingEnabled is false before Init and with DisableTracing.
func tracingEnabled() bool {
	s := active()
	return s != nil && s.tracing
}

// Flush sends everything queued now — before a serverless function returns, or at the end of a
// job. It waits until ctx is done at most.
func Flush(ctx context.Context) error {
	s := active()
	if s == nil {
		return nil
	}
	var err error
	if s.tracerProvider != nil {
		err = s.tracerProvider.ForceFlush(ctx)
	}
	return errors.Join(err, s.loggerProvider.ForceFlush(ctx))
}

// Tracer returns a tracer for spans of your own: zharp.Tracer().Start(ctx, "render invoice").
// With DisableTracing its spans record nothing (the code inside still runs, and its logs carry the
// incoming trace_id, if any).
func Tracer() trace.Tracer {
	return otel.Tracer(scopeName, trace.WithInstrumentationVersion(Version))
}

func newResource(cfg Config) (*resource.Resource, error) {
	attrs := []attribute.KeyValue{
		attribute.String("service.name", cfg.Service),
		attribute.String("telemetry.sdk.name", "zharp"),
		attribute.String("telemetry.sdk.language", "go"),
		attribute.String("telemetry.sdk.version", Version),
		attribute.Int("process.pid", os.Getpid()),
	}
	if cfg.Release != "" {
		attrs = append(attrs, attribute.String("service.version", cfg.Release))
	}
	if cfg.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", cfg.Environment))
	}
	if host, err := os.Hostname(); err == nil {
		attrs = append(attrs, attribute.String("host.name", host))
	}
	// OTEL_RESOURCE_ATTRIBUTES first, ours over it.
	fromEnv, _ := resource.New(context.Background(), resource.WithFromEnv())
	return resource.Merge(fromEnv, resource.NewSchemaless(attrs...))
}

func resolve(cfg Config) Config {
	cfg.Key = first(cfg.Key, os.Getenv("ZHARP_KEY"), os.Getenv("ZHARP_API_KEY"))
	cfg.Service = first(cfg.Service, os.Getenv("ZHARP_SERVICE"), os.Getenv("OTEL_SERVICE_NAME"), mainModuleName(), "unknown-service")
	cfg.Environment = first(cfg.Environment, os.Getenv("ZHARP_ENVIRONMENT"), os.Getenv("ZHARP_ENV"))
	cfg.Release = first(cfg.Release, os.Getenv("ZHARP_RELEASE"), os.Getenv("ZHARP_VERSION"))
	cfg.Endpoint = strings.TrimRight(first(cfg.Endpoint, os.Getenv("ZHARP_ENDPOINT"), DefaultEndpoint), "/")
	if cfg.SampleRate == 0 {
		if rate, err := strconv.ParseFloat(os.Getenv("ZHARP_SAMPLE_RATE"), 64); err == nil {
			cfg.SampleRate = rate
		}
	}
	if !cfg.DisableTracing && isFalse(os.Getenv("ZHARP_TRACING")) {
		cfg.DisableTracing = true
	}
	if cfg.SampleRate <= 0 || cfg.SampleRate > 1 {
		cfg.SampleRate = 1
	}
	return cfg
}

func first(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}

func isTrue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

func isFalse(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "no", "off":
		return true
	}
	return false
}

func mainModuleName() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Path == "" {
		return ""
	}
	parts := strings.Split(info.Main.Path, "/")
	return parts[len(parts)-1]
}

func announce(message string) {
	fmt.Fprintln(os.Stderr, "[Zharp] "+message)
}

// onceErrorHandler prints each distinct OpenTelemetry error once (a wrong key would otherwise
// print a line for every batch).
func onceErrorHandler() otel.ErrorHandler {
	var seen sync.Map
	return otel.ErrorHandlerFunc(func(err error) {
		msg := err.Error()
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if _, loaded := seen.LoadOrStore(msg, true); !loaded {
			announce(err.Error())
		}
	})
}
