# Changelog

## Unreleased

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
