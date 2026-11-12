// Package zharpecho traces Echo (v4) requests for Zharp. Call zharp.Init first, then:
//
//	e := echo.New()
//	e.Use(zharpecho.Middleware())
//
// Each request gets one SERVER span, continuing the caller's trace from traceparent and carrying
// the user and session from baggage. A panic, or an error a handler returns that ends in a 5xx,
// is reported to Zharp as an unhandled error; panics still reach middleware.Recover (or net/http).
//
// With zharp.Config.DisableTracing (logs only) no span is made or sent; traceparent and baggage are
// still read, so the request's logs and errors carry its trace id, user and session.
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
	"go.opentelemetry.io/otel/trace"

	zharp "github.com/zharp-io/zharp-go-lib"
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
	return func(c echo.Context) (err error) {
		// zharp.Config.CaptureBodies: the handler reads and writes through a copy-keeping layer,
		// and the bodies go on the span before it ends. A handler that returns an error without
		// writing has its error response written by Echo after the span ended: that request's
		// body is captured, the response's isn't (set echo-otel's Config.OnNextError to
		// c.Error, as echo-otel suggests for the status code, to have it written inside).
		bodies := zharp.CaptureRequestBodies(c.Request())
		if bodies != nil {
			request, response := c.Request(), c.Response()
			body, writer := request.Body, response.Writer
			request.Body, response.Writer = bodies.Request(body), bodies.ResponseWriter(writer)
			defer func() { request.Body, response.Writer = body, writer }()
		}
		span := trace.SpanFromContext(c.Request().Context())
		defer func() {
			if value := recover(); value != nil {
				if err, ok := value.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
					zharp.ReportPanic(c.Request().Context(), value)
				}
				bodies.End(span, http.StatusInternalServerError)
				panic(value)
			}
			bodies.End(span, echootel.ResolveResponseStatus(c.Response(), err))
		}()
		err = next(c)
		if err != nil && echootel.ResolveResponseStatus(c.Response(), err) >= 500 {
			zharp.ReportUnhandledError(c.Request().Context(), err)
		}
		return err
	}
}
