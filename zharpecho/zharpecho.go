// Package zharpecho traces Echo (v4) requests for Zharp. Call zharp.Init first, then:
//
//	e := echo.New()
//	e.Use(zharpecho.Middleware())
//
// Each request gets one SERVER span, continuing the caller's trace from traceparent and carrying
// the user and session from baggage. A panic, or an error a handler returns that ends in a 5xx,
// is reported to Zharp as an unhandled error; panics still reach middleware.Recover (or net/http).
//
// The span comes from github.com/labstack/echo-otel/v4, Echo's own OpenTelemetry middleware —
// the successor of the deprecated contrib otelecho. It is its own module so the core zharp module
// doesn't depend on Echo.
package zharpecho

import (
	"errors"
	"net/http"

	echootel "github.com/labstack/echo-otel/v4"
	"github.com/labstack/echo/v4"

	"github.com/zharp-io/zharp-go"
)

// Middleware returns Echo's OpenTelemetry middleware with Zharp's error reporting inside its span.
// config is optional and passed on to echo-otel.
func Middleware(config ...echootel.Config) echo.MiddlewareFunc {
	cfg := echootel.Config{}
	if len(config) > 0 {
		cfg = config[0]
	}
	traced, err := cfg.ToMiddleware()
	if err != nil {
		panic("zharpecho: " + err.Error())
	}
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return traced(report(next))
	}
}

// report runs inside the request span.
func report(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		defer func() {
			if value := recover(); value != nil {
				if err, ok := value.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
					zharp.ReportPanic(c.Request().Context(), value)
				}
				panic(value)
			}
		}()
		err := next(c)
		if err != nil && echootel.ResolveResponseStatus(c.Response(), err) >= 500 {
			zharp.ReportUnhandledError(c.Request().Context(), err)
		}
		return err
	}
}
