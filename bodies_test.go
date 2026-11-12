package zharp

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
)

// testdata/bodies_vectors.json is produced by the JavaScript SDK's internal/src/bodies.ts — the
// reference for every SDK — from the same inputs; the Go port must give the same bytes.
type vectors struct {
	Bodies []struct {
		Settings  string  `json:"settings"`
		Text      string  `json:"text"`
		Type      string  `json:"type"`
		Cut       bool    `json:"cut"`
		Value     *string `json:"value"`
		Truncated bool    `json:"truncated"`
	} `json:"bodies"`
	URLs []struct {
		In  string `json:"in"`
		Out string `json:"out"`
	} `json:"urls"`
	Sizes []struct {
		In  int64  `json:"in"`
		Out string `json:"out"`
	} `json:"sizes"`
	Labels []struct {
		Name    string `json:"name"`
		Type    string `json:"type"`
		Size    int64  `json:"size"`
		Partial bool   `json:"partial"`
		Out     string `json:"out"`
	} `json:"labels"`
	Describe []struct {
		Type string `json:"type"`
		Size int64  `json:"size"`
		Out  string `json:"out"`
	} `json:"describe"`
	Secret []struct {
		Key string `json:"key"`
		Out bool   `json:"out"`
	} `json:"secret"`
}

var vectorSettings = map[string]*CaptureBodies{
	"def":   {Mode: BodiesErrors},
	"small": {Mode: BodiesAll, MaxSize: 60},
	"extra": {Mode: BodiesAll, RedactKeys: []string{"dateOfBirth", "account_number"}},
}

