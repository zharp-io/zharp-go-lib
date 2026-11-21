package zharp

import (
	"context"
	"errors"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Failed database queries, reported once, as errors that say where they came from.
//
// A driver's error carries no stack, and by the time a handler returns it the query is long
// gone. zharpsql takes the call site as the query runs (in the app's own goroutine, so the stack
// is the app's) and hands the failure here. It is then reported once:
//   - when the error comes back up and is reported — a router answering 5xx (ReportUnhandledError)
//     or the app calling CaptureError — that report carries the query and the call-site stack;
//   - otherwise when the request ends: as unhandled if it was answered with a 5xx, else as
//     handled (the app dealt with it).

// QueryFailure is a failed query as an integration (zharpsql) hands it over.
type QueryFailure struct {
	// Err is the driver's error, as the app received it.
	Err error
	// System is "postgresql", "mysql", "sqlite"…
	System    string
	Statement string
	Database  string
	// Code is the driver's code (SQLSTATE "23505"), when it has one.
	Code       string
	Constraint string
	// Stack is the call site, in Go's stack format (see CallSite).
	Stack string
	// Span is the query's span; the error is sent in it.
	Span trace.SpanContext
}

type pendingQuery struct {
	QueryFailure
	done bool
}

var queries = struct {
	sync.Mutex
	byTrace map[trace.TraceID][]*pendingQuery
}{byTrace: map[trace.TraceID][]*pendingQuery{}}

const (
	maxPendingTraces = 500
	loneQueryWait    = 1500 * time.Millisecond
)

// ReportFailedQuery hands a failed query to Zharp, for database integrations. It is reported
// once — see the package notes above — never more.
func ReportFailedQuery(ctx context.Context, f QueryFailure) {
	defer func() { _ = recover() }()
	if f.Err == nil || active() == nil {
		return
	}
	traceID := trace.SpanContextFromContext(ctx).TraceID()
	if !f.Span.IsValid() {
		f.Span = trace.SpanContextFromContext(ctx)
	}
	queries.Lock()
	for _, p := range queries.byTrace[traceID] {
		if p.Err == f.Err { // the same error seen twice (a Stmt and its Conn)
			queries.Unlock()
			return
		}
	}
	queries.byTrace[traceID] = append(queries.byTrace[traceID], &pendingQuery{QueryFailure: f})
	if len(queries.byTrace) > maxPendingTraces {
		for id := range queries.byTrace {
			if id != traceID {
				delete(queries.byTrace, id)
				break
			}
		}
	}
	queries.Unlock()
	// Outside a request nothing ends to flush it: a moment for the app to report it, then send.
	if !traceID.IsValid() || !trace.SpanFromContext(ctx).IsRecording() {
		time.AfterFunc(loneQueryWait, func() { flushQueries(traceID, false) })
	}
}

// claimQuery: err is being reported — the failed query it came from (err is it, or wraps it),
// so its details go into that report and it isn't reported again.
func claimQuery(ctx context.Context, err error) *QueryFailure {
	if err == nil {
		return nil
	}
	traceID := trace.SpanContextFromContext(ctx).TraceID()
	queries.Lock()
	defer queries.Unlock()
	for _, p := range queries.byTrace[traceID] {
		if !p.done && (p.Err == err || errors.Is(err, p.Err)) {
			p.done = true
			f := p.QueryFailure
			return &f
		}
	}
	return nil
}

// flushQueries reports what was never claimed: the latest as unhandled when the request failed
// (it is most likely why), the rest as handled.
func flushQueries(traceID trace.TraceID, failed bool) {
	queries.Lock()
	list := queries.byTrace[traceID]
	delete(queries.byTrace, traceID)
	queries.Unlock()
	last := -1
	for i, p := range list {
		if !p.done {
			last = i
		}
	}
	for i, p := range list {
		if p.done {
			continue
		}
		sendQueryFailure(&p.QueryFailure, p.Err.Error(), !(failed && i == last))
	}
}

// sendQueryFailure reports f, with message msg (the app's wrapping of the error when it has one).
func sendQueryFailure(f *QueryFailure, msg string, handled bool) {
	severity := otellog.SeverityError
	ctx := trace.ContextWithSpanContext(context.Background(), f.Span)
	args := []any{"zharp.mechanism", "db", "db.system", f.System}
	for _, kv := range [][2]string{{"db.statement", truncate(f.Statement, 4000)}, {"db.name", f.Database}, {"db.error.code", f.Code}, {"db.error.constraint", f.Constraint}} {
		if kv[1] != "" {
			args = append(args, kv[0], kv[1])
		}
	}
	report(ctx, queryErrorType(f), msg, f.Stack, handled, severity, args)
}

// queryErrorType names a failed query's error from its code (23505 → UniqueViolation), else by
// its Go type.
func queryErrorType(f *QueryFailure) string {
	// Postgres's codes (and those of databases speaking its protocol: CockroachDB, Yugabyte…).
	if name, ok := sqlStateNames[f.Code]; ok && f.System != "mysql" {
		return name
	}
	return typeName(f.Err)
}

var sqlStateNames = map[string]string{
	"23505": "UniqueViolation", "23503": "ForeignKeyViolation", "23502": "NotNullViolation", "23514": "CheckViolation",
	"23P01": "ExclusionViolation", "22001": "StringDataRightTruncation", "22003": "NumericValueOutOfRange",
	"22P02": "InvalidTextRepresentation", "22007": "InvalidDatetimeFormat", "22008": "DatetimeFieldOverflow", "22012": "DivisionByZero",
	"42P01": "UndefinedTable", "42703": "UndefinedColumn", "42883": "UndefinedFunction", "42601": "SyntaxError", "42501": "InsufficientPrivilege",
	"40001": "SerializationFailure", "40P01": "DeadlockDetected", "55P03": "LockNotAvailable", "57014": "QueryCanceled",
	"53300": "TooManyConnections", "08006": "ConnectionFailure", "08001": "ConnectionRefused", "28P01": "InvalidPassword", "3D000": "InvalidCatalogName",
}

// CallSite is the calling goroutine's stack in Go's format, for integrations that take a call site
// (zharpsql): Zharp's frames, the runtime's and database/sql's left out, starting at the first
// frame outside skipPrefixes (an ORM between the app and the driver).
func CallSite(skip int, skipPrefixes ...string) string {
	return callSite(skip+1, append([]string{"database/sql."}, skipPrefixes...))
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// queryFlushProcessor reports a request's unclaimed failed queries when its span ends.
type queryFlushProcessor struct{}

func (queryFlushProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}

func (queryFlushProcessor) OnEnd(s sdktrace.ReadOnlySpan) {
	parent := s.Parent()
	if s.SpanKind() != trace.SpanKindServer && parent.IsValid() && !parent.IsRemote() {
		return
	}
	traceID := s.SpanContext().TraceID()
	queries.Lock()
	_, waiting := queries.byTrace[traceID]
	queries.Unlock()
	if !waiting {
		return
	}
	failed := false
	for _, kv := range s.Attributes() {
		if kv.Key == attribute.Key("http.response.status_code") || kv.Key == attribute.Key("http.status_code") {
			failed = kv.Value.AsInt64() >= 500
		}
	}
	if s.SpanKind() != trace.SpanKindServer && s.Status().Code.String() == "Error" {
		failed = true
	}
	flushQueries(traceID, failed)
}

func (queryFlushProcessor) Shutdown(context.Context) error { return nil }

func (queryFlushProcessor) ForceFlush(context.Context) error {
	queries.Lock()
	ids := make([]trace.TraceID, 0, len(queries.byTrace))
	for id := range queries.byTrace {
		ids = append(ids, id)
	}
	queries.Unlock()
	for _, id := range ids {
		flushQueries(id, false)
	}
	return nil
}
