package zharp

// The process's output as logs: every line written to os.Stdout and os.Stderr that the exact
// captures (slog, the log package, zharpzap, zharplogrus, zharpzerolog) didn't already send.
//
// Go can't hook writes to a file, so Config.CaptureOutput swaps os.Stdout and os.Stderr for the
// write ends of two pipes. One goroutine per pipe copies every byte straight on to the real
// stdout/stderr first and only then hands a copy to a second goroutine that cuts it into lines and
// sends them. The app's output is unchanged and never waits on Zharp: when the sender falls behind,
// lines are dropped from capture, never from the output.
//
// What it can't do, and why it is off by default:
//   - A line printed just before os.Exit (or log.Fatal) can still be in the pipe when the process
//     ends, and is then lost from the output too. (Panics are safe: the runtime writes them to
//     the real stderr, not through os.Stderr.)
//   - Code that sees a pipe where it saw a terminal stops printing colours.
//   - Only code that reads os.Stdout/os.Stderr after Init is seen: a logger built before Init
//     keeps writing to the real file.
//   - A line from a pipe has lost its goroutine, so it carries no request — unless it is a JSON
//     record with its own trace_id/span_id. The exact captures keep the request's context.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/trace"
)

const (
	maxLine     = 32 * 1024
	lineFlushIn = 200 * time.Millisecond
	// Chunks waiting to be cut into lines; beyond this, capture drops them (output never does).
	chunkQueue = 1024
)

type outLevel struct {
	severity otellog.Severity
	text     string
}

var (
	lvlTrace = outLevel{otellog.SeverityTrace, "TRACE"}
	lvlDebug = outLevel{otellog.SeverityDebug, "DEBUG"}
	lvlInfo  = outLevel{otellog.SeverityInfo, "INFO"}
	lvlWarn  = outLevel{otellog.SeverityWarn, "WARN"}
	lvlError = outLevel{otellog.SeverityError, "ERROR"}
	lvlFatal = outLevel{otellog.SeverityFatal, "FATAL"}
)

var levelWords = map[string]outLevel{
	"trace": lvlTrace, "trc": lvlTrace,
	"verbose": lvlDebug, "debug": lvlDebug, "dbg": lvlDebug,
	"info": lvlInfo, "inf": lvlInfo, "notice": lvlInfo, "log": lvlInfo,
	"warn": lvlWarn, "warning": lvlWarn, "wrn": lvlWarn,
	"error": lvlError, "err": lvlError, "eror": lvlError,
	"crit": lvlFatal, "critical": lvlFatal, "fatal": lvlFatal, "ftl": lvlFatal, "emerg": lvlFatal,
	"alert": lvlFatal, "panic": lvlFatal, "pnc": lvlFatal, "dpanic": lvlFatal,
}

var (
	ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// The first level word in the opening of a line: `ERROR`, `[warn]`, `level=error`, zerolog's
	// console `INF`/`WRN`/`ERR`.
	levelWord = regexp.MustCompile(`(?i)(?:^|[\s\[(|:=])(trace|trc|verbose|debug|dbg|info|inf|notice|warn(?:ing)?|wrn|err(?:or)?|eror|crit(?:ical)?|fatal|ftl|emerg|alert|d?panic|pnc)(?:$|[\s\]):|,])`)
	// glog/klog: `E1117 08:33:00.123456 …`
	klog = regexp.MustCompile(`^([IWEF])\d{4} \d\d:\d\d:\d\d`)
)

var klogLevels = map[string]outLevel{"I": lvlInfo, "W": lvlWarn, "E": lvlError, "F": lvlFatal}

type parsedLine struct {
	level *outLevel
	body  string
	span  trace.SpanContext // from a JSON record's own trace_id/span_id
}

func levelOf(v any) *outLevel {
	switch x := v.(type) {
	case float64: // pino/bunyan-style numbers
		for _, step := range []struct {
			n float64
			l outLevel
		}{{60, lvlFatal}, {50, lvlError}, {40, lvlWarn}, {30, lvlInfo}, {20, lvlDebug}, {0, lvlTrace}} {
			if x >= step.n {
				l := step.l
				return &l
			}
		}
	case string:
		if l, ok := levelWords[strings.ToLower(strings.TrimSpace(x))]; ok {
			return &l
		}
	}
	return nil
}

