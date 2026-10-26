package zharp

import (
	"errors"
	"fmt"
	"net/http"
	"runtime/debug"
	"time"
)

// crashFlushTimeout is how long a panicking program waits for its last logs to go out.
const crashFlushTimeout = 2 * time.Second

// Recover sends a panic as a fatal log with its stack, waits briefly for it to go out, then
// panics again — so the program ends as it would have. Defer it first thing in main, and in any
// goroutine whose panics you want to see:
//
//	func main() {
//		zharp.Init(zharp.Config{})
//		defer zharp.Close()
//		defer zharp.Recover()
//		…
//	}
//
// Go has no global hook for panics, so a goroutine without it ends the program unseen.
func Recover() {
	value := recover()
	if value == nil {
		return
	}
	reportPanic(value, LevelFatal, debug.Stack(), nil)
	Flush(crashFlushTimeout)
	panic(value)
}

// Middleware sends a panic in an HTTP handler as an error log with its stack and the request's
// method and path, then panics again so net/http handles it as it would have (it ends that
// request and keeps serving).
//
//	http.ListenAndServe(":8080", zharp.Middleware(mux))
func Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			value := recover()
			if value == nil {
				return
			}
			// http.ErrAbortHandler is how a handler deliberately ends a response, not a failure.
			if err, ok := value.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
				reportPanic(value, LevelError, debug.Stack(), Fields{"method": r.Method, "path": r.URL.Path})
			}
			panic(value)
		}()
		next.ServeHTTP(w, r)
	})
}

func reportPanic(value any, level Level, stack []byte, fields Fields) {
	s := active()
	if s == nil {
		return
	}
	defer func() { _ = recover() }() // reporting a panic must never panic again
	meta := Fields{"type": "panic", "stack": fmt.Sprintf("panic: %v\n\n%s", value, stack)}
	for k, v := range fields {
		meta[k] = v
	}
	message := fmt.Sprint(value)
	if err, ok := value.(error); ok {
		meta["name"] = typeName(err)
		message = err.Error()
	}
	s.pipeline.push(buildEntry(s, level, "panic: "+message, meta, "", time.Now()), true)
}
