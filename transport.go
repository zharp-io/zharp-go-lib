package zharp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// maxRequestBytes is the largest body one request carries. The backend accepts 10 MB; staying far
// below it keeps one slow upload from holding everything behind it. An entry larger than this on
// its own is still sent alone rather than dropped.
const maxRequestBytes = 1_000_000

// rejectedBackoff is how long sending stops once Zharp refuses the API key (401/403): one body is
// tried again after it, and while the key is still refused the wait starts over.
const rejectedBackoff = time.Hour

// transport sends batches of log entries to {endpoint}/agent/logs as {"logs": [...]}.
type transport struct {
	apiKey string
	url    string
	client *http.Client
	// retryDelay: the first retry waits twice this, the second four times.
	retryDelay time.Duration

	// A refused key (401/403) stops sending until retryAt, with one warning — not every batch
	// sent to a dead key, forever. now is time.Now, replaceable in tests.
	mu       sync.Mutex
	rejected bool
	retryAt  time.Time
	now      func() time.Time
}

// send sends entries, split into bodies by size. It reports whether every body was taken or
// refused for good (a 4xx) — false when the network or the server failed throughout.
func (t *transport) send(entries []map[string]any) bool {
	delivered := true
	for _, body := range splitBySize(entries, maxRequestBytes) {
		// While the key is refused, entries are dropped: nothing queues up in memory.
		if t.backedOff() {
			return true
		}
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
		status, err := t.post(body)
		if err == nil && (status == http.StatusUnauthorized || status == http.StatusForbidden) {
			t.refused(status)
			return true
		}
		if err == nil && status < 500 {
			if status < 300 {
				t.accepted()
			}
			return true
		}
		if attempt < 2 {
			time.Sleep(t.retryDelay * time.Duration(2<<attempt))
		}
	}
	return false
}

func (t *transport) clock() time.Time {
	if t.now != nil {
		return t.now()
	}
	return time.Now()
}

// backedOff reports whether sending is stopped because Zharp refused the key. Once the wait is
// over it lets one body through to try again.
func (t *transport) backedOff() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.rejected && t.clock().Before(t.retryAt)
}

// refused stops sending for rejectedBackoff; the warning is printed once, not on every retry.
func (t *transport) refused(status int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.rejected {
		announce(fmt.Sprintf("Zharp rejected this API key (HTTP %d) — logs are not being sent. Check ZHARP_API_KEY.", status))
	}
	t.rejected = true
	t.retryAt = t.clock().Add(rejectedBackoff)
}

// accepted resumes sending after a refusal, and says so once.
func (t *transport) accepted() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.rejected {
		t.rejected = false
		announce("Zharp accepted the API key again — logs are being sent.")
	}
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
