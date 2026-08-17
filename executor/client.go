// Package executor implements the HTTP layer of the Cursor 3.10 client:
// unary requests (AvailableModels, GetDefaultModel), the RunSSE stream, and
// BidiAppend result posting.
package executor

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor/transport"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
	"google.golang.org/protobuf/proto"
)

const (
	// api2 hosts unary calls (AvailableModels, GetDefaultModel, FileSync, ...).
	DefaultAPI2 = "https://api2.cursor.sh"
	// api3 hosts the streaming chat/agent endpoints.
	// TLS on api3 is pinned by the Electron main process — direct connections
	// from Go work fine, but mitmproxy will not intercept them.
	DefaultAPI3 = "https://api3.cursor.sh"
)

// Client bundles an authenticated Cursor session.
//
// Account is loaded once at NewClient time, but can be hot-swapped by
// setting AccountReloader — the executor will call it before every
// upstream call and use the returned account if it's non-nil, otherwise
// keep the existing one. The intended use case is "the user switched
// accounts in the Cursor IDE while cursor-proxy is running" — see
// cmd/cursor-proxy/main.go for the sqlite-mtime-check reloader that
// wires this up.
type Client struct {
	accountMu sync.Mutex
	// Account is the current authenticated identity. Read via CurrentAccount()
	// so any reloader is given a chance to run first.
	Account *auth.Account
	// AccountReloader, if non-nil, is invoked before every upstream call.
	// A non-nil return replaces Account; a nil return keeps it. Cost of
	// the callback should be near-zero when nothing changed (e.g. mtime
	// stat + short-circuit).
	AccountReloader func() *auth.Account
	API2            string // override for api2 host
	API3            string // override for api3 host

	// HTTPVersion pins the upstream protocol version (auto | http1.1 |
	// http1.0). Set at NewClient time via WithHTTPVersion. The
	// Transport this drives is baked into HTTP and is also used by
	// NewStreamClient() for SSE call sites (which need timeout=0).
	HTTPVersion transport.Version
	ProxyURL    string
	HTTP        *http.Client

	// sidecarToken authenticates requests to the optional loopback Chromium
	// sidecar. It is never set on native Cursor requests.
	sidecarToken         string
	sidecarUpstreamProxy string

	modelCatalogMu       sync.RWMutex
	modelCatalog         *cursorpb.AiserverV1_AvailableModelsResponse
	modelCatalogIdentity string
	modelCatalogAt       time.Time
}

// Option configures a Client at construction time. Use these instead of
// mutating fields post-construct so the derived HTTP client stays
// consistent with the settings.
type Option func(*Client)

// WithHTTPVersion pins the upstream HTTP protocol version. Default is
// transport.Auto (Go negotiates h2 via ALPN). Downgrade this when a
// corporate proxy / VPN mangles h2 SSE streams.
func WithHTTPVersion(v transport.Version) Option {
	return func(c *Client) { c.HTTPVersion = v }
}

// WithProxyURL routes all Cursor upstream calls through the supplied proxy.
// Supported schemes are http, https, socks5, and socks5h.
func WithProxyURL(proxyURL string) Option {
	return func(c *Client) { c.ProxyURL = proxyURL }
}

// NewClient wires up defaults and applies opts. The HTTP client is
// built from the resulting HTTPVersion, so callers pass options
// FIRST — post-construct mutation of HTTPVersion does not rebuild
// the client (by design; a mid-run wire-protocol change would leave
// existing SSE streams in an inconsistent state).
func NewClient(acc *auth.Account, opts ...Option) *Client {
	acc.FillSessionDefaults(time.Now())
	c := &Client{
		Account:     acc,
		API2:        DefaultAPI2,
		API3:        DefaultAPI3,
		HTTPVersion: transport.Auto,
		ProxyURL:    acc.ProxyURL,
	}
	for _, o := range opts {
		o(c)
	}
	c.HTTP = transport.ClientWithProxy(c.HTTPVersion, c.ProxyURL, 30*time.Second)
	return c
}

// NewStreamClient returns an *http.Client with the caller's chosen
// HTTPVersion and no timeout (SSE streams live for the duration of a
// generation, which may be minutes). Every SSE call site should build
// its client through this helper so `-http-version` reaches the wire.
func (c *Client) NewStreamClient() *http.Client {
	return transport.ClientWithProxy(c.HTTPVersion, c.ProxyURL, 0)
}