func loadVectors(t *testing.T) vectors {
	t.Helper()
	raw, err := os.ReadFile("testdata/bodies_vectors.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vectors
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestBodiesMatchJavaScript(t *testing.T) {
	v := loadVectors(t)
	for i, c := range v.Bodies {
		t.Run(fmt.Sprintf("body %d", i), func(t *testing.T) {
			s := bodySettingsFrom(vectorSettings[c.Settings])
			value, truncated, ok := formatBody(c.Text, c.Type, s, c.Cut)
			if c.Value == nil {
				if ok {
					t.Fatalf("want no body, got %q", value)
				}
				return
			}
			if !ok || value != *c.Value || truncated != c.Truncated {
				t.Fatalf("\n got %q (truncated %v)\nwant %q (truncated %v)", value, truncated, *c.Value, c.Truncated)
			}
		})
	}
	for _, c := range v.URLs {
		if got := redactURL(c.In); got != c.Out {
			t.Errorf("redactURL(%q)\n got %q\nwant %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Sizes {
		if got := formatSize(c.In); got != c.Out {
			t.Errorf("formatSize(%d) = %q, want %q", c.In, got, c.Out)
		}
	}
	for _, c := range v.Labels {
		if got := fileLabel(c.Name, c.Type, c.Size, c.Partial); got != c.Out {
			t.Errorf("fileLabel = %q, want %q", got, c.Out)
		}
	}
	for _, c := range v.Describe {
		if got := describeBody(c.Type, c.Size, false); got != c.Out {
			t.Errorf("describeBody(%q, %d) = %q, want %q", c.Type, c.Size, got, c.Out)
		}
	}
	for _, c := range v.Secret {
		if got := isSecretKey(c.Key, nil, nil); got != c.Out {
			t.Errorf("isSecretKey(%q) = %v, want %v", c.Key, got, c.Out)
		}
	}
}

func TestBodySettings(t *testing.T) {
	if bodySettingsFrom(nil) != nil {
		t.Fatal("off by default")
	}
	s := bodySettingsFrom(&CaptureBodies{})
	if s.mode != BodiesErrors || s.maxSize != 32768 || s.rawLimit() != 65536 {
		t.Fatalf("defaults: %+v", s)
	}
	if bodySettingsFrom(&CaptureBodies{MaxSize: 1 << 20}).maxSize != 131072 {
		t.Fatal("maxSize is capped at 128 KB")
	}
	if !s.wanted(0) || !s.wanted(404) || s.wanted(200) {
		t.Fatal("errors: no response and 4xx/5xx only")
	}
	if !bodySettingsFrom(&CaptureBodies{Mode: BodiesAll}).wanted(200) {
		t.Fatal("all: every request")
	}
	t.Setenv("ZHARP_CAPTURE_BODIES", "all")
	if s := bodySettingsFrom(nil); s == nil || s.mode != BodiesAll {
		t.Fatal("ZHARP_CAPTURE_BODIES=all")
	}
	t.Setenv("ZHARP_CAPTURE_BODIES", "errors")
	if s := bodySettingsFrom(nil); s == nil || s.mode != BodiesErrors {
		t.Fatal("ZHARP_CAPTURE_BODIES=errors")
	}
	t.Setenv("ZHARP_CAPTURE_BODIES", "off")
	if bodySettingsFrom(nil) != nil {
		t.Fatal("ZHARP_CAPTURE_BODIES=off")
	}
	u := bodySettingsFrom(&CaptureBodies{URLs: []string{"api.example.com", "https://pay.test/v1"}})
	for url, want := range map[string]bool{
		"https://api.example.com:8443/x": true, "https://pay.test/v1/charge": true, "https://pay.test/v2": false, "https://other.test/": false,
	} {
		if u.allowed(url) != want {
			t.Errorf("allowed(%q) != %v", url, want)
		}
	}
}

func TestJSNumbers(t *testing.T) {
	for in, want := range map[float64]string{
		1: "1", 1.5: "1.5", -2.25: "-2.25", 1e21: "1e+21", 1e20: "100000000000000000000", 1e-7: "1e-7",
		0.000001: "0.000001", 123.456: "123.456", 1.2345e-10: "1.2345e-10", 5e300: "5e+300",
	} {
		if got := jsNumber(in); got != want {
			t.Errorf("jsNumber(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestJSSliceKeepsCharactersWhole(t *testing.T) {
	if got := jsSlice("ab😀c", 3); got != "ab" {
		t.Fatalf("got %q: an emoji is two UTF-16 units and is left out rather than split", got)
	}
	if jsLen("ab😀c") != 5 {
		t.Fatal("UTF-16 length")
	}
}

func multipartBody(boundary string, file []byte) []byte {
	var b bytes.Buffer
	b.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"title\"\r\n\r\nHoliday\r\n")
	b.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"password\"\r\n\r\nupload-secret\r\n")
	b.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"avatar\"; filename=\"me.png\"\r\nContent-Type: image/png\r\n\r\n")
	b.Write(file)
	b.WriteString("\r\n--" + boundary + "\r\nContent-Disposition: form-data; name=\"tag\"\r\n\r\na\r\n")
	b.WriteString("--" + boundary + "\r\nContent-Disposition: form-data; name=\"tag\"\r\n\r\nb\r\n")
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes()
}

func TestMultipartSummaryAnyChunking(t *testing.T) {
	body := multipartBody("zb42", bytes.Repeat([]byte{1}, 100*1024))
	want := `{"title":"Holiday","password":"upload-secret","avatar":"[file me.png · image/png · 100 KB]","tag":["a","b"]}`
	for _, size := range []int{1, 7, 64, 16384, len(body)} {
		m := newMultipartSummary("zb42", 100)
		for i := 0; i < len(body); i += size {
			m.push(body[i:min(i+size, len(body))])
		}
		got, cut := m.result()
		if got != want || cut {
			t.Fatalf("chunk %d:\n got %s (cut %v)\nwant %s", size, got, cut, want)
		}
	}
}

func TestMultipartSummaryStoppedInsideAFile(t *testing.T) {
	body := multipartBody("zb42", bytes.Repeat([]byte{1}, 5000))
	m := newMultipartSummary("zb42", 100)
	m.push(body[:2000])
	got, cut := m.result()
	if !cut || !strings.Contains(got, `"avatar":"[file me.png · image/png · 2 KB+]"`) {
		t.Fatalf("got %s (cut %v)", got, cut)
	}
}

func TestTapDescribesAndInflates(t *testing.T) {
	s := bodySettingsFrom(&CaptureBodies{Mode: BodiesAll})
	bin := newTap(s, func([]byte) string { return "image/png" })
	bin.add(make([]byte, 3000))
	if got := attrMap(bin.attributes("response", "image/png", "")); got[attrResponseBody] != "[binary · image/png · 3 KB]" {
		t.Fatalf("binary: %v", got)
	}

	var gz bytes.Buffer
	w := gzipWriter(&gz)
	_, _ = w.Write([]byte(`{"error":"declined","cardNumber":"4242424242424242"}`))
	_ = w.Close()
	zipped := newTap(s, func([]byte) string { return "application/json" })
	zipped.add(gz.Bytes())
	got := attrMap(zipped.attributes("response", "application/json", "gzip"))
	if got[attrResponseBody] != `{"error":"declined","cardNumber":"[Filtered]"}` {
		t.Fatalf("gzip: %v", got)
	}

	br := newTap(s, func([]byte) string { return "application/json" })
	br.add([]byte{0x1b, 0x2, 0x3})
	if got := attrMap(br.attributes("response", "application/json", "br")); got[attrResponseBody] != "[binary · application/json · 3 B]" {
		t.Fatalf("brotli is described: %v", got)
	}
}
