// Package zharpmux names zharp.Handler's spans after gorilla/mux routes. gorilla/mux doesn't set
// the Go 1.22 request pattern zharp.Handler reads, so without it every request's span is named by
// its method alone. Call zharp.Init first, then:
//
//	router := mux.NewRouter()
//	router.Use(zharpmux.Middleware())
//	http.ListenAndServe(":8080", zharp.Handler(router)) // or zharp.Handler(negroniApp)
//
// The span, the trace, panics, headers and bodies are zharp.Handler's; this only adds the route
// ("GET /orders/{id}") once gorilla/mux has matched one.
//
// It is its own module so the core zharp module doesn't depend on gorilla/mux.
package zharpmux

import (
	"net/http"

	"github.com/gorilla/mux"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Middleware sets http.route and the span name from the route gorilla/mux matched. Register it
// with router.Use; mux runs it for matched routes only.
func Middleware() mux.MiddlewareFunc {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if route := mux.CurrentRoute(r); route != nil {
				if template, err := route.GetPathTemplate(); err == nil && template != "" {
					if span := trace.SpanFromContext(r.Context()); span.IsRecording() {
						span.SetAttributes(attribute.String("http.route", template))
						span.SetName(r.Method + " " + template)
					}
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}
