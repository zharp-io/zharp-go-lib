package zharp

import (
	"context"
	"log/slog"
	"time"
)

// SlogOptions configures NewSlogHandler.
type SlogOptions struct {
	// Level is the lowest level sent. slog.LevelInfo when nil.
	Level slog.Leveler
}

// NewSlogHandler returns a log/slog handler that sends records to Zharp, at the matching level
// (slog.LevelWarn → warn; above slog.LevelError → fatal). Attributes become the log's fields,
// groups nested; an error attribute brings its stack. Start Zharp with Init first.
//
//	slog.SetDefault(slog.New(zharp.NewSlogHandler(nil)))
//	slog.Warn("Stock low", "sku", "SKU-1")
//
// To keep printing to the console too, send records to both handlers — see the README.
func NewSlogHandler(opts *SlogOptions) slog.Handler {
	h := &slogHandler{level: slog.LevelInfo}
	if opts != nil && opts.Level != nil {
		h.level = opts.Level
	}
	return h
}

type slogHandler struct {
	level  slog.Leveler
	attrs  []slog.Attr
	groups []string
}

func (h *slogHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level.Level() && IsActive()
}

func (h *slogHandler) Handle(_ context.Context, record slog.Record) error {
	s := active()
	if s == nil {
		return nil
	}
	fields := Fields{}
	// Attributes from WithAttrs belong under the groups opened before them; a record's own
	// attributes under all of them.
	target := fields
	for _, attr := range h.attrs {
		addAttr(target, attr)
	}
	for _, g := range h.groups {
		next := Fields{}
		target[g] = next
		target = next
	}
	record.Attrs(func(attr slog.Attr) bool {
		addAttr(target, attr)
		return true
	})
	at := record.Time
	if at.IsZero() {
		at = time.Now()
	}
	level := slogLevel(record.Level)
	var err error
	findError(fields, &err)
	var message any = record.Message
	entryFields := fields
	if err != nil {
		if _, has := entryFields["stack"]; !has {
			entryFields = Fields{}
			for k, v := range fields {
				entryFields[k] = v
			}
			entryFields["name"] = typeName(err)
			entryFields["stack"] = stackOf(err)
		}
	}
	func() {
		defer func() { _ = recover() }()
		s.pipeline.push(buildEntry(s, level, message, entryFields, "", at), urgent(level))
	}()
	return nil
}

func (h *slogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	next := *h
	next.attrs = append(append([]slog.Attr(nil), h.attrs...), nestUnder(h.groups, attrs)...)
	return &next
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	next := *h
	next.groups = append(append([]string(nil), h.groups...), name)
	return &next
}

// nestUnder puts attrs inside the groups already open, so a later group doesn't capture them.
func nestUnder(groups []string, attrs []slog.Attr) []slog.Attr {
	if len(groups) == 0 {
		return attrs
	}
	args := make([]any, len(attrs))
	for i, a := range attrs {
		args[i] = a
	}
	attr := slog.Group(groups[len(groups)-1], args...)
	for i := len(groups) - 2; i >= 0; i-- {
		attr = slog.Group(groups[i], attr)
	}
	return []slog.Attr{attr}
}

func addAttr(fields Fields, attr slog.Attr) {
	value := attr.Value.Resolve()
	if attr.Key == "" && value.Kind() != slog.KindGroup {
		return
	}
	if value.Kind() == slog.KindGroup {
		inner := fields
		if attr.Key != "" {
			existing, ok := fields[attr.Key].(Fields)
			if !ok {
				existing = Fields{}
				fields[attr.Key] = existing
			}
			inner = existing
		}
		for _, a := range value.Group() {
			addAttr(inner, a)
		}
		return
	}
	fields[attr.Key] = value.Any()
}

func findError(fields Fields, found *error) {
	for _, v := range fields {
		if *found != nil {
			return
		}
		switch x := v.(type) {
		case error:
			*found = x
		case Fields:
			findError(x, found)
		}
	}
}

func slogLevel(level slog.Level) Level {
	switch {
	case level > slog.LevelError:
		return LevelFatal
	case level >= slog.LevelError:
		return LevelError
	case level >= slog.LevelWarn:
		return LevelWarn
	case level >= slog.LevelInfo:
		return LevelInfo
	default:
		return LevelDebug
	}
}
