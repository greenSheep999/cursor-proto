package kernel

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

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

// withoutCatalogRetries removes the startup-window backoff so failure-path
// tests assert the outcome without waiting out the retry schedule.
func withoutCatalogRetries(t *testing.T) {
	t.Helper()
	previous := catalogRetrySchedule
	catalogRetrySchedule = nil
	t.Cleanup(func() { catalogRetrySchedule = previous })
}

func TestDispatch_ModelForAuthDoesNotAdvertiseStaticModelsWhenLiveCatalogFails(t *testing.T) {
	withoutCatalogRetries(t)
	previous := listModelsForAuth
	defer func() { listModelsForAuth = previous }()

	listModelsForAuth = func(*auth.Account) ([]string, error) {
		return nil, errors.New("account catalog unavailable")
	}

	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "tok",
			Email:       "catalog-failure@example.com",
		},
	}
	storage, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(authModelRequest{
		AuthID:       "catalog-failure@example.com",
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
	models, ok := result["Models"].([]any)
	if !ok {
		t.Fatalf("Models has unexpected shape: %T", result["Models"])
	}
	if len(models) != 0 {
		t.Fatalf("failed account advertised %d models; want zero so CPA removes it from model shards", len(models))
	}
}

// forAuthPayload builds a model.for_auth request for one account id.
func forAuthPayload(t *testing.T, authID string) []byte {
	t.Helper()
	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "tok",
			Email:       authID,
		},
	}
	storage, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(authModelRequest{
		AuthID:       authID,
		AuthProvider: "cursor",
		StorageJSON:  storage,
	})
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func forAuthModelIDs(t *testing.T, payload []byte) []string {
	t.Helper()
	raw, rc := dispatch("model.for_auth", payload)
	if rc != 0 {
		t.Fatalf("rc = %d", rc)
	}
	models, ok := unwrapOK(t, raw)["Models"].([]any)
	if !ok {
		t.Fatal("Models has unexpected shape")
	}
	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.(map[string]any)["ID"].(string))
	}
	return ids
}

// The Chromium sidecar joins CPA's network namespace, so AvailableModels is
// refused for the first moments after a restart. Without a retry CPA registers
// an empty shard and every request fails with "unknown provider".
func TestDispatch_ModelForAuthRetriesThroughStartupWindow(t *testing.T) {
	previousSchedule := catalogRetrySchedule
	catalogRetrySchedule = []time.Duration{time.Millisecond, time.Millisecond}
	defer func() { catalogRetrySchedule = previousSchedule }()

	previous := listModelsForAuth
	defer func() { listModelsForAuth = previous }()

	attempts := 0
	listModelsForAuth = func(*auth.Account) ([]string, error) {
		attempts++
		if attempts < 3 {
			return nil, errors.New("dial tcp 127.0.0.1:18901: connect: connection refused")
		}
		return []string{"claude-opus-4-8"}, nil
	}

	ids := forAuthModelIDs(t, forAuthPayload(t, "startup-window@example.com"))
	if len(ids) != 1 || ids[0] != "claude-opus-4-8" {
		t.Fatalf("models = %v, want the catalog from the successful retry", ids)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want the call retried until it succeeded", attempts)
	}
}

// A blip after startup must not remove a healthy account from routing.
func TestDispatch_ModelForAuthFallsBackToLastKnownCatalog(t *testing.T) {
	withoutCatalogRetries(t)
	previous := listModelsForAuth
	defer func() { listModelsForAuth = previous }()

	payload := forAuthPayload(t, "cached-catalog@example.com")

	listModelsForAuth = func(*auth.Account) ([]string, error) {
		return []string{"claude-opus-4-8", "claude-sonnet-5"}, nil
	}
	if ids := forAuthModelIDs(t, payload); len(ids) != 2 {
		t.Fatalf("warm-up returned %v", ids)
	}

	listModelsForAuth = func(*auth.Account) ([]string, error) {
		return nil, errors.New("sidecar restarting")
	}
	ids := forAuthModelIDs(t, payload)
	if len(ids) != 2 || ids[0] != "claude-opus-4-8" {
		t.Fatalf("models = %v, want the last known good catalog", ids)
	}
}

func TestDispatch_ModelForAuthDoesNotAdvertiseStaticModelsForEmptyLiveCatalog(t *testing.T) {
	withoutCatalogRetries(t)
	previous := listModelsForAuth
	defer func() { listModelsForAuth = previous }()

	listModelsForAuth = func(*auth.Account) ([]string, error) { return nil, nil }

	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "tok",
			Email:       "empty-catalog@example.com",
		},
	}
	storage, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(authModelRequest{
		AuthID:       "empty-catalog@example.com",
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
	models, ok := result["Models"].([]any)
	if !ok {
		t.Fatalf("Models has unexpected shape: %T", result["Models"])
	}
	if len(models) != 0 {
		t.Fatalf("empty-catalog account advertised %d models; want zero", len(models))
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
	}
	for _, id := range []string{
		"claude-opus-5",
		"claude-opus-5-medium",
		"claude-opus-5-thinking-high",
		"claude-opus-4-8",
		"claude-fable-5",
		"claude-sonnet-5",
	} {
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
	if got := m["metadata"].(map[string]any)["Version"]; got != pluginVersion {
		t.Fatalf("metadata.Version = %v, want %s", got, pluginVersion)
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
