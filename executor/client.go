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
	"time"

	"github.com/router-for-me/cursor-proto/auth"
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
	HTTP            *http.Client
}

// NewClient wires up defaults.
func NewClient(acc *auth.Account) *Client {
	acc.FillSessionDefaults(time.Now())
	return &Client{
		Account: acc,
		API2:    DefaultAPI2,
		API3:    DefaultAPI3,
		HTTP:    &http.Client{Timeout: 30 * time.Second},
	}
}

// CurrentAccount returns the account after giving the AccountReloader a
// chance to run. Every request path that talks to Cursor upstream should
// prefer this over the raw Account field so IDE-side account switches
// take effect without a proxy restart.
func (c *Client) CurrentAccount() *auth.Account {
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
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.Header.Get("Content-Encoding") == "gzip" ||
		(len(raw) >= 2 && raw[0] == 0x1f && raw[1] == 0x8b) {
		gz, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return raw, err
		}
		defer gz.Close()
		return io.ReadAll(gz)
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
// present. If flags & 0x80 != 0, it's a trailer frame (grpc-status/message).
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
	isTrailer = (flags & 0x80) != 0
	return payload, isTrailer, rest, true
}
