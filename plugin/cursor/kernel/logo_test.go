package kernel

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

const logoPrefix = "data:image/png;base64,"

// TestCursorLogoDataURI_NotEmpty asserts that the returned data URI is
// well-formed: it must carry the PNG data URI prefix and the payload
// after the prefix must be valid standard base64.
func TestCursorLogoDataURI_NotEmpty(t *testing.T) {
	uri := CursorLogoDataURI()
	if uri == "" {
		t.Fatalf("CursorLogoDataURI returned empty string; embed likely failed")
	}
	if !strings.HasPrefix(uri, logoPrefix) {
		t.Fatalf("CursorLogoDataURI missing PNG data URI prefix; got %q", uri[:min(len(uri), 64)])
	}
	payload := strings.TrimPrefix(uri, logoPrefix)
	if payload == "" {
		t.Fatalf("CursorLogoDataURI base64 payload is empty")
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("base64 decode failed: %v", err)
	}
	if len(decoded) < 8 || string(decoded[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("decoded payload is not a PNG (missing signature)")
	}
}

// TestCursorLogoDataURI_Cached asserts the sync.Once cache is honored:
// calling the accessor many times must return the same string value.
func TestCursorLogoDataURI_Cached(t *testing.T) {
	first := CursorLogoDataURI()
	if first == "" {
		t.Fatalf("first CursorLogoDataURI call returned empty")
	}
	for i := 0; i < 100; i++ {
		got := CursorLogoDataURI()
		if got != first {
			t.Fatalf("CursorLogoDataURI mismatch at call %d: cache not honored", i+2)
		}
	}
}

// TestRegisterResult_HasLogo confirms the register JSON exposes the
// embedded logo through metadata.Logo, so the CPA management UI has
// an image src to render.
func TestRegisterResult_HasLogo(t *testing.T) {
	raw := registerResult()
	if raw == "" {
		t.Fatalf("registerResult returned empty JSON")
	}
	var body struct {
		Metadata struct {
			Logo string `json:"Logo"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("unmarshal registerResult: %v", err)
	}
	if body.Metadata.Logo == "" {
		t.Fatalf("metadata.Logo is empty; expected data URI")
	}
	if !strings.HasPrefix(body.Metadata.Logo, "data:image/png") {
		t.Fatalf("metadata.Logo does not start with data:image/png prefix; got %q", body.Metadata.Logo[:min(len(body.Metadata.Logo), 64)])
	}
}