func firstString(record map[string]any, keys ...string) (string, bool) {
	for _, k := range keys {
		if s, ok := record[k].(string); ok {
			return s, true
		}
	}
	return "", false
}

// parseOutputLine reads a line for its level and, for JSON records, its own message and trace.
func parseOutputLine(raw string) parsedLine {
	line := strings.TrimRight(ansi.ReplaceAllString(raw, ""), " \t\r\n")
	if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
		var record map[string]any
		if json.Unmarshal([]byte(line), &record) == nil {
			out := parsedLine{body: line}
			for _, k := range []string{"level", "severity", "lvl"} {
				if v, ok := record[k]; ok {
					out.level = levelOf(v)
					break
				}
			}
			if msg, ok := firstString(record, "msg", "message", "event"); ok {
				out.body = msg
			}
			out.span = spanOf(record)
			return out
		}
	}
	if m := klog.FindStringSubmatch(line); m != nil {
		l := klogLevels[m[1]]
		return parsedLine{level: &l, body: line}
	}
	head := line
	if len(head) > 120 {
		head = head[:120]
	}
	out := parsedLine{body: line}
	if m := levelWord.FindStringSubmatch(head); m != nil {
		out.level = levelOf(m[1])
	}
	return out
}

func spanOf(record map[string]any) trace.SpanContext {
	tid, _ := firstString(record, "trace_id", "traceId", "traceID", "trace.id")
	sid, _ := firstString(record, "span_id", "spanId", "spanID", "span.id")
	traceID, err := trace.TraceIDFromHex(tid)
	if err != nil {
		return trace.SpanContext{}
	}
	cfg := trace.SpanContextConfig{TraceID: traceID, Remote: true, TraceFlags: trace.FlagsSampled}
	if spanID, err := trace.SpanIDFromHex(sid); err == nil {
		cfg.SpanID = spanID
	}
	return trace.NewSpanContext(cfg)
}

// ── what was already sent ─────────────────────────────────────────────────────

// Bodies the exact captures sent in the last few seconds. A printed line containing one is the
// same log reaching the output, and is not sent again.
const (
	recentFor = 3 * time.Second
	recentMax = 500
)

type sentBody struct {
	at            time.Time
	text, escaped string
}

var (
	recentMu sync.Mutex
	recent   []sentBody
)

func normalise(s string) string { return strings.Join(strings.Fields(s), " ") }

func rememberSent(body string) {
	text := normalise(body)
	if len(text) < 4 {
		return
	}
	escaped := text
	if b, err := json.Marshal(text); err == nil {
		escaped = string(b[1 : len(b)-1])
	}
	recentMu.Lock()
	recent = append(recent, sentBody{time.Now(), text, escaped})
	if len(recent) > recentMax {
		recent = append(recent[:0:0], recent[len(recent)-recentMax:]...)
	}
	recentMu.Unlock()
}

func alreadySent(line string) bool {
	recentMu.Lock()
	defer recentMu.Unlock()
	now := time.Now()
	drop := 0
	for drop < len(recent) && now.Sub(recent[drop].at) > recentFor {
		drop++
	}
	recent = recent[drop:]
	if len(recent) == 0 {
		return false
	}
	text := normalise(line)
	for i := len(recent) - 1; i >= 0; i-- {
		if strings.Contains(text, recent[i].text) || strings.Contains(text, recent[i].escaped) {
			recent = append(recent[:i:i], recent[i+1:]...)
			return true
		}
	}
	return false
}

// ── the pipes ─────────────────────────────────────────────────────────────────

// outputOn is true while lines are being captured; the sent-body processor only remembers then.
var outputOn atomic.Bool

type outStream struct {
	name     string
	fallback outLevel
	target   **os.File // &os.Stdout or &os.Stderr
	original *os.File
	w        *os.File
	chunks   chan []byte
	flushReq chan chan struct{}
	marks    atomic.Int32  // sync marks written and not yet seen
	synced   chan struct{} // a mark came through
}

