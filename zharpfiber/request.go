package zharpfiber

// The request as net/http sees it, and its OpenTelemetry attributes. Kept in step with the same
// code in zharpfasthttp (Fiber runs on fasthttp); the modules don't import each other.

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/valyala/fasthttp"
	"go.opentelemetry.io/otel/attribute"
)

// httpRequest is the request as net/http would see it, for the propagator, the header and body
// capture. Every value is copied: fasthttp reuses its buffers.
func httpRequest(rc *fasthttp.RequestCtx) *http.Request {
	header := make(http.Header)
	rc.Request.Header.VisitAll(func(k, v []byte) {
		header.Add(string(k), string(v))
	})
	u := &url.URL{Path: string(rc.Path()), RawQuery: string(rc.URI().QueryString())}
	req := &http.Request{
		Method:     string(rc.Method()),
		URL:        u,
		Proto:      string(rc.Request.Header.Protocol()),
		Header:     header,
		Host:       string(rc.Host()),
		RemoteAddr: rc.RemoteAddr().String(),
		RequestURI: string(rc.RequestURI()),
	}
	if rc.IsTLS() {
		req.TLS = rc.TLSConnectionState()
	}
	return req
}

func responseHeader(h *fasthttp.ResponseHeader) http.Header {
	header := make(http.Header)
	h.VisitAll(func(k, v []byte) {
		header.Add(string(k), string(v))
	})
	return header
}

// requestAttributes are the HTTP server attributes OpenTelemetry's net/http instrumentation sets.
func requestAttributes(rc *fasthttp.RequestCtx, req *http.Request) []attribute.KeyValue {
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	attrs := []attribute.KeyValue{
		attribute.String("http.request.method", req.Method),
		attribute.String("url.scheme", scheme),
		attribute.String("url.path", req.URL.Path),
	}
	if host, port := splitHostPort(req.Host); host != "" {
		attrs = append(attrs, attribute.String("server.address", host))
		if port > 0 {
			attrs = append(attrs, attribute.Int("server.port", port))
		}
	}
	if req.URL.RawQuery != "" {
		attrs = append(attrs, attribute.String("url.query", req.URL.RawQuery))
	}
	peer, peerPort := splitHostPort(req.RemoteAddr)
	if peer != "" {
		attrs = append(attrs, attribute.String("network.peer.address", peer))
		if peerPort > 0 {
			attrs = append(attrs, attribute.Int("network.peer.port", peerPort))
		}
	}
	client := peer
	if xff := req.Header.Get("X-Forwarded-For"); xff != "" {
		client = strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	if client != "" {
		attrs = append(attrs, attribute.String("client.address", client))
	}
	if ua := req.Header.Get("User-Agent"); ua != "" {
		attrs = append(attrs, attribute.String("user_agent.original", ua))
	}
	if v := strings.TrimPrefix(req.Proto, "HTTP/"); v != "" && v != req.Proto {
		attrs = append(attrs, attribute.String("network.protocol.version", v))
	}
	return attrs
}

func splitHostPort(hostport string) (string, int) {
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		return hostport, -1
	}
	p, err := strconv.Atoi(port)
	if err != nil {
		return host, -1
	}
	return host, p
}
