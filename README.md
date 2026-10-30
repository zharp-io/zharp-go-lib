# zharp-go

[Zharp](https://zharp.io) Application Monitoring for Go: every request traced, logs sent with their
trace, panics and errors grouped into issues. A thin layer over the official
[OpenTelemetry Go SDK](https://opentelemetry.io/docs/languages/go/) — `zharp.Init` configures the
OTLP/HTTP exporters, resource, W3C tracecontext + baggage propagation and a `log/slog` bridge, and
sets the global providers, so any OpenTelemetry instrumentation in your app is sent too.

Full documentation: [zharp.io/docs/sdks/go](https://zharp.io/docs/sdks/go)

## Install

```bash
go get github.com/zharp-io/zharp-go
```

## Start it

```go
shutdown := zharp.Init(zharp.Config{Key: "zpk_…", Service: "checkout-api", Environment: "production"})
defer shutdown(context.Background())
```

Copy the key from **Applications → your app → Setup** in Zharp. Leave fields empty to read them
from `ZHARP_KEY`, `ZHARP_SERVICE` (then `OTEL_SERVICE_NAME`), `ZHARP_ENVIRONMENT`,
`ZHARP_RELEASE`, `ZHARP_ENDPOINT`, `ZHARP_SAMPLE_RATE`. `ZHARP_DISABLED=true` turns it off. Init
never panics: without a key it prints one line and sends nothing.

## Trace requests

**net/http**

```go
http.ListenAndServe(":8080", zharp.Handler(mux))
```

**Gin** — `go get github.com/zharp-io/zharp-go/zharpgin`

```go
router.Use(zharpgin.Middleware())
```

**Echo (v4)** — `go get github.com/zharp-io/zharp-go/zharpecho`

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

## Logs

`slog` is bridged: after `Init`, `slog.InfoContext(ctx, …)` — and the `log` package — prints as
before and goes to Zharp with the request's trace (use the `…Context` variants). To log to Zharp
only:

```go
zharp.Log.Info(ctx, "Order placed", "order_id", 42)   // Debug, Info, Warn, Error, Fatal
```

`Config.KeepSlogDefault` leaves `slog.Default` alone; `zharp.SlogHandler()` gives you the handler.

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
go test ./... && (cd zharpgin && go test ./...) && (cd zharpecho && go test ./...)
ZHARP_KEY=zpk_… ZHARP_ENDPOINT=http://localhost:3000/api/v1/otlp go run ./examples/nethttp
```

`zharpgin` and `zharpecho` are separate modules with a `replace` to the core in this repo; release
them with their own tags (`zharpgin/v0.1.0`, `zharpecho/v0.1.0`) after tagging the core (`v0.1.0`).

## License

MIT © [Zharp](https://zharp.io)
