# Changelog

## 0.1.0 (`github.com/zharp-io/zharp-go`)

- First release of Zharp Application Monitoring for Go, on OpenTelemetry: `zharp.Init` (OTLP/HTTP
  traces + logs with gzip, resource, tracecontext + baggage, slog bridge), `zharp.Handler`
  (otelhttp), `zharp.Transport`, `zharpgin` (otelgin) and `zharpecho` (labstack/echo-otel) modules.
- `enduser.id` / `session.id` from baggage on every span and log; `zharp.SetUser`, `zharp.SetTag`.
- Panics and 5xx handler errors reported unhandled; `zharp.CaptureError` (handled); `zharp.Recover`.
- `zharp.Log.*`, `zharp.Flush`, `zharp.Tracer`.
- Logs only: `Config.DisableTracing` / `ZHARP_TRACING=false` sends logs and errors with no spans and
  no outgoing `traceparent`/`baggage`; incoming ones are still read for trace id, user and session.
- Replaces the logs-only `github.com/zharp-io/zharp-go-lib` 0.1.x package.
