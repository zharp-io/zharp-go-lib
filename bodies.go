package zharp

// Request and response bodies on HTTP spans (Config.CaptureBodies), and secrets in URLs.
//
// The rules are the same in every Zharp SDK (the JavaScript SDK's internal/src/bodies.ts is the
// reference), so a payload reads the same whichever service sent it:
//   - field names stay; values under keys that look secret (password, token, authorization, card,
//     …) become "[Filtered]", and an object under such a key keeps its keys with every value
//     filtered
//   - an inline image or other long base64 value becomes "[image/png · 24 KB]", a file in a form
//     upload "[file photo.png · image/png · 240 KB]", and a body that is not text at all a
//     one-line description of it ("[binary · application/pdf · 1.2 MB]", "[event stream]")
//   - text is capped (32 KB by default), with http.*.body.truncated set when it was cut

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
)

// BodyMode says which requests have their bodies captured.
type BodyMode int

const (
	// BodiesErrors captures bodies of failed requests only: status 400 and above, or no response.
	// It is the zero value, so CaptureBodies{} means errors only.
	BodiesErrors BodyMode = iota
	// BodiesAll captures bodies of every request.
	BodiesAll
)

// CaptureBodies puts request and response bodies on HTTP spans — incoming requests (Handler,
// zharpgin, zharpecho) and outgoing calls (Transport). Set Config.CaptureBodies to turn it on;
// ZHARP_CAPTURE_BODIES=errors|all does the same without code.
//
// Text is kept up to MaxSize; files in uploads, inline images and binary bodies are described
// (name, type, size), never sent. Field names are always kept; values under secret-looking keys
// (password, token, authorization, card, …) are replaced with "[Filtered]" before anything is
// sent. Other personal data in a body is sent as it is: add its keys to RedactKeys.
type CaptureBodies struct {
	// Mode is BodiesErrors (the default: failed requests only) or BodiesAll.
	Mode BodyMode
	// MaxSize is the number of characters kept per body. Default 32768 (32 KB), at most 131072.
	MaxSize int
	// RedactKeys are more keys whose values are replaced with "[Filtered]", on top of the
	// built-in ones. They match a key ignoring case, '-' and '_' ("accountNumber" also covers
	// account_number).
	RedactKeys []string
	// RedactPatterns are tested against keys as they are; a match is filtered.
	RedactPatterns []*regexp.Regexp
	// URLs limits capture to these requests (incoming and outgoing): a bare host
	// ("api.example.com") matches that host, a string with a scheme matches URLs starting with
	// it. URLPatterns are tested against the full URL. Both empty: every request.
	URLs        []string
	URLPatterns []*regexp.Regexp
}

const (
	defaultMaxBody = 32 * 1024
	maxBody        = 128 * 1024
	filtered       = "[Filtered]"
)

const (
	attrRequestBody          = "http.request.body"
	attrRequestBodyTruncated = "http.request.body.truncated"
	attrRequestType          = "http.request.header.content-type"
	attrResponseBody         = "http.response.body"
	attrResponseTruncated    = "http.response.body.truncated"
	attrResponseType         = "http.response.header.content-type"
)

type bodySettings struct {
	mode        BodyMode
	maxSize     int
	extraKeys   []string
	extraRegexp []*regexp.Regexp
	urls        []string
	urlPatterns []*regexp.Regexp
}

// bodySettingsFrom resolves Config.CaptureBodies and ZHARP_CAPTURE_BODIES; nil means off.
func bodySettingsFrom(option *CaptureBodies) *bodySettings {
	if option == nil {
		switch strings.ToLower(strings.TrimSpace(os.Getenv("ZHARP_CAPTURE_BODIES"))) {
		case "errors", "true", "1", "yes", "on":
			option = &CaptureBodies{Mode: BodiesErrors}
		case "all":
			option = &CaptureBodies{Mode: BodiesAll}
		default:
			return nil
		}
	}
	size := defaultMaxBody
	if option.MaxSize > 0 {
		size = min(option.MaxSize, maxBody)
	}
	mode := BodiesErrors
	if option.Mode == BodiesAll {
		mode = BodiesAll
	}
	return &bodySettings{
		mode: mode, maxSize: size,
		extraKeys: option.RedactKeys, extraRegexp: option.RedactPatterns,
		urls: option.URLs, urlPatterns: option.URLPatterns,
	}
}