// NewUnaryClient returns an *http.Client with the caller's chosen
// HTTPVersion and a bounded timeout for one-shot RPCs. Kept separate
// from HTTP so callers with a different timeout requirement (e.g.
// count_tokens, which is heavier) can construct their own without
// stomping the shared client.
func (c *Client) NewUnaryClient(timeout time.Duration) *http.Client {
	return transport.ClientWithProxy(c.HTTPVersion, c.ProxyURL, timeout)
}

// CurrentAccount returns the account after giving the AccountReloader a
// chance to run. Every request path that talks to Cursor upstream should
// prefer this over the raw Account field so IDE-side account switches
// take effect without a proxy restart.
func (c *Client) CurrentAccount() *auth.Account {
	c.accountMu.Lock()
	defer c.accountMu.Unlock()
	if c.AccountReloader != nil {
		if fresh := c.AccountReloader(); fresh != nil {
			c.Account = fresh
		}
	}
	return c.Account
}

// UnaryCall performs a Connect unary RPC: sends `msg` as raw proto to
// <api2>/<service>/<method>, waits for the response, decompresses gzip,
// and unmarshals into `into`.
//
// This is the exact request shape we validated end-to-end for AvailableModels
// on 2026-07-09 (see cmd/test-connect).
func (c *Client) UnaryCall(service, method string, msg, into proto.Message) error {
	var reqBody []byte
	if msg != nil {
		b, err := proto.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		reqBody = b
	}

	url := fmt.Sprintf("%s/%s/%s", c.API2, service, method)
	req, err := http.NewRequest("POST", url, bytes.NewReader(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/proto")
	ApplyCommonHeaders(req, c.CurrentAccount(), auth.GenerateRequestID())
	c.applySidecarToken(req)

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("http do: %w", err)
	}
	defer resp.Body.Close()

	body, err := readBody(resp)
	if err != nil {
		return fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("http %d: %s", resp.StatusCode, string(body))
	}
	if into != nil && len(body) > 0 {
		if err := proto.Unmarshal(body, into); err != nil {
			return fmt.Errorf("unmarshal response: %w (body=%d bytes)", err, len(body))
		}
	}
	return nil
}

// readBody reads the response body and gunzips it if needed.
func readBody(resp *http.Response) ([]byte, error) {
	const maxCompressedBody = 32 << 20
	const maxDecompressedBody = 64 << 20
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxCompressedBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > maxCompressedBody {
		return nil, fmt.Errorf("response body exceeds %d bytes", maxCompressedBody)
	}
	if resp.Header.Get("Content-Encoding") == "gzip" ||
		(len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b) {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, err
		}
		defer gz.Close()
		decoded, err := io.ReadAll(io.LimitReader(gz, maxDecompressedBody+1))
		if err != nil {
			return nil, err
		}
		if int64(len(decoded)) > maxDecompressedBody {
			return nil, fmt.Errorf("decompressed response body exceeds %d bytes", maxDecompressedBody)
		}
		return decoded, nil
	}
	return raw, nil
}

// addConnectEnvelope prepends the 5-byte Connect protocol frame header:
// [flags:1][length:4 BE].
func addConnectEnvelope(data []byte, compressed bool) []byte {
	frame := make([]byte, 5+len(data))
	if compressed {
		frame[0] = 1
	}
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(data)))
	copy(frame[5:], data)
	return frame
}

// splitConnectFrame reads one Connect frame from `buf`. Returns the frame's
// payload, remaining bytes after the frame, and whether a full frame was
// present. Both gRPC-web trailers (0x80) and Connect end-stream frames (0x02)
// terminate the stream and must be parsed as status rather than protobuf data.
func splitConnectFrame(buf []byte) (payload []byte, isTrailer bool, rest []byte, ok bool) {
	if len(buf) < 5 {
		return nil, false, buf, false
	}
	flags := buf[0]
	length := binary.BigEndian.Uint32(buf[1:5])
	if uint32(len(buf)-5) < length {
		return nil, false, buf, false
	}
	end := 5 + int(length)
	payload = buf[5:end]
	rest = buf[end:]
	isTrailer = (flags&0x80) != 0 || (flags&0x02) != 0
	return payload, isTrailer, rest, true
}
