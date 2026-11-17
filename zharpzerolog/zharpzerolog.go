// Package zharpzerolog sends zerolog logs to Zharp, with their level, message and fields — and,
// with TraceHook, the trace and span of the context the event was given.
//
//	shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api"})
//	defer shutdown(context.Background())
//
//	logger := zerolog.New(zharpzerolog.Writer(os.Stdout)).Hook(zharpzerolog.TraceHook{}).With().Timestamp().Logger()
//	logger.Info().Ctx(r.Context()).Int("order_id", 42).Msg("Order placed")
//
// TraceHook adds the trace_id and span_id of the event's context (event.Ctx(ctx)) as fields, so
// the printed line carries its request's ids and the Zharp agent links it to the request
// (zharp.Config.LogCorrelation). Writer passes every event on to the writer it wraps (os.Stdout, a
// ConsoleWriter, a file…) and, with zharp.Config.Logs on, sends it to Zharp first, in the trace
// those fields name: zerolog's hooks can't see an event's fields and its writers can't see its
// context. A ConsoleWriter's line ends with " trace_id=… span_id=…" instead of listing them
// among its sorted fields.
package zharpzerolog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rs/zerolog"
	zharp "github.com/zharp-io/zharp-go-lib"
	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

const scopeName = "github.com/zharp-io/zharp-go-lib/zharpzerolog"

// The fields TraceHook adds and Writer reads.
const (
	TraceIDField = "trace_id"
	SpanIDField  = "span_id"
)

// Writer returns a zerolog writer that sends each event to Zharp and then writes it to w as
// before. It may be made before zharp.Init; events go to whichever Init is active (nowhere before
// Init). A nil w sends to Zharp only.
func Writer(w io.Writer) zerolog.LevelWriter {
	return &writer{
		next:   w,
		logger: zharp.LoggerProvider().Logger(scopeName, otellog.WithInstrumentationVersion(zharp.Version)),
	}
}

type writer struct {
	next   io.Writer
	logger otellog.Logger
}

func (w *writer) Write(p []byte) (int, error) {
	return w.WriteLevel(zerolog.NoLevel, p)
}

func (w *writer) WriteLevel(level zerolog.Level, p []byte) (int, error) {
	w.send(level, p)
	switch next := w.next.(type) {
	case nil:
		return len(p), nil
	case zerolog.ConsoleWriter:
		return w.console(next, p)
	case *zerolog.ConsoleWriter:
		return w.console(*next, p)
	case zerolog.LevelWriter:
		return next.WriteLevel(level, p)
	default:
		return next.Write(p)
	}
}

// send never fails the caller: an event it can't read is only printed.
func (w *writer) send(level zerolog.Level, p []byte) {
	defer func() { _ = recover() }()
	ctx := context.Background()
	if !w.logger.Enabled(ctx, otellog.EnabledParameters{}) {
		return
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.UseNumber()
	var event map[string]any
	if dec.Decode(&event) != nil {
		return
	}
	if level == zerolog.NoLevel {
		if s, ok := event[zerolog.LevelFieldName].(string); ok {
			if l, err := zerolog.ParseLevel(s); err == nil {
				level = l
			}
		}
	}
	severity, text := severityOf(level)

	var r otellog.Record
	now := time.Now()
	r.SetTimestamp(now)
	r.SetObservedTimestamp(now)
	r.SetSeverity(severity)
	r.SetSeverityText(text)
	if msg, ok := event[zerolog.MessageFieldName].(string); ok {
		r.SetBody(attribute.StringValue(msg))
	}
	var traceID trace.TraceID
	var spanID trace.SpanID
	for k, v := range event {
		switch k {
		case zerolog.LevelFieldName, zerolog.MessageFieldName, zerolog.TimestampFieldName:
		case TraceIDField:
			if s, ok := v.(string); ok {
				traceID, _ = trace.TraceIDFromHex(s)
			}
		case SpanIDField:
			if s, ok := v.(string); ok {
				spanID, _ = trace.SpanIDFromHex(s)
			}
		case zerolog.ErrorFieldName:
			if s, ok := v.(string); ok {
				r.SetErr(errors.New(s))
			} else {
				r.AddAttributes(attribute.KeyValue{Key: attribute.Key(k), Value: valueOf(v)})
			}
		default:
			r.AddAttributes(attribute.KeyValue{Key: attribute.Key(k), Value: valueOf(v)})
		}
	}
	if traceID.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
		}))
	}
	w.logger.Emit(ctx, r)
}

// console prints a ConsoleWriter's line with the ids at its end, where Zharp reads them.
func (w *writer) console(cw zerolog.ConsoleWriter, p []byte) (int, error) {
	var ids struct {
		TraceID string `json:"trace_id"`
		SpanID  string `json:"span_id"`
	}
	if json.Unmarshal(p, &ids) != nil || ids.TraceID == "" || cw.Out == nil {
		return cw.Write(p)
	}
	traceID, err1 := trace.TraceIDFromHex(ids.TraceID)
	spanID, err2 := trace.SpanIDFromHex(ids.SpanID)
	if err1 != nil || err2 != nil {
		return cw.Write(p)
	}
	out := cw.Out
	var buf bytes.Buffer
	cw.Out = &buf
	cw.FieldsExclude = append(append([]string(nil), cw.FieldsExclude...), TraceIDField, SpanIDField)
	if _, err := cw.Write(p); err != nil {
		return 0, err
	}
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{TraceID: traceID, SpanID: spanID}))
	if _, err := out.Write(zharp.StampLine(ctx, buf.Bytes())); err != nil {
		return 0, err
	}
	return len(p), nil
}

func severityOf(level zerolog.Level) (otellog.Severity, string) {
	switch level {
	case zerolog.TraceLevel:
		return otellog.SeverityTrace, "trace"
	case zerolog.DebugLevel:
		return otellog.SeverityDebug, "debug"
	case zerolog.WarnLevel:
		return otellog.SeverityWarn, "warn"
	case zerolog.ErrorLevel:
		return otellog.SeverityError, "error"
	case zerolog.FatalLevel:
		return otellog.SeverityFatal, "fatal"
	case zerolog.PanicLevel:
		return otellog.SeverityFatal4, "panic"
	default:
		return otellog.SeverityInfo, "info"
	}
}

func valueOf(v any) attribute.Value {
	switch x := v.(type) {
	case string:
		return attribute.StringValue(x)
	case bool:
		return attribute.BoolValue(x)
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return attribute.Int64Value(i)
		}
		if f, err := x.Float64(); err == nil {
			return attribute.Float64Value(f)
		}
		return attribute.StringValue(x.String())
	case nil:
		return attribute.StringValue("")
	default: // objects and arrays, as JSON
		if b, err := json.Marshal(x); err == nil {
			return attribute.StringValue(string(b))
		}
		return attribute.StringValue(fmt.Sprint(x))
	}
}

// TraceHook adds the trace_id and span_id of the event's context (event.Ctx(ctx)) as fields, while
// a zharp.Init is active with LogCorrelation on. Writer sends the event in that trace.
type TraceHook struct{}

// Run implements zerolog.Hook.
func (TraceHook) Run(e *zerolog.Event, _ zerolog.Level, _ string) {
	if traceID, spanID, ok := zharp.LogIDs(e.GetCtx()); ok {
		e.Str(TraceIDField, traceID).Str(SpanIDField, spanID)
	}
}
