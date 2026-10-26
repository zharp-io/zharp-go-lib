package zharp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// maxRequestBytes is the largest body one request carries. The backend accepts 10 MB; staying far
// below it keeps one slow upload from holding everything behind it. An entry larger than this on
// its own is still sent alone rather than dropped.
const maxRequestBytes = 1_000_000

// transport sends batches of log entries to {endpoint}/agent/logs as {"logs": [...]}.
type transport struct {
	apiKey string
	url    string
	client *http.Client
	// retryDelay: the first retry waits twice this, the second four times.
	retryDelay time.Duration
}

// send sends entries, split into bodies by size. It reports whether every body was taken or
// refused for good (a 4xx) — false when the network or the server failed throughout.
func (t *transport) send(entries []map[string]any) bool {
	delivered := true
	for _, body := range splitBySize(entries, maxRequestBytes) {
		if !t.sendBody(body) {
			delivered = false
		}
	}
	return delivered
}

// sendBody makes three attempts in all. Only network errors and 5xx responses are retried — a 4xx
// won't be fixed by sending the same thing again.
func (t *transport) sendBody(body []byte) bool {
	for attempt := 0; attempt < 3; attempt++ {
		if status, err := t.post(body); err == nil && status < 500 {
			return true
		}
		if attempt < 2 {
			time.Sleep(t.retryDelay * time.Duration(2<<attempt))
		}
	}
	return false
}

func (t *transport) post(body []byte) (int, error) {
	req, err := http.NewRequest(http.MethodPost, t.url, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	// The key goes in a header, never the URL: URLs end up in proxy logs.
	req.Header.Set("X-API-KEY", t.apiKey)
	req.Header.Set("X-Zharp-SDK", sdkName)
	req.Header.Set("User-Agent", sdkName)
	resp, err := t.client.Do(req)
	if err != nil {
		return 0, err
	}
	// Read to the end so the connection can be reused.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// splitBySize makes {"logs": [...]} bodies of at most limit bytes each. An entry too large to fit
// even alone still gets a body of its own: whether it can be delivered is for the network to decide.
func splitBySize(entries []map[string]any, limit int) [][]byte {
	overhead := len(`{"logs":[]}`)
	var bodies [][]byte
	var group [][]byte
	size := overhead
	flushGroup := func() {
		bodies = append(bodies, append(append([]byte(`{"logs":[`), bytes.Join(group, []byte(","))...), ']', '}'))
		group = nil
		size = overhead
	}
	for _, entry := range entries {
		encoded, err := json.Marshal(entry)
		if err != nil {
			continue // every value was made JSON-safe when the log was made; this can't happen
		}
		entrySize := len(encoded) + 1 // +1 for the separating comma
		if len(group) > 0 && size+entrySize > limit {
			flushGroup()
		}
		group = append(group, encoded)
		size += entrySize
	}
	if len(group) > 0 {
		flushGroup()
	}
	return bodies
}