// rawLimit is how much raw text is read before shortening and capping: inline images shrink to a
// few words, so more is read than is kept.
func (s *bodySettings) rawLimit() int { return max(64*1024, s.maxSize*2) }

// wanted says whether a request with this outcome keeps its bodies. Status 0 means no response.
func (s *bodySettings) wanted(status int) bool {
	return s.mode == BodiesAll || status == 0 || status >= 400
}

// allowed says whether a URL's bodies may be captured.
func (s *bodySettings) allowed(rawURL string) bool {
	if len(s.urls) == 0 && len(s.urlPatterns) == 0 {
		return true
	}
	for _, p := range s.urlPatterns {
		if p.MatchString(rawURL) {
			return true
		}
	}
	return urlMatches(rawURL, s.urls)
}

func (s *bodySettings) isSecret(key string) bool { return isSecretKey(key, s.extraKeys, s.extraRegexp) }

// ── content types ─────────────────────────────────────────────────────────────

func mimeOf(contentType string) string {
	if i := strings.IndexByte(contentType, ';'); i >= 0 {
		contentType = contentType[:i]
	}
	return strings.ToLower(strings.TrimSpace(contentType))
}

func isEventStream(contentType string) bool { return mimeOf(contentType) == "text/event-stream" }
func isMultipart(contentType string) bool   { return mimeOf(contentType) == "multipart/form-data" }
func isForm(contentType string) bool {
	return mimeOf(contentType) == "application/x-www-form-urlencoded"
}

var textualApplication = regexp.MustCompile(`^application/(json|xml|x-www-form-urlencoded|graphql|javascript|x-ndjson|ld\+json)$`)

// isTextual says whether a body of this type is text worth showing. An unknown type is given the
// benefit of the doubt.
func isTextual(contentType string) bool {
	t := mimeOf(contentType)
	if t == "" {
		return true
	}
	if t == "text/event-stream" {
		return false
	}
	return strings.HasPrefix(t, "text/") || strings.HasSuffix(t, "+json") || strings.HasSuffix(t, "+xml") || textualApplication.MatchString(t)
}

// formatSize: "240 KB", "1.4 MB" — rounded as the JavaScript SDK rounds.
func formatSize(bytes int64) string {
	switch {
	case bytes < 1024:
		return strconv.FormatInt(bytes, 10) + " B"
	case bytes < 1024*1024:
		return strconv.FormatInt(int64(math.Floor(float64(bytes)/1024+0.5)), 10) + " KB"
	default:
		tenths := int64(math.Floor(float64(bytes)/(1024*1024)*10 + 0.5))
		return strconv.FormatInt(tenths/10, 10) + "." + strconv.FormatInt(tenths%10, 10) + " MB"
	}
}

