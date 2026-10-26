# Zharp Logs for Go

Send your Go app's logs and panics to [Zharp](https://zharp.io). No dependencies — only the standard
library — and Go 1.21 and up. Works on its own or as a `log/slog` handler.

Full documentation: [zharp.io/docs/sdks/go](https://zharp.io/docs/sdks/go)

---

## 1. Install

```bash
go get github.com/zharp-io/zharp-go-lib
```

```go
import zharp "github.com/zharp-io/zharp-go-lib"
```

## 2. Get an API key

1. In Zharp, open **Integrations & API** and choose **Main API keys**.
2. Click **Create API key**, give it a name, and tick **Ingest only**. That key can send logs but
   can't read anything, so it's safe in a server's environment.
3. Click **Create key** and copy the key — it's shown only once.

<p>
  <img src="https://www.zharp.io/images/docs/api-key-create.png" alt="Create API key, with Ingest only ticked" width="48%">
  <img src="https://www.zharp.io/images/docs/api-key-created.png" alt="The new key, shown once with a Copy button" width="48%">
</p>

## 3. Start it

Call `Init` first thing in `main`, and **defer `Close`** — Go runs nothing at exit by itself, so
without it the last logs are lost:

```go
func main() {
	zharp.Init(zharp.Config{
		APIKey:      "your-ingest-only-key",
		Service:     "checkout-api",
		Environment: "production",
	})
	defer zharp.Close()
	defer zharp.Recover() // send a panic before the program dies of it

	// …
}
```

Or leave the config empty and set the environment instead:

```bash
ZHARP_API_KEY=your-ingest-only-key
ZHARP_SERVICE=checkout-api
ZHARP_ENV=production
```

```go
zharp.Init(zharp.Config{})
```

You can mix them: what's set in `Config` is used, and anything left empty is read from the
environment. Without a service name, the last part of your module path is used
(`github.com/acme/checkout-api` → `checkout-api`).

When it starts, it prints one line to stderr so you know it's working:

```
[Zharp] Logging active — service "checkout-api", environment "production"
```

With no API key anywhere it prints how to give it one and sends nothing. It never returns an error
or panics.

## 4. Log

```go
zharp.Info("User signed in", zharp.Fields{"user_id": 42})
zharp.Warn("Slow network detected", zharp.Fields{"latency_ms": 3200})

if err := charge(order); err != nil {
	zharp.Error("Payment failed", zharp.Fields{"order_id": order.ID, "err": err})
	// or the error alone:
	zharp.CaptureError(err)
}
```

`Debug` and `Fatal` are there too — `Fatal` logs at fatal but, unlike `log.Fatal`, doesn't exit.
Fields are optional and are kept with the log. An error — as the message or among the fields —
brings its type and a stack: Go errors don't carry one, so it's where the error was logged; an error
that formats its own with `%+v` (as `github.com/pkg/errors` does) keeps that one.

Logs are sent from a background goroutine in batches every 5 seconds; `Error` and `Fatal` go at
once. Logging never blocks on the network and never panics.

## 5. See your logs

Sign in at [zharp.io](https://zharp.io/login), open **Logs**, and pick your service in the service
dropdown.

![The Logs page on zharp.io](https://www.zharp.io/images/docs/logs-dashboard.png)

---

## log/slog

Send your `slog` logs to Zharp:

```go
slog.SetDefault(slog.New(zharp.NewSlogHandler(nil)))

slog.Warn("Stock low", "sku", "SKU-1")
slog.Error("Pricing failed", "err", err)
```

Attributes become the log's fields (groups nested), an error attribute brings its stack, and levels
map as you'd expect. `NewSlogHandler(&zharp.SlogOptions{Level: slog.LevelWarn})` sends only `WARN`
and up; the default is `INFO`.

To keep printing to the console as well, pass each record to both handlers — a few lines:

```go
type both []slog.Handler

func (b both) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range b {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}
func (b both) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range b {
		if h.Enabled(ctx, r.Level) {
			_ = h.Handle(ctx, r.Clone())
		}
	}
	return nil
}
func (b both) WithAttrs(a []slog.Attr) slog.Handler {
	out := make(both, len(b))
	for i, h := range b {
		out[i] = h.WithAttrs(a)
	}
	return out
}
func (b both) WithGroup(g string) slog.Handler {
	out := make(both, len(b))
	for i, h := range b {
		out[i] = h.WithGroup(g)
	}
	return out
}

slog.SetDefault(slog.New(both{slog.NewTextHandler(os.Stderr, nil), zharp.NewSlogHandler(nil)}))
```

Where the [Zharp agent](https://zharp.io/docs/agent/setup) already collects your app's output
(Docker, journald, Kubernetes), send logs through one of them, not both, or every line arrives
twice.

## Panics

Go has no global hook for panics, so capturing them takes one line where they can happen:

- **`defer zharp.Recover()`** in `main`, and at the top of any goroutine you start. A panic is sent
  as a `fatal` log with its stack, then it carries on — Go prints it and the program ends as it
  would have.
- **`zharp.Middleware(handler)`** around an HTTP handler. A panic in a request is sent as an `error`
  log with the stack and the request's method and path, then `net/http` handles it as usual — it ends
  that request and keeps serving.

```go
http.ListenAndServe(":8080", zharp.Middleware(mux))
```

## Options

| `Config` field | Environment variable | Default |
|---|---|---|
| `APIKey` | `ZHARP_API_KEY` | — |
| `Service` | `ZHARP_SERVICE`, then `OTEL_SERVICE_NAME` | the last part of your module path, else `unknown-service` |
| `Environment` | `ZHARP_ENV` | none |
| `AppVersion` | `ZHARP_VERSION` | none |
| `Source` | `ZHARP_SOURCE` | none |
| `Endpoint` | `ZHARP_ENDPOINT` | `https://api.zharp.io/api/v1` |
| `FlushInterval` | — | 5 seconds |
| `MaxBatchSize` | — | 50 |
| `HTTPClient` | — | a client with a 15-second timeout |

A field set in `Config` wins. `ZHARP_DISABLED=true` turns the SDK off.

## What's sent automatically

Every log carries a session ID (the same for one run of the program) and where it ran: hostname,
process ID, Go version, OS and architecture. Add your own fields to every log with
`zharp.SetContext(zharp.Fields{"region": "eu-west"})`; start a new session ID with
`zharp.ResetSessionID()`.

`zharp.Flush(5 * time.Second)` sends what's queued and waits for it — call it before a serverless
function returns.

## Releasing (maintainers)

Go modules are published by tag — there's no registry upload. To release:

1. Update `Version` in `zharp.go` and `CHANGELOG.md`, run `go vet ./... && go test ./...`, commit
   and push.
2. Tag and push the tag: `git tag v0.1.1 && git push origin v0.1.1`.
3. Optionally ask the module proxy to pick it up now, so it shows on
   [pkg.go.dev](https://pkg.go.dev/github.com/zharp-io/zharp-go-lib) straight away:
   `GOPROXY=https://proxy.golang.org go list -m github.com/zharp-io/zharp-go-lib@v0.1.1`.

A version is permanent once fetched — the proxy caches it forever — so never move or reuse a tag;
release a new one instead.

## License

MIT © [Zharp](https://zharp.io)