var (
	outMu   sync.Mutex
	streams []*outStream // nil until first started; the pipes then live as long as the process
)

// startOutputCapture swaps os.Stdout and os.Stderr for the pipes (making them on first use).
func startOutputCapture() {
	outMu.Lock()
	defer outMu.Unlock()
	if streams == nil {
		for _, s := range []*outStream{
			{name: "stdout", fallback: lvlInfo, target: &os.Stdout},
			{name: "stderr", fallback: lvlWarn, target: &os.Stderr},
		} {
			if *s.target == nil {
				continue
			}
			r, w, err := os.Pipe()
			if err != nil {
				continue
			}
			s.original, s.w = *s.target, w
			s.chunks = make(chan []byte, chunkQueue)
			s.flushReq = make(chan chan struct{})
			s.synced = make(chan struct{}, 8)
			go s.forward(r)
			go s.lines()
			streams = append(streams, s)
		}
	}
	for _, s := range streams {
		*s.target = s.w
	}
	outputOn.Store(len(streams) > 0)
}

// stopOutputCapture puts os.Stdout and os.Stderr back. The pipes keep forwarding, so anything that
// kept hold of them still prints; nothing more is sent.
func stopOutputCapture() {
	outMu.Lock()
	defer outMu.Unlock()
	outputOn.Store(false)
	for _, s := range streams {
		if *s.target == s.w {
			*s.target = s.original
		}
	}
}

// originalStderr is the real stderr, for the SDK's own lines and the slog console handler.
func originalStderr() *os.File {
	outMu.Lock()
	defer outMu.Unlock()
	for _, s := range streams {
		if s.name == "stderr" {
			return s.original
		}
	}
	return os.Stderr
}

// syncMark is written into a pipe by drainOutput and taken out by its forwarder before the
// output: once the forwarder has seen it, everything printed before the call has come through. A
// write this small is never split or interleaved with another.
var syncMark = []byte("\x00\x1bzharp-sync\x00")

// drainOutput waits for what was printed to come through the pipes, then sends unfinished lines.
func drainOutput(ctx context.Context) {
	if !outputOn.Load() {
		return
	}
	outMu.Lock()
	list := append([]*outStream(nil), streams...)
	outMu.Unlock()
	deadline := time.Now().Add(500 * time.Millisecond)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	for _, s := range list {
		for stale := true; stale; { // a mark from a drain that gave up waiting
			select {
			case <-s.synced:
			default:
				stale = false
			}
		}
		s.marks.Add(1)
		if _, err := s.w.Write(syncMark); err != nil {
			s.marks.Add(-1)
			continue
		}
		select {
		case <-s.synced:
		case <-time.After(time.Until(deadline)):
			continue
		}
		done := make(chan struct{})
		select {
		case s.flushReq <- done:
			select {
			case <-done:
			case <-time.After(time.Until(deadline) + 50*time.Millisecond):
			}
		case <-time.After(time.Until(deadline) + 50*time.Millisecond):
		}
	}
}

// forward copies the pipe to the real file before anything else, and never stops reading.
func (s *outStream) forward(r *os.File) {
	buf := make([]byte, 32*1024)
	var carry []byte
	for {
		n, err := r.Read(buf)
		if n > 0 {
			data := buf[:n]
			synced := 0
			if s.marks.Load() > 0 || len(carry) > 0 {
				data = append(carry, data...)
				carry = nil
				for {
					i := bytes.Index(data, syncMark)
					if i < 0 {
						break
					}
					data = append(data[:i:i], data[i+len(syncMark):]...)
					s.marks.Add(-1)
					synced++
				}
				// A mark cut between two reads: hold its start back for the rest, which is on its way.
				if s.marks.Load() > 0 {
					for k := min(len(syncMark)-1, len(data)); k > 0; k-- {
						if bytes.HasSuffix(data, syncMark[:k]) {
							carry = append([]byte(nil), data[len(data)-k:]...)
							data = data[:len(data)-k]
							break
						}
					}
				}
			}
			if len(data) > 0 {
				_, _ = s.original.Write(data)
				if outputOn.Load() {
					select {
					case s.chunks <- append([]byte(nil), data...):
					default: // behind: this chunk isn't captured; it was printed
					}
				}
			}
			for ; synced > 0; synced-- {
				select {
				case s.synced <- struct{}{}:
				default:
				}
			}
		}
		if err != nil {
			if len(carry) > 0 {
				_, _ = s.original.Write(carry)
			}
			return
		}
	}
}