// fileLabel is how a file in a form upload is shown: "[file photo.png · image/png · 240 KB]".
// size < 0 means unknown.
func fileLabel(name, contentType string, size int64, partial bool) string {
	if name == "" {
		name = "blob"
	}
	parts := []string{"file " + name}
	if contentType != "" {
		parts = append(parts, contentType)
	}
	if size >= 0 {
		s := formatSize(size)
		if partial {
			s += "+"
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, " · ") + "]"
}

// describeBody is what a body that is not text says instead: "[binary · image/png · 24 KB]",
// "[event stream]". size <= 0 leaves the size out.
func describeBody(contentType string, size int64, partial bool) string {
	if isEventStream(contentType) {
		return "[event stream]"
	}
	parts := []string{"binary"}
	if t := mimeOf(contentType); t != "" {
		parts = append(parts, t)
	}
	if size > 0 {
		s := formatSize(size)
		if partial {
			s += "+"
		}
		parts = append(parts, s)
	}
	return "[" + strings.Join(parts, " · ") + "]"
}

// ── redaction ─────────────────────────────────────────────────────────────────

// Built-in keys whose values never leave the process, compared without case or punctuation.
var (
	secretContains = []string{
		"password", "passwd", "passphrase", "secret", "token", "apikey", "accesskey", "privatekey",
		"authorization", "cookie", "creditcard", "cardnumber", "cvv", "cvc", "ssn", "iban", "sessionid",
		"signature", "credential",
	}
	secretExact = []string{"auth", "pin", "pwd", "otp", "card", "pan", "jwt", "sid", "codeverifier"}
	// Also secret in a query string, where they are API keys, signed-URL signatures and the like.
	urlExact = []string{"key", "sig", "code", "state", "session"}
)

func normalizeKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func isSecretKey(key string, extra []string, patterns []*regexp.Regexp) bool {
	k := normalizeKey(key)
	if k == "" {
		return false
	}
	for _, e := range secretExact {
		if k == e {
			return true
		}
	}
	for _, c := range secretContains {
		if strings.Contains(k, c) {
			return true
		}
	}
	for _, e := range extra {
		if normalizeKey(e) == k {
			return true
		}
	}
	for _, p := range patterns {
		if p.MatchString(key) {
			return true
		}
	}
	return false
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

var (
	dataURIPattern = regexp.MustCompile(`^data:([\w.+-]+/[\w.+-]+)?(?:;[\w=.+-]+)*;base64,([A-Za-z0-9+/=\s]*)$`)
	base64Pattern  = regexp.MustCompile(`^[A-Za-z0-9+/_-]{256,}={0,2}$`)
	base64Hint     = regexp.MustCompile(`[0-9+/]`)
)

// shorten turns a long base64 value into a short description; other strings stay as they are.
func shorten(value string) string {
	if jsLen(value) < 256 {
		return value
	}
	if m := dataURIPattern.FindStringSubmatch(value); m != nil {
		kind := m[1]
		if kind == "" {
			kind = "data"
		}
		return "[" + kind + " · " + formatSize(int64(math.Floor(float64(len(m[2]))*3/4))) + "]"
	}
	if base64Pattern.MatchString(value) && base64Hint.MatchString(value) {
		return "[base64 · " + formatSize(int64(math.Floor(float64(len(value))*3/4))) + "]"
	}
	return value
}

// filterAll filters every value under v and keeps its keys: {brand: "[Filtered]", …}.
func filterAll(v any, depth int) any {
	if depth > 20 {
		return filtered
	}
	switch t := v.(type) {
	case nil:
		return nil
	case string:
		if t == "" {
			return t
		}
		return filtered
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = filterAll(e, depth+1)
		}
		return out
	case *jsonObject:
		out := &jsonObject{index: map[string]int{}}
		for i, k := range t.keys {
			out.set(k, filterAll(t.values[i], depth+1))
		}
		return out
	default:
		return filtered
	}
}

func redactValue(v any, s *bodySettings, depth int) any {
	if str, ok := v.(string); ok {
		return shorten(str)
	}
	if depth > 20 {
		return v
	}
	switch t := v.(type) {
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = redactValue(e, s, depth+1)
		}
		return out
	case *jsonObject:
		out := &jsonObject{index: map[string]int{}}
		for i, k := range t.keys {
			if s.isSecret(k) {
				out.set(k, filterAll(t.values[i], depth+1))
			} else {
				out.set(k, redactValue(t.values[i], s, depth+1))
			}
		}
		return out
	}
	return v
}

var (
	looseRuns  = regexp.MustCompile(`"((?:data:[\w.+-]+/[\w.+-]+(?:;[\w=.+-]+)*;base64,)?[A-Za-z0-9+/_=-]{256,})("?)`)
	loosePairs = regexp.MustCompile(`("([^"\\]{1,100})"\s*:\s*)("(?:[^"\\]|\\.)*"?|-?\d[\d.eE+-]*|true|false)`)
)

