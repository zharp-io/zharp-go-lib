# zharp-go-lib

[Zharp](https://zharp.io) Application Monitoring for Go: every request traced, logs sent with their
trace, panics and errors grouped into issues. A thin layer over the official
[OpenTelemetry Go SDK](https://opentelemetry.io/docs/languages/go/) — `zharp.Init` configures the
OTLP/HTTP exporters, resource, W3C tracecontext + baggage propagation and a `log/slog` bridge, and
sets the global providers, so any OpenTelemetry instrumentation in your app is sent too.

Full documentation: [zharp.io/docs/sdks/go](https://zharp.io/docs/sdks/go)

## Install

```bash
go get github.com/zharp-io/zharp-go-lib
```

```go
import zharp "github.com/zharp-io/zharp-go-lib"
```

## Start it

```go
shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api", Environment: "production"})
defer shutdown(context.Background())
```

Copy the key from **Applications → your app → Setup** in Zharp. Leave fields empty to read them
from `ZHARP_KEY`, `ZHARP_SERVICE` (then `OTEL_SERVICE_NAME`), `ZHARP_ENVIRONMENT`,
`ZHARP_RELEASE`, `ZHARP_ENDPOINT`, `ZHARP_SAMPLE_RATE`, `ZHARP_TRACING`, `ZHARP_CAPTURE_BODIES`,
`ZHARP_CAPTURE_OUTPUT`. `ZHARP_DISABLED=true` turns it off. Init
never panics: without a key it prints one line and sends nothing.

## Trace requests

**net/http**

```go
http.ListenAndServe(":8080", zharp.Handler(mux))
```

**Gin** — `go get github.com/zharp-io/zharp-go-lib/zharpgin`

```go
router.Use(zharpgin.Middleware())
```

**Echo (v4)** — `go get github.com/zharp-io/zharp-go-lib/zharpecho`

```go
e.Use(zharpecho.Middleware())
```

Each request becomes one SERVER span named by its route (`GET /orders/{id}`), continuing the
caller's trace from `traceparent`. The user and session a Zharp front-end SDK sends in `baggage`
(`enduser.id`, `session.id`) go on every span and log of the request. A panic is reported as an
unhandled error (and answered with a 500 by `zharp.Handler`; Gin and Echo keep their own recovery);
so is an error a Gin/Echo handler returns that ends in a 5xx.

Outgoing calls: `&http.Client{Transport: zharp.Transport(nil)}`, with `http.NewRequestWithContext`.
Gin and Echo live in their own modules, so the core module doesn't pull them in.

Secret-looking query values (`?token=`, `key`, `sig`, `password`, `session`, …) are recorded as
`[Filtered]` in every span's URL, whatever made the span.

### Request and response bodies

Off by default. With `CaptureBodies`, the spans of incoming requests (`zharp.Handler`, `zharpgin`,
`zharpecho`) and of outgoing calls (`zharp.Transport`) carry what was sent and what came back:

```go
zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api",
	CaptureBodies: &zharp.CaptureBodies{}, // failed requests only; Mode: zharp.BodiesAll for every request
})
```

or `ZHARP_CAPTURE_BODIES=errors` (or `all`) in the environment. More control:
`&zharp.CaptureBodies{Mode: zharp.BodiesAll, MaxSize: 4096, RedactKeys: []string{"dateOfBirth"}, URLs: []string{"api.example.com"}}`.

- The default keeps bodies only for requests that answered 400 and above (or got no response).
- Text — JSON, forms, XML, plain text — is kept up to 32 KB (`MaxSize`, up to 128 KB). Gzip and
  deflate bodies are inflated first.
- Every field name is kept. A file upload (`multipart/form-data`) shows each text field and each
  file as `[file photo.png · image/png · 240 KB]`; inline images and long base64 values as
  `[image/png · 24 KB]`; a body that isn't text as `[binary · application/pdf · 1.2 MB]` or
  `[event stream]`. File bytes are counted as they pass, never kept.
- Values under keys that look secret — `password`, `pwd`, `token`, `secret`, `apiKey`,
  `authorization`, `cookie`, `card`, `cvv`, `ssn` and similar — become `[Filtered]` in your process,
  before anything is sent; under a secret object such as `card`, each value inside is hidden and its
  keys kept. Anything else, such as names and email addresses, is sent as it is: add those keys to
  `RedactKeys`.
- Handlers read and write exactly as before; the SDK keeps a copy of the first bytes only.
- Echo: an error a handler returns is written by Echo after the span ends, so that request's body
  is captured but its error response isn't — unless echo-otel's `OnNextError` writes it inside the
  span (`func(c echo.Context, err error) { c.Error(err) }`).

## Logs

Every log line your app writes should reach Zharp, tied to the request it was written in. There
are two layers: exact captures, which know a log's level, fields and request; and an optional net
under them for anything printed some other way.

**slog and the log package** are bridged: after `Init`, `slog.InfoContext(ctx, …)` — and
`log.Print…` — print as before and go to Zharp with the request's trace (use the `…Context`
variants; the `log` package has no context, so its lines carry no trace). If you set your own
default after `Init`, wrap its handler:

```go
slog.SetDefault(slog.New(zharp.WrapSlogHandler(slog.NewJSONHandler(os.Stdout, nil))))
```

`Config.KeepSlogDefault` leaves `slog.Default` alone; `zharp.SlogHandler()` gives you Zharp's
handler alone. To log to Zharp only:

```go
zharp.Log.Info(ctx, "Order placed", "order_id", 42)   // Debug, Info, Warn, Error, Fatal
```

**zap, logrus, zerolog** — small modules, so the core doesn't pull these libraries in. Each keeps
the logger's output exactly as it was and sends each entry its level lets through, with its fields:

```go
// go get github.com/zharp-io/zharp-go-lib/zharpzap
logger := zharpzap.Wrap(zap.Must(zap.NewProduction()))
logger.Info("Order placed", zap.Int("order_id", 42), zharpzap.Context(ctx))   // Context: the request's trace; not printed

// go get github.com/zharp-io/zharp-go-lib/zharplogrus
zharplogrus.Install()                                   // the standard logger; or Install(myLogger)
logrus.WithContext(ctx).WithField("order_id", 42).Info("Order placed")

// go get github.com/zharp-io/zharp-go-lib/zharpzerolog
logger := zerolog.New(zharpzerolog.Writer(os.Stdout)).Hook(zharpzerolog.TraceHook{})
logger.Info().Ctx(ctx).Int("order_id", 42).Msg("Order placed")
```

zerolog's hooks can't see an event's fields and its writers can't see its context, so
`TraceHook` adds `trace_id` and `span_id` fields (printed too) and `Writer` sends the event in that
trace. `zharplogrus.Install` also has `logrus.Fatal` send what is queued before it exits. These
work when made before `Init`. For any other OpenTelemetry log bridge, `zharp.LoggerProvider()` is
a provider that follows the active `Init`.

**Everything printed: `CaptureOutput`** (off by default)

```go
zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api", CaptureOutput: true})
```

or `ZHARP_CAPTURE_OUTPUT=true`. Every line written to `os.Stdout` and `os.Stderr` — `fmt.Println`,
a `log.Logger`, a logger with no Zharp module, a library — is sent too, and still prints exactly as
before. Its level is read from the line: a JSON record's `level`/`severity` (and its message as the
body), `level=error`, `ERROR`, `[warn]`, zerolog's `ERR`, klog's `E1117 …`; otherwise stdout is
info and stderr a warning. Colour codes are stripped, an unfinished line is sent after 200 ms, a
line is cut at 32 KB, and a line an exact capture above already sent is not sent again. The SDK's
own `[Zharp]` lines are never sent.

It is off by default because Go can't watch a file being written: `Init` swaps `os.Stdout` and
`os.Stderr` for pipes, and a goroutine copies each one to the real output before sending its lines.
That has costs you should accept knowingly:

- A line printed right before `os.Exit` (or `log.Fatal`) can still be in the pipe when the process
  ends, and is then lost **from the output too**. Panics are not affected: the runtime writes them
  straight to the real stderr.
- Code that checks whether stdout is a terminal sees a pipe, so it stops printing colours.
- Only code that reads `os.Stdout`/`os.Stderr` after `Init` is seen: call `Init` first thing in
  `main`, before you build loggers.
- A printed line has lost its goroutine, so it carries no request — unless it is a JSON record with
  its own `trace_id`/`span_id`. Prefer the exact captures above for request logs.

`zharp.Flush` and the shutdown function wait for what was printed to come through. After shutdown
`os.Stdout`/`os.Stderr` are restored; anything still holding the pipe keeps printing.

## Logs only

To send logs and errors without tracing:

```go
shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api", DisableTracing: true})
```

or `ZHARP_TRACING=false`. Logs (`zharp.Log`, `slog`, `log`), errors (panics, 5xx handler errors,
`CaptureError`, with the handled flag), `SetUser` and `SetTag` work as usual. No spans are created
or sent: `zharp.Handler`, `zharpgin` and `zharpecho` make none, `zharp.Tracer()` spans record
nothing (the code inside still runs), and `zharp.Transport` and the global propagator add no
`traceparent` or `baggage` to outgoing requests. Incoming `traceparent` and `baggage` are still
read, so a request's logs and errors carry the caller's trace id, user and session.

## Errors

```go
if err := charge(ctx, order); err != nil {
	zharp.CaptureError(ctx, err, "order_id", order.ID)   // handled
}
```

Go has no global panic hook: `defer zharp.Recover()` at the top of `main` and in goroutines reports
a panic (fatal), sends it, and panics again.

## More

```go
ctx = zharp.SetUser(ctx, zharp.User{ID: account.ID})   // this request's user, when the server knows better
zharp.SetTag("region", "eu-west")                      // on every span and log after
ctx, span := zharp.Tracer().Start(ctx, "render invoice")
defer span.End()
zharp.Flush(ctx)                                       // send now (serverless, end of a job)
```

Telemetry is batched (5 s), gzip-compressed and retried by the OpenTelemetry exporters.

## Development

```bash
go test ./... && for m in zharpgin zharpecho zharpzap zharplogrus zharpzerolog; do (cd $m && go test ./...); done
ZHARP_KEY=zpk_… ZHARP_ENDPOINT=http://localhost:3000/api/v1/otlp go run ./examples/nethttp
```

`zharpgin`, `zharpecho`, `zharpzap`, `zharplogrus` and `zharpzerolog` are separate modules with a
`replace` to the core in this repo; release them with their own tags (`zharpgin/v0.2.0`, …) after
tagging the core (`v0.2.0`).

## License

MIT © [Zharp](https://zharp.io)
