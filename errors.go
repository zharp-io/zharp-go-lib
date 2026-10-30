package zharp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	otellog "go.opentelemetry.io/otel/log"
	"go.opentelemetry.io/otel/trace"
)

// Errors go to Zharp as log records with exception.* attributes — the shape Zharp groups into
// issues — carrying the trace and span of ctx. Logs aren't sampled, so an error is never lost to
// trace sampling. The span in ctx is marked as failed.

// CaptureError reports an error you handled. It becomes a Zharp issue (marked handled) with the
// request's trace, user and session. Extra attributes are slog-style key-value pairs:
//
//	if err := charge(ctx, order); err != nil {
//		zharp.CaptureError(ctx, err, "order_id", order.ID)
//	}
func CaptureError(ctx context.Context, err error, args ...any) {
	if err == nil {
		return
	}
	report(ctx, typeName(err), err.Error(), stackTrace(2, false), true, otellog.SeverityError, args)
}

// Recover reports a panic (unhandled, fatal), waits up to two seconds for it to be sent, then
// panics again so the program ends as it would have. Defer it first thing in main and in
// goroutines whose panics you want to see — Go has no global panic hook:
//
//	defer zharp.Recover()
func Recover() {
	value := recover()
	if value == nil {
		return
	}
	reportPanic(context.Background(), value, otellog.SeverityFatal)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	_ = Flush(ctx)
	cancel()
	panic(value)
}

// RecoverRequest is the panic handling of Handler, for middleware of other routers: deferred
// inside the request's span, it reports a panic (unhandled), marks the span as failed and answers
// 500. http.ErrAbortHandler is re-panicked, as net/http expects.
//
//	defer zharp.RecoverRequest(w, r)
func RecoverRequest(w http.ResponseWriter, r *http.Request) {
	value := recover()
	if value == nil {
		return
	}
	if err, ok := value.(error); ok && errors.Is(err, http.ErrAbortHandler) {
		panic(value)
	}
	ReportPanic(r.Context(), value)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

// ReportUnhandledError reports an error that ended a request with a 5xx — what a router does
// with an error a handler returned. For router integrations (zharpgin, zharpecho); in your own
// code use CaptureError. A returned error carries no stack, so it is grouped by type and message.
func ReportUnhandledError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	report(ctx, typeName(err), err.Error(), "", false, otellog.SeverityError, nil)
}

// ReportPanic reports a recovered panic value as an unhandled error, with the stack of the
// panicking goroutine. Call it in a deferred function, right after recover().
func ReportPanic(ctx context.Context, value any) {
	reportPanic(ctx, value, otellog.SeverityError)
}

func reportPanic(ctx context.Context, value any, severity otellog.Severity) {
	typ, msg := "panic", fmt.Sprint(value)
	if err, ok := value.(error); ok {
		typ, msg = typeName(err), err.Error()
	}
	report(ctx, typ, msg, stackTrace(3, true), false, severity, nil)
}

func report(ctx context.Context, typ, msg, stack string, handled bool, severity otellog.Severity, args []any) {
	defer func() { _ = recover() }() // reporting must never panic

	if span := trace.SpanFromContext(ctx); span.IsRecording() && !handled {
		span.SetStatus(codes.Error, msg)
	}
	s := active()
	if s == nil {
		return
	}
	var record otellog.Record
	now := time.Now()
	record.SetTimestamp(now)
	record.SetObservedTimestamp(now)
	record.SetSeverity(severity)
	record.SetSeverityText(severityText(severity))
	record.SetBody(attribute.StringValue(typ + ": " + msg))
	record.AddAttributes(keyValues(args)...)
	handledText := "false"
	if handled {
		handledText = "true"
	}
	record.AddAttributes(
		attribute.String("exception.type", typ),
		attribute.String("exception.message", msg),
		attribute.String("zharp.handled", handledText),
	)
	if stack != "" {
		record.AddAttributes(attribute.String("exception.stacktrace", typ+": "+msg+"\n\n"+stack))
	}
	s.logger.Emit(ctx, record)
}

func severityText(s otellog.Severity) string {
	switch {
	case s >= otellog.SeverityFatal:
		return "FATAL"
	case s >= otellog.SeverityError:
		return "ERROR"
	case s >= otellog.SeverityWarn:
		return "WARN"
	case s >= otellog.SeverityInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// typeName is the error's dynamic type (*fs.PathError), unwrapping fmt.Errorf's wrappers, which
// say nothing about what went wrong.
func typeName(err error) string {
	for {
		name := fmt.Sprintf("%T", err)
		if name != "*fmt.wrapError" && name != "*fmt.wrapErrors" {
			return name
		}
		next := errors.Unwrap(err)
		if next == nil {
			return name
		}
		err = next
	}
}

// stackTrace is the calling goroutine's stack in Go's own format ("goroutine N [running]:", then
// a function line and a "\tfile:line" line per frame), without this package's frames — and, for a
// panic, without the frames between the panic and the recover.
func stackTrace(skip int, afterPanic bool) string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(skip+1, pcs)
	frames := runtime.CallersFrames(pcs[:n])

	var b strings.Builder
	b.WriteString("goroutine 1 [running]:\n")
	skipping := afterPanic
	for {
		frame, more := frames.Next()
		fn := frame.Function
		switch {
		case skipping:
			if fn == "runtime.gopanic" || strings.HasPrefix(fn, "runtime.panic") {
				skipping = false
			}
		case (strings.HasPrefix(fn, scopeName+".") || strings.HasPrefix(fn, scopeName+"/")) && !strings.HasSuffix(frame.File, "_test.go"),
			strings.HasPrefix(fn, "runtime."):
			// Zharp's own frames and the runtime's say where the error was reported, not why.
		default:
			fmt.Fprintf(&b, "%s(...)\n\t%s:%d\n", fn, frame.File, frame.Line)
		}
		if !more {
			break
		}
	}
	if skipping { // not after a panic after all: keep everything
		return stackTrace(skip+1, false)
	}
	return b.String()
}