// lines cuts the copies into lines and sends them; an unfinished line goes after 200 ms.
func (s *outStream) lines() {
	var pending []byte
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	timing := false
	flush := func() {
		if timing {
			timer.Stop()
			timing = false
		}
		if len(pending) > 0 {
			line := string(pending)
			pending = pending[:0]
			s.emit(line)
		}
	}
	for {
		select {
		case chunk := <-s.chunks:
			pending = append(pending, chunk...)
			for {
				i := bytes.IndexByte(pending, '\n')
				if i < 0 {
					break
				}
				line := string(pending[:i])
				pending = pending[i+1:]
				s.emit(line)
			}
			if len(pending) == 0 {
				pending = nil
				if timing {
					timer.Stop()
					timing = false
				}
			} else if len(pending) > maxLine {
				flush()
			} else if !timing {
				timer.Reset(lineFlushIn)
				timing = true
			}
		case <-timer.C:
			timing = false
			flush()
		case done := <-s.flushReq:
			for more := true; more; {
				select {
				case chunk := <-s.chunks:
					pending = append(pending, chunk...)
				default:
					more = false
				}
			}
			for _, line := range strings.SplitAfter(string(pending), "\n") {
				s.emit(line)
			}
			pending = pending[:0]
			if timing {
				timer.Stop()
				timing = false
			}
			close(done)
		}
	}
}

func (s *outStream) emit(line string) {
	defer func() { _ = recover() }() // a line that can't be sent is not worth the process
	if !outputOn.Load() || strings.TrimSpace(line) == "" || strings.Contains(line, "[Zharp]") {
		return
	}
	st := active()
	if st == nil || alreadySent(line) {
		return
	}
	if len(line) > maxLine {
		line = line[:maxLine]
	}
	p := parseOutputLine(line)
	if strings.TrimSpace(p.body) == "" {
		return
	}
	level := s.fallback
	if p.level != nil {
		level = *p.level
	}
	ctx := context.Background()
	if p.span.IsValid() {
		ctx = trace.ContextWithSpanContext(ctx, p.span)
	}
	var r otellog.Record
	now := time.Now()
	r.SetTimestamp(now)
	r.SetObservedTimestamp(now)
	r.SetSeverity(level.severity)
	r.SetSeverityText(level.text)
	r.SetBody(attribute.StringValue(p.body))
	r.AddAttributes(attribute.String("log.source", s.name), attribute.String("log.iostream", s.name))
	st.logger.Emit(ctx, r)
}

// sentLogProcessor remembers what the exact captures send while output is captured, so the same
// line reaching the output is recognised. It runs inside Emit, before the caller prints — the
// slog fanout, zharpzap and zharpzerolog send first and print second, logrus fires hooks first.
type sentLogProcessor struct{}

func (sentLogProcessor) OnEmit(_ context.Context, record *sdklog.Record) error {
	if !outputOn.Load() {
		return nil
	}
	fromOutput := false
	record.WalkAttributes(func(kv attribute.KeyValue) bool {
		if kv.Key == "log.iostream" {
			fromOutput = true
			return false
		}
		return true
	})
	if !fromOutput && record.Body().Type() == attribute.STRING {
		rememberSent(record.Body().AsString())
	}
	return nil
}

// Enabled is false: this processor only watches; the exporting processor decides.
func (sentLogProcessor) Enabled(context.Context, sdklog.EnabledParameters) bool { return false }
func (sentLogProcessor) Shutdown(context.Context) error                         { return nil }
func (sentLogProcessor) ForceFlush(context.Context) error                       { return nil }