// redactLoose handles text that did not parse (cut-off JSON, a body that only looks like JSON):
// "key": value pairs under secret keys are filtered where they appear, and long base64 runs
// shortened.
func redactLoose(text string, s *bodySettings) string {
	text = looseRuns.ReplaceAllStringFunc(text, func(match string) string {
		m := looseRuns.FindStringSubmatch(match)
		short := shorten(m[1])
		if short == m[1] {
			return match
		}
		if m[2] != "" {
			return `"` + short + `"`
		}
		return `"` + short[:len(short)-1] + `+]"`
	})
	return loosePairs.ReplaceAllStringFunc(text, func(match string) string {
		m := loosePairs.FindStringSubmatch(match)
		if s.isSecret(m[2]) {
			return m[1] + `"` + filtered + `"`
		}
		return match
	})
}

func jsDecodeURIComponent(s string) (string, bool) {
	out, err := url.PathUnescape(s)
	return out, err == nil
}

func redactForm(text string, s *bodySettings) string {
	pairs := strings.Split(text, "&")
	for i, pair := range pairs {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		key := pair[:eq]
		if decoded, ok := jsDecodeURIComponent(strings.ReplaceAll(key, "+", " ")); ok {
			key = decoded
		}
		if s.isSecret(key) {
			pairs[i] = pair[:eq] + "=" + filtered
			continue
		}
		value := pair[eq+1:]
		if jsLen(value) >= 256 {
			pairs[i] = pair[:eq] + "=" + shortenEncoded(value)
		}
	}
	return strings.Join(pairs, "&")
}

func shortenEncoded(value string) string {
	decoded, ok := jsDecodeURIComponent(strings.ReplaceAll(value, "+", " "))
	if !ok {
		return value
	}
	if short := shorten(decoded); short != decoded {
		return short
	}
	return value
}

// ── the attribute value ───────────────────────────────────────────────────────

// formatBody is a body as it is sent: redacted, then cut to maxSize characters. cut says the text
// was already cut short where it was read. ok is false for an empty body.
func formatBody(text, contentType string, s *bodySettings, cut bool) (value string, truncated bool, ok bool) {
	if strings.TrimSpace(text) == "" {
		return "", false, false
	}
	body := text
	if isForm(contentType) {
		body = redactForm(body, s)
	} else {
		parsed := false
		trimmed := strings.TrimSpace(body)
		if !cut && (strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")) {
			if v, err := parseOrderedJSON(trimmed); err == nil {
				body = stringifyJS(redactValue(v, s, 0))
				parsed = true
			}
		}
		if !parsed {
			body = redactLoose(body, s)
		}
	}
	n := jsLen(body)
	truncated = cut || n > s.maxSize
	if n > s.maxSize {
		body = jsSlice(body, s.maxSize)
	}
	return body, truncated, true
}

func typeAttribute(side string) string {
	if side == "request" {
		return attrRequestType
	}
	return attrResponseType
}

func bodyAttribute(side string) string {
	if side == "request" {
		return attrRequestBody
	}
	return attrResponseBody
}

func truncatedAttribute(side string) string {
	if side == "request" {
		return attrRequestBodyTruncated
	}
	return attrResponseTruncated
}

// bodyAttributes are the span attributes for one side ("request" or "response") of a text body.
func bodyAttributes(side, text, contentType string, s *bodySettings, cut bool) []attribute.KeyValue {
	if !isTextual(contentType) && !isMultipart(contentType) {
		return nil
	}
	value, truncated, ok := formatBody(text, contentType, s, cut)
	if !ok {
		return nil
	}
	out := []attribute.KeyValue{attribute.String(bodyAttribute(side), value)}
	if truncated {
		out = append(out, attribute.Bool(truncatedAttribute(side), true))
	}
	if contentType != "" {
		out = append(out, attribute.String(typeAttribute(side), contentType))
	}
	return out
}

// describedAttributes are the attributes of a body that is not text: what it was, not what it said.
func describedAttributes(side, contentType string, size int64, partial bool) []attribute.KeyValue {
	out := []attribute.KeyValue{attribute.String(bodyAttribute(side), describeBody(contentType, size, partial))}
	if contentType != "" {
		out = append(out, attribute.String(typeAttribute(side), contentType))
	}
	return out
}

