package executor

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"path"
	"strings"
)

const (
	sidecarTokenHeader = "x-cursor-chromium-sidecar-token"
	sidecarProxyHeader = "x-cursor-chromium-upstream-proxy"
)

// ChromiumSidecarOption routes every Cursor RPC through a loopback Chromium
// sidecar. Keeping this as one executor option ensures AvailableModels,
// RunSSE, and BidiAppend share the same transport identity.
func ChromiumSidecarOption(rawURL, token string) (Option, error) {
	base, err := validateChromiumSidecarURL(rawURL)
	if err != nil {
		return nil, err
	}
	return func(c *Client) {
		// Preserve the account/request proxy for the Chromium hop before
		// clearing it from Go's loopback transport. The sidecar applies this
		// route inside Chromium, where Cursor sees the browser network stack.
		c.sidecarUpstreamProxy = strings.TrimSpace(c.ProxyURL)
		c.API2 = joinSidecarPath(base, "api2")
		// Cursor 3.16 routes RunSSE on api2. The sidecar supports /api3 for
		// future protocol lines, but the current client must use the verified
		// api2 route for both halves of the chat protocol.
		c.API3 = joinSidecarPath(base, "api2")
		c.sidecarToken = strings.TrimSpace(token)
		// The Go hop is loopback and must never be sent through an account or
		// process-wide upstream proxy. Any egress tunnelling belongs on the
		// Chromium process so its server-facing TLS identity stays intact.
		c.ProxyURL = ""
	}, nil
}

func validateChromiumSidecarURL(rawURL string) (*url.URL, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil, fmt.Errorf("Chromium sidecar URL is empty")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse Chromium sidecar URL: %w", err)
	}
	if u.Scheme != "http" {
		return nil, fmt.Errorf("Chromium sidecar URL must use loopback HTTP, got %q", u.Scheme)
	}
	host := u.Hostname()
	if host == "" {
		return nil, fmt.Errorf("Chromium sidecar URL has no host")
	}
	if !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return nil, fmt.Errorf("Chromium sidecar must be loopback-only, got host %q", host)
		}
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

func joinSidecarPath(base *url.URL, segment string) string {
	copyURL := *base
	copyURL.Path = path.Join(base.Path, segment)
	return strings.TrimRight(copyURL.String(), "/")
}

func (c *Client) applySidecarToken(req *http.Request) {
	if c == nil {
		return
	}
	if token := strings.TrimSpace(c.sidecarToken); token != "" {
		req.Header.Set(sidecarTokenHeader, token)
	}
	if proxyURL := strings.TrimSpace(c.sidecarUpstreamProxy); proxyURL != "" {
		req.Header.Set(sidecarProxyHeader, proxyURL)
	}
}
