// Package transport supplies an http.Transport tuned for a specific
// upstream HTTP protocol version. cursor-proxy uses this in place of
// nil-Transport http.Clients so the operator's -http-version choice
// reaches every upstream call.
//
// Motivation: Go's http.DefaultTransport ALPN-negotiates h2 against
// modern TLS servers (which Cloudflare/api2.cursor.sh is). Some
// corporate proxies, TLS-inspecting VPNs, and older SSL-terminating
// appliances mangle or truncate HTTP/2 streams — the middlebox re-
// frames TLS records and either drops frames or breaks stream
// interleaving, so SSE responses come back partial with an
// "unexpected EOF" a few seconds in. HTTP/1.1 (or 1.0) sidesteps this
// because there's no stream multiplexing to break.
//
// Contract: this package returns *http.Transport values that can be
// dropped into any http.Client. It does not touch the request path
// (URL, headers, method) — only the wire negotiation and, for the
// 1.0 case, the per-request Proto downgrade. Nothing here is Cursor-
// specific.
package transport

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Version enumerates the wire protocols cursor-proxy can force.
type Version int

const (
	// Auto lets Go negotiate — HTTP/2 in practice on modern TLS servers,
	// falling back to HTTP/1.1 if the server disables h2. This is the
	// default and what users on healthy networks should use.
	Auto Version = iota

	// Http1_1 disables h2 ALPN so TLS negotiates http/1.1 for the
	// duration of the connection. Fixes the corporate-proxy / VPN
	// truncation class of bugs. Slightly higher per-request overhead
	// than h2 because there's no stream multiplexing, but SSE / long
	// generations become reliable on restrictive networks.
	Http1_1

	// Http1_0 disables h2 AND disables keepalive, so every request
	// closes the TCP connection after the response. Only useful for
	// the most broken middleboxes that mangle even HTTP/1.1
	// chunked-transfer or keep-alive semantics.
	//
	// Caveat about the request-line proto: Go's http.Transport
	// unconditionally writes "HTTP/1.1" on the wire for TCP
	// connections — Request.ProtoMajor/Minor is not respected by
	// the built-in transport. HTTP1_0RoundTripper sets req.Proto so
	// application-level middleware sees "HTTP/1.0", but a network
	// packet capture will still show "HTTP/1.1". The real behavioral
	// difference for restrictive middleboxes comes from
	// DisableKeepAlives (each request forces "Connection: close" and
	// a fresh TCP+TLS handshake), which IS respected on the wire.
	//
	// Every request pays a fresh TLS handshake — expect noticeable
	// latency; not a default anyone should pick without evidence.
	Http1_0
)

// String is stable and matches the CLI flag values / env values that
// map back to each Version (see Parse). Kept short so it can be shown
// on `/v1/proxy-info` and in operator logs without wrapping.
func (v Version) String() string {
	switch v {
	case Http1_1:
		return "http1.1"
	case Http1_0:
		return "http1.0"
	default:
		return "auto"
	}
}

// Parse turns an operator-supplied string ("auto" / "http1.1" /
// "http1.0" / a couple of common aliases) into a Version. Returns an
// error on any other input so bad CLI flag values surface loudly
// instead of silently falling through to Auto.
func Parse(s string) (Version, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "", "auto", "h2", "http2", "http/2":
		return Auto, nil
	case "http1.1", "http/1.1", "1.1", "h1", "http1":
		return Http1_1, nil
	case "http1.0", "http/1.0", "1.0":
		return Http1_0, nil
	}
	return Auto, fmt.Errorf("unrecognized http version %q (want auto|http1.1|http1.0)", s)
}

// New returns an *http.Transport configured for the requested wire
// version. The returned Transport is a fresh clone of
// http.DefaultTransport so proxy env vars (HTTPS_PROXY, HTTP_PROXY,
// NO_PROXY) still work — we only mutate the h2 negotiation and
// keepalive knobs.
func New(v Version) *http.Transport {
	base := http.DefaultTransport.(*http.Transport).Clone()
	switch v {
	case Http1_1, Http1_0:
		// Disable h2 in two places, matching Go's own upgrade path:
		//   - ForceAttemptHTTP2=false stops the transport from
		//     opportunistically upgrading a cleartext connection.
		//   - TLSNextProto={} disables the ALPN "h2" registration
		//     that the h2_bundle.go init() sets up.
		base.ForceAttemptHTTP2 = false
		base.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
		if v == Http1_0 {
			// HTTP/1.0 doesn't keep-alive by default. Turning
			// keepalive off in the Transport too is belt-and-braces —
			// some servers accept the 1.0 proto but keep the
			// connection open when Transport says it wants to; we
			// don't want that half-alive state on restrictive
			// networks.
			base.DisableKeepAlives = true
		}
	}
	return base
}

// HTTP1_0RoundTripper wraps a Transport and downgrades every request's
// Proto to HTTP/1.0 before it hits the wire. Only used when
// Version == Http1_0; the underlying Transport is expected to have
// been produced by New(Http1_0).
type HTTP1_0RoundTripper struct{ Base http.RoundTripper }

func (r HTTP1_0RoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	// Copy so we don't mutate a caller's request in place — RoundTrip
	// contract says the input request must be unchanged.
	req2 := req.Clone(req.Context())
	req2.Proto = "HTTP/1.0"
	req2.ProtoMajor = 1
	req2.ProtoMinor = 0
	return r.Base.RoundTrip(req2)
}

// Client builds an *http.Client with the right Transport + optional
// HTTP/1.0 wrapper. Callers pass their own timeout — cursor-proxy
// uses 30s for one-shot RPCs and 0 (no timeout) for SSE streams.
func Client(v Version, timeout time.Duration) *http.Client {
	var rt http.RoundTripper = New(v)
	if v == Http1_0 {
		rt = HTTP1_0RoundTripper{Base: rt}
	}
	return &http.Client{Transport: rt, Timeout: timeout}
}
