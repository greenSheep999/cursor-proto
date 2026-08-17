package kernel

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// unwrapOK asserts that the envelope reports OK=true and returns the
// decoded result payload.
func unwrapOK(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var out map[string]any
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return out
}

func TestDispatch_ModelForAuthUsesLiveModelsAndProxy(t *testing.T) {
	previous := listModelsForAuth
	defer func() { listModelsForAuth = previous }()

	listModelsForAuth = func(acc *auth.Account) ([]string, error) {
		if acc.ProxyURL != "socks5://proxy.example:1080" {
			t.Fatalf("ProxyURL = %q", acc.ProxyURL)
		}
		return []string{"composer-live", "claude-live"}, nil
	}

	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "tok",
			Email:       "models@example.com",
		},
		ProxyURL: "socks5://proxy.example:1080",
	}
	storage, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(authModelRequest{
		AuthID:       "models@example.com",
		AuthProvider: "cursor",
		StorageJSON:  storage,
	})
	if err != nil {
		t.Fatal(err)
	}

	raw, rc := dispatch("model.for_auth", payload)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	result := unwrapOK(t, raw)
	models := result["Models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models = %v", models)
	}
	if models[0].(map[string]any)["ID"] != "composer-live" {
		t.Fatalf("first model = %v", models[0])
	}
}

func TestDispatch_Register(t *testing.T) {
	raw, rc := dispatch("plugin.register", nil)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	m := unwrapOK(t, raw)
	if got := m["metadata"].(map[string]any)["Name"]; got != "cursor" {
		t.Errorf("metadata.Name = %v, want cursor", got)
	}
	caps := m["capabilities"].(map[string]any)
	for _, key := range []string{"auth_provider", "executor", "model_provider"} {
		if v, ok := caps[key].(bool); !ok || !v {
			t.Errorf("capability %s not advertised: %v", key, caps[key])
		}
	}
}

func TestDispatch_AuthIdentifier(t *testing.T) {
	raw, rc := dispatch("auth.identifier", nil)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	m := unwrapOK(t, raw)
	if m["identifier"] != "cursor" {
		t.Errorf("identifier = %v, want cursor", m["identifier"])
	}
}

func TestDispatch_ModelStatic(t *testing.T) {
	raw, rc := dispatch("model.static", nil)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	m := unwrapOK(t, raw)
	if m["Provider"] != "cursor" {
		t.Errorf("Provider = %v, want cursor", m["Provider"])
	}
	models, ok := m["Models"].([]any)
	if !ok || len(models) == 0 {
		t.Fatalf("Models missing or empty: %v", m["Models"])
	}
	seen := make(map[string]bool, len(models))
	for _, rawModel := range models {
		id := rawModel.(map[string]any)["ID"].(string)
		seen[id] = true
		if strings.Contains(id, "-thinking-") || strings.HasSuffix(id, "-fast") {
			t.Fatalf("variant model %q must not be statically advertised", id)
		}
	}
	for _, id := range []string{"claude-opus-5", "claude-opus-4-8", "claude-fable-5", "claude-sonnet-5"} {
		if !seen[id] {
			t.Errorf("required Cursor 3.16 base model %q is missing", id)
		}
	}
}

func TestDispatch_RegisterReportsCurrentPluginVersion(t *testing.T) {
	raw, rc := dispatch("plugin.register", nil)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	m := unwrapOK(t, raw)
	if got := m["metadata"].(map[string]any)["Version"]; got != "0.8.0" {
		t.Fatalf("metadata.Version = %v, want 0.8.0", got)
	}
}

func TestDispatch_AuthParse_OK(t *testing.T) {
	// Build a valid CPA-shape auth JSON.
	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "tok",
			Email:       "unit@example.com",
		},
	}
	rawFile, err := file.Marshal()
	if err != nil {
		t.Fatalf("marshal auth file: %v", err)
	}
	reqBuf, err := json.Marshal(authParseRequest{
		Provider: "cursor",
		Path:     "/tmp/cursor-unit_at_example.com.json",
		FileName: "cursor-unit_at_example.com.json",
		RawJSON:  rawFile,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	raw, rc := dispatch("auth.parse", reqBuf)
	if rc != 0 {
		t.Fatalf("rc = %d, envelope=%s", rc, string(raw))
	}
	m := unwrapOK(t, raw)
	if v, ok := m["Handled"].(bool); !ok || !v {
		t.Fatalf("Handled = %v", m["Handled"])
	}
	auth, ok := m["Auth"].(map[string]any)
	if !ok {
		t.Fatalf("Auth missing")
	}
	if auth["Provider"] != "cursor" {
		t.Errorf("Provider = %v", auth["Provider"])
	}
	if auth["Label"] != "unit@example.com" {
		t.Errorf("Label = %v", auth["Label"])
	}
}

func TestDispatch_AuthParse_WrongType(t *testing.T) {
	// A JSON with type != cursor should return Handled=false.
	body := []byte(`{"type":"claude","access_token":"x","email":"e@example.com"}`)
	reqBuf, _ := json.Marshal(authParseRequest{
		Provider: "claude",
		Path:     "/tmp/claude.json",
		RawJSON:  body,
	})
	raw, rc := dispatch("auth.parse", reqBuf)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0", rc)
	}
	m := unwrapOK(t, raw)
	if v, ok := m["Handled"].(bool); !ok || v {
		t.Errorf("Handled should be false for non-cursor auth, got %v", m["Handled"])
	}
}

func TestDispatch_AuthRefresh_Passthrough(t *testing.T) {
	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:         cpaformat.ProviderType,
			AccessToken:  "at",
			RefreshToken: "rt",
			Email:        "ref@example.com",
		},
	}
	storage, err := file.Marshal()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	reqBuf, _ := json.Marshal(authRefreshRequest{
		AuthID:       "ref@example.com",
		AuthProvider: "cursor",
		StorageJSON:  storage,
	})
	raw, rc := dispatch("auth.refresh", reqBuf)
	if rc != 0 {
		t.Fatalf("rc = %d, envelope=%s", rc, string(raw))
	}
	m := unwrapOK(t, raw)
	auth, ok := m["Auth"].(map[string]any)
	if !ok {
		t.Fatalf("Auth missing")
	}
	if auth["Provider"] != "cursor" {
		t.Errorf("Provider = %v", auth["Provider"])
	}
	// StorageJSON is base64-encoded on the wire.
	if _, ok := auth["StorageJSON"].(string); !ok {
		t.Errorf("StorageJSON not present: %v", auth["StorageJSON"])
	}
}

func TestDispatch_ExecutorExecuteStream_BadRequest(t *testing.T) {
	// Empty payload can't be decoded as JSON, so we expect a
	// structured bad_request envelope (rc=0 with OK=false).
	raw, rc := dispatch("executor.execute_stream", nil)
	if rc != 0 {
		t.Fatalf("expected rc=0 for structured error, got %d", rc)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK {
		t.Fatal("expected OK=false for empty payload")
	}
	if env.Error == nil || env.Error.Code != "bad_request" {
		t.Errorf("unexpected error: %+v", env.Error)
	}
}

func TestDispatch_UnknownMethod(t *testing.T) {
	raw, rc := dispatch("bogus.method", nil)
	if rc == 0 {
		t.Fatal("expected non-zero rc for unknown method")
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK {
		t.Fatal("expected OK=false")
	}
	if env.Error == nil || env.Error.Code != "unknown_method" {
		t.Errorf("unexpected error: %+v", env.Error)
	}
}
