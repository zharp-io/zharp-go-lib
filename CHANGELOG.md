# Changelog

## v0.5.0 (`github.com/zharp-io/zharp-go-lib` and its integrations; new: `…/zharpsql` v0.1.0) — 2026-11-21

- Request and response headers on HTTP spans (`DisableHeaders`, `ZHARP_CAPTURE_HEADERS=false` to turn off); secrets are sent as `[Filtered]`.
- New `zharpsql`: wraps `database/sql` and any driver (GORM, sqlx, bun on top), traces each query and reports a failed one once, with the call site.

## v0.4.0 (`github.com/zharp-io/zharp-go-lib`, `…/zharpgin`, `…/zharpecho`; new: `…/zharpzap`, `…/zharplogrus`, `…/zharpzerolog` v0.1.0) — 2026-11-17

**Behaviour change:** slog and the `log` package are no longer sent to Zharp unless `Logs` is on (`ZHARP_LOGS=true`). Lines logged in a request end with `trace_id=… span_id=…` and the Zharp agent brings them in; errors, traces and `zharp.Log` are sent as before.

- Logs are off by default (`Config.Logs`, `ZHARP_LOGS=true` turns them on): what the app logs and
  prints reaches Zharp through the Zharp agent, and sending it from the SDK too made every line
  arrive twice. Errors, traces and `zharp.Log` are sent as before. Turn logs on where there is no
  agent (serverless). Breaking for apps that relied on slog being sent: set `Logs: zharp.Bool(true)`.
- Every line logged during a request is stamped with its ids, so Zharp links the agent's copy to the
  request: a text line ends with ` trace_id=<32 hex> span_id=<16 hex>`, a JSON line gets `trace_id`
  and `span_id` fields. slog's default, `WrapSlogHandler`, `zharpzap`, `zharplogrus` and
  `zharpzerolog` stamp; lines outside a request and lines with their own `trace_id` are left alone.
  On by default; `Config.LogCorrelation` / `ZHARP_LOG_CORRELATION=false` turns it off.
- `zharp.Bool`, `zharp.LogIDs` and `zharp.StampLine` (for logging integrations).
- Every log line, whatever it is written with:
  - `zharpzap` (`Wrap`, `Core`, `Context`), `zharplogrus` (`Install`, `Hook`) and `zharpzerolog`
    (`Writer`, `TraceHook`) modules send zap, logrus and zerolog logs with their level, fields and
    the request's trace; the logger prints exactly as before.
  - `zharp.WrapSlogHandler` sends a slog handler's records too (for a `slog.SetDefault` after
    `Init`); `zharp.LoggerProvider` is a provider for any OpenTelemetry log bridge that follows the
    active `Init`.
  - `Config.CaptureOutput` / `ZHARP_CAPTURE_OUTPUT=true` (with logs on): every line printed to `os.Stdout` and
    `os.Stderr` is sent too, level read from the line, never a line an exact capture already sent.
    Off by default (it swaps `os.Stdout`/`os.Stderr` for pipes; see the README for why).
- slog's default now sends a record to Zharp before printing it.
- The new modules need the core module from this release (their `go.mod` says v0.3.0 until it is
  tagged; the `replace` covers development).

## v0.3.0 (`github.com/zharp-io/zharp-go-lib`, `…/zharpgin`, `…/zharpecho`)

- `Config.CaptureBodies` / `ZHARP_CAPTURE_BODIES=errors|all`: request and response bodies on HTTP
  spans — `zharp.Handler`, `zharp.Transport`, `zharpgin`, `zharpecho` — as `http.request.body` /
  `http.response.body` (with `.truncated` and the content types). Off by default; failed requests
  only unless `Mode: BodiesAll`. Field names kept, secret values `[Filtered]` (a secret object keeps
  its keys), uploads listed by field with files as `[file name · type · size]`, inline images and
  binary bodies described, 32 KB default / 128 KB max. Same rules and output as the other Zharp SDKs.
- Secret-looking query values are `[Filtered]` in `url.full`, `url.query`, `http.url` and
  `http.target` on every span, with or without `CaptureBodies`.
- `zharp.CaptureRequestBodies` / `RequestBodies` for router integrations.
- `zharpgin` and `zharpecho` need the core module from this release for `CaptureBodies`.

## v0.2.0 (`github.com/zharp-io/zharp-go-lib`, `…/zharpgin`, `…/zharpecho`)

- First release of Zharp Application Monitoring for Go, on OpenTelemetry: `zharp.Init` (OTLP/HTTP
  traces + logs with gzip, resource, tracecontext + baggage, slog bridge), `zharp.Handler`
  (otelhttp), `zharp.Transport`, `zharpgin` (otelgin) and `zharpecho` (labstack/echo-otel) modules.
- `enduser.id` / `session.id` from baggage on every span and log; `zharp.SetUser`, `zharp.SetTag`.
- Panics and 5xx handler errors reported unhandled; `zharp.CaptureError` (handled); `zharp.Recover`.
- `zharp.Log.*`, `zharp.Flush`, `zharp.Tracer`.
- Logs only: `Config.DisableTracing` / `ZHARP_TRACING=false` sends logs and errors with no spans and
  no outgoing `traceparent`/`baggage`; incoming ones are still read for trace id, user and session.
- Replaces the logs-only 0.1.x API of this module (breaking): `Init` now returns a shutdown func;
  logging moves to `zharp.Log.*(ctx, msg, args...)` and `slog`; `CaptureError` and `Flush` take a
  `context.Context`; `NewSlogHandler` → `SlogHandler`, `Middleware` → `Handler`; `Close`, `Fields`,
  `SetContext`, `SessionID`, `IsActive`, `LogService` and the package-level
  `Debug`…`Fatal` are removed.

## v0.1.1

- A rejected API key (HTTP 401/403) stops sending for an hour with one warning, instead of every batch going to a dead key; sending resumes, with a notice, once the key is accepted again.

## v0.1.0

- First release: `zharp.Init` and `Debug`/`Info`/`Warn`/`Error`/`Fatal`, from `Config` or `ZHARP_*` environment variables.
- Errors carry their type and a stack (where logged, or their own via `%+v`).
- `NewSlogHandler` for `log/slog`.
- `Recover` and `Middleware` for panics.
- Background sending, batches, retries; `Close` and `Flush` send what is left.
