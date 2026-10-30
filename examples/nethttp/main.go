// A net/http server with Zharp. The key and endpoint come from the environment:
//
//	ZHARP_KEY=zpk_… ZHARP_ENDPOINT=http://localhost:3000/api/v1/otlp go run ./examples/nethttp
//
// then: curl localhost:8080/hello/ann · curl localhost:8080/boom
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"

	"github.com/zharp-io/zharp-go"
)

func main() {
	shutdown := zharp.Init(zharp.Config{Service: "go-example", Environment: "development"})
	defer shutdown(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /hello/{name}", func(w http.ResponseWriter, r *http.Request) {
		slog.InfoContext(r.Context(), "Saying hello", "name", r.PathValue("name"))
		fmt.Fprintf(w, "hello %s\n", r.PathValue("name"))
	})
	mux.HandleFunc("GET /boom", func(w http.ResponseWriter, r *http.Request) {
		slog.WarnContext(r.Context(), "About to fail")
		var orders map[string]int
		orders["first"]++ // assignment to a nil map: panics
	})

	addr := ":8080"
	if port := os.Getenv("PORT"); port != "" {
		addr = ":" + port
	}
	srv := &http.Server{Addr: addr, Handler: zharp.Handler(mux)}
	go func() {
		stop := make(chan os.Signal, 1)
		signal.Notify(stop, os.Interrupt)
		<-stop
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != http.ErrServerClosed {
		slog.Error("server stopped", "error", err)
	}
}
