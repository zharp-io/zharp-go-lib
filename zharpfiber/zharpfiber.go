// Package zharpfiber traces Fiber (v2) requests for Zharp. Call zharp.Init first, then:
//
//	app := fiber.New()
//	app.Use(recover.New())          // optional; before zharpfiber, so panics are reported with their stack
//	app.Use(zharpfiber.Middleware())
//
// Each request gets one SERVER span named by its route ("GET /orders/:id"), continuing the
// caller's trace from traceparent and carrying the user and session from baggage, with the
// request's and response's headers (secrets [Filtered]) and, with zharp.Config.CaptureBodies, its
// bodies. A panic, or an error a handler returns that ends in a 5xx, is reported to Zharp as an
// unhandled error. Panics still reach Fiber's recover middleware as before.
//
// A handler's error is answered by the app's ErrorHandler inside the span, so the span has the
// status the client got (as Fiber's logger and otelfiber do); the middleware then returns nil.
//
// Handlers get the request's context from c.UserContext(): pass it to slog.InfoContext,
// zharp.CaptureError and outgoing calls so they join the request.
//
// With zharp.Config.DisableTracing (logs only) no span is made or sent; traceparent and baggage are
// still read, so the request's logs and errors carry its trace id, user and session.
//
// It is its own module so the core zharp module doesn't depend on Fiber.
package zharpfiber

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	zharp "github.com/zharp-io/zharp-go-lib"
)

const scope = "github.com/zharp-io/zharp-go-lib/zharpfiber"

// Version is this module's version.
const Version = "0.1.0"

// Middleware traces every request that reaches it. Register it with app.Use before the routes.
func Middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		rc := c.Context()
		req := httpRequest(rc)
		saved := c.UserContext()
		ctx := otel.GetTextMapPropagator().Extract(saved, propagation.HeaderCarrier(req.Header))
		// Named by the route once routing has happened; the method alone if nothing matched.
		ctx, span := otel.Tracer(scope, trace.WithInstrumentationVersion(Version)).Start(ctx, req.Method,
			trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(requestAttributes(rc, req)...))
		c.SetUserContext(ctx)
		bodies := zharp.CaptureRequestBodies(req)

		defer func() {
			// Fiber has no recovery of its own: the panic goes on to recover.New() (or ends the
			// process, as it would have). The span and the report are done first.
			if value := recover(); value != nil {
				if err, ok := value.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
					zharp.ReportPanic(ctx, value)
				}
				end(c, span, req, bodies, http.StatusInternalServerError, true)
				c.SetUserContext(saved)
				panic(value)
			}
		}()

		reported := false
		if err := c.Next(); err != nil {
			// What Fiber would do with it after the chain returns, done here so the span sees the
			// status the client gets.
			if catch := c.App().ErrorHandler(c, err); catch != nil {
				_ = c.SendStatus(fiber.StatusInternalServerError)
			}
			if c.Response().StatusCode() >= 500 {
				zharp.ReportUnhandledError(ctx, err)
				reported = true
			}
		}
		end(c, span, req, bodies, c.Response().StatusCode(), reported)
		c.SetUserContext(saved)
		return nil
	}
}

// end puts the route and the response on the span and ends it.
func end(c *fiber.Ctx, span trace.Span, req *http.Request, bodies *zharp.RequestBodies, status int, reported bool) {
	defer span.End()
	if !span.IsRecording() {
		return
	}
	defer func() { _ = recover() }() // never break a request over its telemetry
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	if route := routeOf(c); route != "" {
		span.SetAttributes(attribute.String("http.route", route))
		span.SetName(req.Method + " " + route)
	}
	if status >= 500 && !reported {
		span.SetStatus(codes.Error, "")
	}
	rc := c.Context()
	response := responseHeader(&rc.Response.Header)
	if bodies != nil {
		// fasthttp has the whole request body in memory: it goes through the copy-keeping reader
		// now, once the handlers are done with it.
		// A streamed body (Server.StreamRequestBody, SetBodyStream) is left alone, on either
		// side: reading it here would consume it.
		if !rc.Request.IsBodyStream() {
			_, _ = io.Copy(io.Discard, bodies.Request(io.NopCloser(bytes.NewReader(rc.Request.Body()))))
		}
		if !rc.Response.IsBodyStream() {
			bodies.Wrote(rc.Response.Body(), response)
		}
		bodies.End(span, status)
	}
	zharp.RecordHeaders(span, req.Header, response)
}

// routeOf is the template of the route that matched ("/orders/:id"), or "" when only middleware
// (app.Use) ran — a 404, a 405, or a catch-all handler registered with Use.
func routeOf(c *fiber.Ctx) string {
	route := c.Route()
	if route == nil || route.Path == "" || isUse(route) {
		return ""
	}
	return strings.Clone(route.Path)
}

// isUse reports whether the route was registered with Use. Fiber keeps that in an unexported
// field; it is read (never written) by reflection.
func isUse(route *fiber.Route) bool {
	v := reflect.ValueOf(route).Elem().FieldByName("use")
	return v.IsValid() && v.Kind() == reflect.Bool && v.Bool()
}
