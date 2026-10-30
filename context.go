package zharp

import (
	"context"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/baggage"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// The attributes Zharp joins telemetry on. A browser or mobile app sends enduser.id and
// session.id in W3C baggage with each request; the processors below copy them onto every span and
// log of that request.
const (
	attrUserID    = "enduser.id"
	attrUserEmail = "enduser.email"
	attrUserName  = "enduser.name"
	attrSessionID = "session.id"
)

var propagated = []string{attrUserID, attrSessionID}
var inherited = []string{attrUserID, attrUserEmail, attrUserName, attrSessionID}

// User is who a request is for.
type User struct {
	ID    string
	Email string
	Name  string
}

type userKey struct{}

// SetUser returns a context whose spans and logs carry this user (enduser.id, enduser.email,
// enduser.name), and puts it on the span already in ctx. A front end using a Zharp SDK sends its
// user with each request, so this is for when the server knows better:
//
//	ctx := zharp.SetUser(r.Context(), zharp.User{ID: account.ID})
//	r = r.WithContext(ctx)
func SetUser(ctx context.Context, user User) context.Context {
	if span := trace.SpanFromContext(ctx); span.IsRecording() {
		span.SetAttributes(user.attributes()...)
	}
	return context.WithValue(ctx, userKey{}, user)
}

func (u User) attributes() []attribute.KeyValue {
	var out []attribute.KeyValue
	if u.ID != "" {
		out = append(out, attribute.String(attrUserID, u.ID))
	}
	if u.Email != "" {
		out = append(out, attribute.String(attrUserEmail, u.Email))
	}
	if u.Name != "" {
		out = append(out, attribute.String(attrUserName, u.Name))
	}
	return out
}

var (
	tagsMu sync.RWMutex
	tags   = map[string]string{}
)

// SetTag puts an attribute on every span and log from now on, across the program. An empty value
// removes it.
func SetTag(key, value string) {
	tagsMu.Lock()
	defer tagsMu.Unlock()
	if value == "" {
		delete(tags, key)
	} else {
		tags[key] = value
	}
}

// contextAttributes: tags, then user and session from the parent span, baggage and SetUser
// (later wins).
func contextAttributes(ctx context.Context) map[string]string {
	out := map[string]string{}
	tagsMu.RLock()
	for k, v := range tags {
		out[k] = v
	}
	tagsMu.RUnlock()

	if parent, ok := trace.SpanFromContext(ctx).(sdktrace.ReadOnlySpan); ok {
		for _, kv := range parent.Attributes() {
			for _, key := range inherited {
				if string(kv.Key) == key && kv.Value.AsString() != "" {
					out[key] = kv.Value.AsString()
				}
			}
		}
	}

	bag := baggage.FromContext(ctx)
	for _, key := range propagated {
		if v := bag.Member(key).Value(); v != "" {
			out[key] = v
		}
	}

	if user, ok := ctx.Value(userKey{}).(User); ok {
		delete(out, attrUserID)
		delete(out, attrUserEmail)
		delete(out, attrUserName)
		for _, kv := range user.attributes() {
			out[string(kv.Key)] = kv.Value.AsString()
		}
	}
	return out
}

// contextSpanProcessor puts user, session and tags on every span as it starts.
type contextSpanProcessor struct{}

func (contextSpanProcessor) OnStart(parent context.Context, s sdktrace.ReadWriteSpan) {
	for k, v := range contextAttributes(parent) {
		s.SetAttributes(attribute.String(k, v))
	}
}
func (contextSpanProcessor) OnEnd(sdktrace.ReadOnlySpan)      {}
func (contextSpanProcessor) Shutdown(context.Context) error   { return nil }
func (contextSpanProcessor) ForceFlush(context.Context) error { return nil }

// contextLogProcessor puts user, session and tags on every log record, before it is batched.
type contextLogProcessor struct{}

func (contextLogProcessor) OnEmit(ctx context.Context, record *sdklog.Record) error {
	attrs := contextAttributes(ctx)
	if len(attrs) == 0 {
		return nil
	}
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		delete(attrs, string(kv.Key))
		return true
	})
	for k, v := range attrs {
		record.AddAttributes(attribute.String(k, v))
	}
	return nil
}

// Enabled is false: this processor only decorates; the exporting processor decides.
func (contextLogProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return false }
func (contextLogProcessor) Shutdown(context.Context) error                         { return nil }
func (contextLogProcessor) ForceFlush(context.Context) error                       { return nil }

func toValue(v any) attribute.Value {
	switch x := v.(type) {
	case string:
		return attribute.StringValue(x)
	case bool:
		return attribute.BoolValue(x)
	case int:
		return attribute.IntValue(x)
	case int64:
		return attribute.Int64Value(x)
	case int32:
		return attribute.Int64Value(int64(x))
	case uint32:
		return attribute.Int64Value(int64(x))
	case float64:
		return attribute.Float64Value(x)
	case float32:
		return attribute.Float64Value(float64(x))
	case error:
		return attribute.StringValue(x.Error())
	case fmt.Stringer:
		return attribute.StringValue(x.String())
	default:
		return attribute.StringValue(fmt.Sprint(v))
	}
}

// keyValues turns slog-style arguments ("order_id", 42, "plan", "pro") into log attributes.
func keyValues(args []any) []attribute.KeyValue {
	var out []attribute.KeyValue
	for i := 0; i < len(args); i++ {
		switch k := args[i].(type) {
		case string:
			if i+1 < len(args) {
				out = append(out, attribute.KeyValue{Key: attribute.Key(k), Value: toValue(args[i+1])})
				i++
			} else {
				out = append(out, attribute.String("!BADKEY", k))
			}
		case attribute.KeyValue:
			out = append(out, k)
		default:
			out = append(out, attribute.KeyValue{Key: "!BADKEY", Value: toValue(k)})
		}
	}
	return out
}