// ── URLs ──────────────────────────────────────────────────────────────────────

var userInfo = regexp.MustCompile(`(?i)^([a-z][a-z0-9+.-]*://)[^/?#@]*@`)

// redactURL is a URL as it is recorded: query values under secret-looking keys (token, key, sig,
// password, …) become "[Filtered]", and user:password@ is dropped.
func redactURL(raw string) string {
	if raw == "" {
		return raw
	}
	out := userInfo.ReplaceAllString(raw, "$1")
	q := strings.IndexByte(out, '?')
	if q < 0 {
		return out
	}
	end := len(out)
	if h := strings.IndexByte(out[q:], '#'); h >= 0 {
		end = q + h
	}
	query := out[q+1 : end]
	if redacted := redactQuery(query); redacted != query {
		out = out[:q+1] + redacted + out[end:]
	}
	return out
}

// redactQuery is a query string (without "?") with secret-looking values filtered.
func redactQuery(query string) string {
	if query == "" {
		return query
	}
	pairs := strings.Split(query, "&")
	for i, pair := range pairs {
		eq := strings.IndexByte(pair, '=')
		if eq < 0 {
			continue
		}
		key := pair[:eq]
		if decoded, ok := jsDecodeURIComponent(strings.ReplaceAll(key, "+", " ")); ok {
			key = decoded
		}
		secret := isSecretKey(key, nil, nil) || contains(urlExact, normalizeKey(key))
		if secret && eq < len(pair)-1 {
			pairs[i] = pair[:eq] + "=" + filtered
		}
	}
	return strings.Join(pairs, "&")
}

// urlMatches: a bare host (with or without a port) matches that host; a string with a scheme
// matches as a prefix.
func urlMatches(raw string, patterns []string) bool {
	var host, hostname string
	if u, err := url.Parse(raw); err == nil {
		host, hostname = u.Host, u.Hostname()
	}
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.Contains(p, "://") {
			if strings.HasPrefix(raw, p) {
				return true
			}
		} else if host == p || hostname == p {
			return true
		}
	}
	return false
}

// ── JSON as JavaScript reads and writes it ─────────────────────────────────────
//
// Payloads are re-serialised the way JSON.stringify(JSON.parse(text)) does in the JavaScript SDK:
// keys in their original order, numbers in JavaScript's shortest form, the same string escapes.

type jsonObject struct {
	keys   []string
	values []any
	index  map[string]int
}

// set keeps a key where it first appeared and its last value, as JSON.parse does.
func (o *jsonObject) set(key string, value any) {
	if i, ok := o.index[key]; ok {
		o.values[i] = value
		return
	}
	o.index[key] = len(o.keys)
	o.keys = append(o.keys, key)
	o.values = append(o.values, value)
}

var errTrailing = errors.New("trailing data")

func parseOrderedJSON(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errTrailing
	}
	return v, nil
}

func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			obj := &jsonObject{index: map[string]int{}}
			for dec.More() {
				keyTok, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyTok.(string)
				if !ok {
					return nil, errors.New("bad key")
				}
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				obj.set(key, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return obj, nil
		case '[':
			arr := []any{}
			for dec.More() {
				v, err := decodeValue(dec)
				if err != nil {
					return nil, err
				}
				arr = append(arr, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, err
			}
			return arr, nil
		}
		return nil, errors.New("unexpected delimiter")
	case json.Number:
		f, err := strconv.ParseFloat(string(t), 64)
		if err != nil && !errors.Is(err, strconv.ErrRange) {
			return nil, err
		}
		return f, nil
	default:
		return t, nil // string, bool, nil
	}
}

func stringifyJS(v any) string {
	var b bytes.Buffer
	writeJS(&b, v)
	return b.String()
}

