package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestServeLoginPage confirms the embedded HTML panel is served
// verbatim under the resource path CPA sends it to. The important
// bits: 200 status, HTML content-type, no-store caching, and body
// contains the panel headline ("Add Cursor account") so a broken
// //go:embed doesn't silently ship an empty file.
func TestServeLoginPage(t *testing.T) {
	resp := routeManagement(context.Background(), managementRequest{
		Method: http.MethodGet,
		// This is exactly what CPA sends for a Menu-tagged GET route.
		Path: "/v0/resource/plugins/cursor/cli-proxy-api/cursor/login",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	ct := headerFirst(resp.Headers, "Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		t.Fatalf("Content-Type = %q, want text/html*", ct)
	}
	if headerFirst(resp.Headers, "Cache-Control") == "" {
		t.Fatal("Cache-Control must be set (no-store) to prevent stale panel JS after a plugin rebuild")
	}
	if !bytes.Contains(resp.Body, []byte("Add Cursor account")) {
		t.Fatalf("body missing panel headline; got %d bytes starting with %q",
			len(resp.Body), truncatePreview(resp.Body, 120))
	}
	// Sanity: JS should be reaching for the management-namespaced API,
	// not the resource-namespaced one where the panel itself lives.
	if !bytes.Contains(resp.Body, []byte("/v0/management/cli-proxy-api/cursor/login/start")) {
		t.Errorf("panel JS should call /login/start under /v0/management, not the resource path")
	}
}

// TestLoginStart_Passthrough_Errors verifies the /login/start route
// forwards bad requests all the way through to handleAuthLoginStart
// and surfaces its error payload on the HTTP layer. We give it a
// bogus mode; handleAuthLoginStart returns a `bad_request` envelope.
func TestLoginStart_Passthrough_Errors(t *testing.T) {
	body := mustJSON(t, map[string]any{
		"Provider": "cursor",
		"Metadata": map[string]any{"mode": "not-a-mode"},
	})
	resp := routeManagement(context.Background(), managementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/cli-proxy-api/cursor/login/start",
		Body:   body,
	})
	if resp.StatusCode < 400 {
		t.Fatalf("StatusCode = %d, want 4xx/5xx for bogus mode", resp.StatusCode)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, string(resp.Body))
	}
	if _, ok := out["error"]; !ok {
		t.Fatalf("response missing 'error' key: %s", string(resp.Body))
	}
}

// TestLoginPoll_Passthrough_MissingState verifies the /login/poll
// route surfaces the ABI-level "State required" error as an HTTP
// error the panel JS can render.
func TestLoginPoll_Passthrough_MissingState(t *testing.T) {
	body := mustJSON(t, map[string]any{"Provider": "cursor"})
	resp := routeManagement(context.Background(), managementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/cli-proxy-api/cursor/login/poll",
		Body:   body,
	})
	if resp.StatusCode < 400 {
		t.Fatalf("StatusCode = %d, want 4xx for missing State", resp.StatusCode)
	}
	if !bytes.Contains(resp.Body, []byte("State")) {
		t.Errorf("response body should mention missing State field: %s", string(resp.Body))
	}
}

// TestLoginStart_EmptyBody guards against a panel bug (or a caller
// with no body) — /login/start with an empty body should 400 with a
// clear message, not crash or 200 with garbage.
func TestLoginStart_EmptyBody(t *testing.T) {
	resp := routeManagement(context.Background(), managementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/cli-proxy-api/cursor/login/start",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400", resp.StatusCode)
	}
}

// mustJSON encodes v to JSON or fails the test — panel wire-format
// tests need this so often that inlining json.Marshal + err checks
// bloats every case.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	buf, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return buf
}

// truncatePreview returns at most n bytes of b, for a compact "body
// looked wrong" failure message. Non-printable bytes are left as-is
// because a truncated hex dump would only obscure the real problem.
func truncatePreview(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "..."
}

// headerFirst returns the first value of a raw map[string][]string
// header, or "" if the key is absent / empty. managementResponse
// stores headers as a raw map (not http.Header) so tests can't rely
// on the http.Header.Get() convenience method.
func headerFirst(h map[string][]string, key string) string {
	if h == nil {
		return ""
	}
	vals := h[key]
	if len(vals) == 0 {
		return ""
	}
	return vals[0]
}
