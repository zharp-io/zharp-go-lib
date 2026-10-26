package zharp

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path"
	"runtime"
	"strings"
	"time"
)

// toMessage makes a log message text. Callers pass whatever they have — an error, a map, nil; the
// backend refuses an entry whose message isn't non-empty text, so every path coerces here.
func toMessage(value any) string {
	var text string
	switch v := value.(type) {
	case nil:
		text = ""
	case string:
		text = v
	case error:
		text = v.Error()
	case fmt.Stringer:
		text = v.String()
	case map[string]any, Fields, []any:
		if b, err := json.Marshal(toJSONValue(v)); err == nil {
			text = string(b)
		} else {
			text = fmt.Sprint(v)
		}
	default:
		text = fmt.Sprint(v)
	}
	if strings.TrimSpace(text) == "" {
		return "(empty message)"
	}
	return text
}

// typeName is an error's Go type, e.g. "*fs.PathError" — or "error" for the plain ones made by
// errors.New and fmt.Errorf, whose type says nothing.
func typeName(err error) string {
	name := fmt.Sprintf("%T", err)
	switch name {
	case "*errors.errorString", "*fmt.wrapError", "*fmt.wrapErrors", "*errors.joinError":
		return "error"
	}
	return name
}

// stackOf is where an error came from. A Go error carries no stack, so it's where it was logged,
// from the caller's code — unless the error formats its own with %+v, as errors made by
// github.com/pkg/errors and similar packages do; then that one. Wrapped errors are searched too.
func stackOf(err error) string {
	for e := err; e != nil; e = errors.Unwrap(e) {
		if _, ok := e.(fmt.Formatter); ok {
			detailed := fmt.Sprintf("%+v", e)
			if strings.Contains(detailed, "\n") && detailed != e.Error() {
				return detailed
			}
		}
	}
	return err.Error() + "\n\n" + callerStack()
}

// ownDir is where this package's source is, so its frames can be left out of a stack.
var ownDir = func() string {
	_, file, _, _ := runtime.Caller(0)
	return path.Dir(file)
}()

// isOwnFrame reports whether a frame is inside this package (its tests aside).
func isOwnFrame(file string) bool {
	return path.Dir(file) == ownDir && !strings.HasSuffix(file, "_test.go")
}

// callerStack is the calling goroutine's stack, starting at the caller's code rather than inside
// this package, in the form a Go panic prints.
func callerStack() string {
	pcs := make([]uintptr, 64)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	b.WriteString("goroutine stack (where it was logged):\n")
	for {
		frame, more := frames.Next()
		if !isOwnFrame(frame.File) {
			fmt.Fprintf(&b, "%s()\n\t%s:%d\n", frame.Function, frame.File, frame.Line)
		}
		if !more {
			break
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// errorFields is an error as plain fields: its type, its text and where it came from.
func errorFields(err error) map[string]any {
	return map[string]any{"name": typeName(err), "message": err.Error(), "stack": stackOf(err)}
}

// toJSONMap copies a map value by value, so an error nested in it keeps its fields.
func toJSONMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = toJSONValue(v)
	}
	return out
}

func firstError(fields Fields) error {
	for _, v := range fields {
		if err, ok := v.(error); ok && err != nil {
			return err
		}
	}
	return nil
}

// toJSONValue copies a value into something encoding/json writes without failing: an error as its
// fields, a time as an ISO string, and anything JSON can't hold (NaN, a channel, a func) as text.
func toJSONValue(value any) any {
	switch v := value.(type) {
	case nil, bool, string, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return v
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Sprint(v)
		}
		return v
	case float32:
		return toJSONValue(float64(v))
	case error:
		return errorFields(v)
	case time.Time:
		return iso(v)
	case time.Duration:
		return v.String()
	case json.RawMessage:
		return v
	case Fields:
		return toJSONMap(v)
	case map[string]any:
		return toJSONMap(v)
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = toJSONValue(item)
		}
		return out
	}
	b, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	// Decoded back, so later changes to the caller's value don't reach the queued log.
	var copied any
	if json.Unmarshal(b, &copied) != nil {
		return fmt.Sprint(value)
	}
	return copied
}