func writeJS(b *bytes.Buffer, v any) {
	switch t := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if t {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case float64:
		b.WriteString(jsNumber(t))
	case string:
		b.WriteString(jsQuote(t))
	case []any:
		b.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				b.WriteByte(',')
			}
			writeJS(b, e)
		}
		b.WriteByte(']')
	case *jsonObject:
		b.WriteByte('{')
		for i, idx := range jsKeyOrder(t.keys) {
			if i > 0 {
				b.WriteByte(',')
			}
			b.WriteString(jsQuote(t.keys[idx]))
			b.WriteByte(':')
			writeJS(b, t.values[idx])
		}
		b.WriteByte('}')
	default:
		b.WriteString("null")
	}
}

// jsKeyOrder is the order JavaScript keeps an object's keys in: array-index keys ("0", "17") first,
// ascending, then the rest as they were added.
func jsKeyOrder(keys []string) []int {
	var indexes, others []int
	for i, k := range keys {
		if isArrayIndex(k) {
			indexes = append(indexes, i)
		} else {
			others = append(others, i)
		}
	}
	if len(indexes) == 0 {
		return others
	}
	sort.SliceStable(indexes, func(a, b int) bool {
		x, _ := strconv.ParseUint(keys[indexes[a]], 10, 64)
		y, _ := strconv.ParseUint(keys[indexes[b]], 10, 64)
		return x < y
	})
	return append(indexes, others...)
}

func isArrayIndex(k string) bool {
	if k == "" || len(k) > 10 || (len(k) > 1 && k[0] == '0') {
		return false
	}
	n, err := strconv.ParseUint(k, 10, 64)
	return err == nil && n < math.MaxUint32
}

// jsNumber formats a number as JavaScript's Number#toString does (JSON.stringify uses it).
func jsNumber(f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "null"
	}
	if f == 0 {
		return "0"
	}
	sign := ""
	if f < 0 {
		sign, f = "-", -f
	}
	// Shortest round-trip digits and exponent: d.ddde±x
	e := strconv.FormatFloat(f, 'e', -1, 64)
	mant, expPart, _ := strings.Cut(e, "e")
	digits := strings.Replace(mant, ".", "", 1)
	exp, _ := strconv.Atoi(expPart)
	k, n := len(digits), exp+1
	switch {
	case k <= n && n <= 21:
		return sign + digits + strings.Repeat("0", n-k)
	case 0 < n && n <= 21:
		return sign + digits[:n] + "." + digits[n:]
	case -6 < n && n <= 0:
		return sign + "0." + strings.Repeat("0", -n) + digits
	}
	expSign := "+"
	if n-1 < 0 {
		expSign = "-"
	}
	expAbs := strconv.Itoa(int(math.Abs(float64(n - 1))))
	if k == 1 {
		return sign + digits + "e" + expSign + expAbs
	}
	return sign + digits[:1] + "." + digits[1:] + "e" + expSign + expAbs
}

// jsQuote quotes a string as JSON.stringify does: only ", \ and control characters are escaped.
func jsQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\b':
			b.WriteString(`\b`)
		case '\f':
			b.WriteString(`\f`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(`\u00`)
				b.WriteByte("0123456789abcdef"[r>>4])
				b.WriteByte("0123456789abcdef"[r&0xf])
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// jsLen is a string's length in UTF-16 code units, as JavaScript counts it.
func jsLen(s string) int {
	n := 0
	for _, r := range s {
		if r >= 0x10000 {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// jsSlice is the first n UTF-16 code units of s. A character that would be split in two is left
// out (JavaScript would keep half of it).
func jsSlice(s string, n int) string {
	count := 0
	for i, r := range s {
		w := 1
		if r >= 0x10000 {
			w = 2
		}
		if count+w > n {
			return s[:i]
		}
		count += w
	}
	return s
}

// validText turns bytes into text the way a JavaScript runtime decodes UTF-8: invalid bytes become
// U+FFFD, and one at the very end (a character cut at the cap) is dropped.
func validText(raw []byte) string {
	s := string(raw)
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "\uFFFD")
	}
	return strings.TrimSuffix(s, "\uFFFD")
}
