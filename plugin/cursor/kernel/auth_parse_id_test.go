package kernel

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// handleAuthParse MUST return the bare filename as the auth ID so it matches
// how CPA's ModelRegistry keys clients and how the panel's GetAuthFileModels
// lookup resolves them. Returning the container-absolute req.Path caused the
// registry to key the model catalog under one string (the absolute path) while
// the panel queried it under the filename — /v0/management/auth-files/models
// answered `{"models":[]}` even after live discovery had reported 665 models,
// which surfaced as "No available models for this credential" on the
// production dashboard.
//
// This test pins the ID = FileName invariant so a future refactor cannot
// silently reintroduce the mismatch.
func TestHandleAuthParseIDMatchesFileName(t *testing.T) {
	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "AT",
			Email:       "probe@example.com",
			UserID:      "user_probe",
		},
	}
	rawStorage, err := file.Marshal()
	if err != nil {
		t.Fatalf("marshal storage: %v", err)
	}
	reqBody, _ := json.Marshal(authParseRequest{
		Provider: pluginName,
		Path:     "/root/.cli-proxy-api/cursor-probe_at_example.com.json",
		FileName: "cursor-probe_at_example.com.json",
		RawJSON:  rawStorage,
	})
	raw, rc := handleAuthParse(reqBody)
	if rc != 0 {
		t.Fatalf("handleAuthParse rc=%d body=%s", rc, raw)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("not ok: %s", raw)
	}
	var resp authParseResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled {
		t.Fatal("expected Handled=true")
	}
	const wantFileName = "cursor-probe_at_example.com.json"
	if resp.Auth.FileName != wantFileName {
		t.Fatalf("Auth.FileName = %q, want %q", resp.Auth.FileName, wantFileName)
	}
	if resp.Auth.ID != wantFileName {
		t.Fatalf("Auth.ID = %q, want %q (must equal FileName so the panel and registry agree)", resp.Auth.ID, wantFileName)
	}
	if resp.Auth.ID == "/root/.cli-proxy-api/cursor-probe_at_example.com.json" {
		t.Fatal("Auth.ID regressed to req.Path (absolute path); this is the exact regression that produces the 'No available models' panel state")
	}
}

// A raw req.Path without a FileName still yields a filename-only ID via the
// filepath.Base fallback in handleAuthParse.
func TestHandleAuthParseIDFallsBackToPathBase(t *testing.T) {
	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "AT",
			Email:       "probe2@example.com",
			UserID:      "user_probe_2",
		},
	}
	rawStorage, _ := file.Marshal()
	reqBody, _ := json.Marshal(authParseRequest{
		Provider: pluginName,
		Path:     "/tmp/nested/dir/cursor-probe2_at_example.com.json",
		FileName: "",
		RawJSON:  rawStorage,
	})
	raw, rc := handleAuthParse(reqBody)
	if rc != 0 {
		t.Fatalf("handleAuthParse rc=%d body=%s", rc, raw)
	}
	var env struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp authParseResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	const want = "cursor-probe2_at_example.com.json"
	if resp.Auth.ID != want || resp.Auth.FileName != want {
		t.Fatalf("Auth.ID=%q Auth.FileName=%q; both should be %q", resp.Auth.ID, resp.Auth.FileName, want)
	}
}
