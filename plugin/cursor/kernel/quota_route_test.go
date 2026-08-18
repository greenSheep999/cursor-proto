package kernel

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

func TestMain(m *testing.M) {
	// Unit tests must not fan out to Cursor's usage API. Production keeps
	// fetchAccountQuotaLive; individual tests stub a quota when they care.
	fetchAccountQuota = func(*auth.Account) (accountQuota, bool) {
		return accountQuota{}, false
	}
	os.Exit(m.Run())
}

func stubQuota(t *testing.T, auto, api float64, ok bool) {
	t.Helper()
	previous := fetchAccountQuota
	resetQuotaCache()
	fetchAccountQuota = func(*auth.Account) (accountQuota, bool) {
		return accountQuota{Auto: auto, API: api}, ok
	}
	t.Cleanup(func() {
		fetchAccountQuota = previous
		resetQuotaCache()
	})
}

func TestNormalizeQuotaPercent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want float64
	}{
		{0.07, 0.07},
		{1.0, 1.0},
		{1.45, 1.45},
		{100, 1.0},
		{99.0, 0.99},
	}
	for _, tc := range cases {
		if got := normalizeQuotaPercent(tc.in); got != tc.want {
			t.Errorf("normalizeQuotaPercent(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestQuotaExhausted(t *testing.T) {
	t.Parallel()
	if quotaExhausted(0.98) {
		t.Fatal("0.98 auto must still be routable")
	}
	if !quotaExhausted(1.0) {
		t.Fatal("1.0 auto is exhausted")
	}
	if !quotaExhausted(100) {
		t.Fatal("live API=100 must count as exhausted")
	}
	if quotaExhausted(0) {
		t.Fatal("zero is not exhausted")
	}
}

func TestModelQuotaBucket(t *testing.T) {
	t.Parallel()
	cursor := []string{
		"composer-2.5",
		"composer-2.5-fast",
		"composer-2.5[fast=true]",
		"cursor-small",
		"cursor-grok-4.6-high-fast",
		"grok-4.6",
		"auto-smart",
		"default",
	}
	other := []string{
		"claude-sonnet-4-6",
		"claude-opus-5-medium",
		"cursor/claude-sonnet-4-6",
		"gpt-5.5",
		"gemini-3.6-flash",
		"kimi-k2.7-code",
		"glm-5.2",
	}
	for _, id := range cursor {
		if got := modelQuotaBucket(id); got != quotaBucketCursor {
			t.Errorf("modelQuotaBucket(%q) = %s, want cursor", id, got)
		}
	}
	for _, id := range other {
		if got := modelQuotaBucket(id); got != quotaBucketOther {
			t.Errorf("modelQuotaBucket(%q) = %s, want other", id, got)
		}
	}
}

func TestFilterModelsForQuotaDropsExhaustedOther(t *testing.T) {
	stubQuota(t, 0.07, 100, true)
	acc := &auth.Account{Email: "pool@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("pool@example.com", acc, []string{
		"composer-2.5", "grok-4.6", "claude-sonnet-4-6", "gpt-5.5",
	})
	if len(got) != 2 || got[0] != "composer-2.5" || got[1] != "grok-4.6" {
		t.Fatalf("got %v, want composer+grok only", got)
	}
}

func TestFilterModelsForQuotaDropsExhaustedCursor(t *testing.T) {
	stubQuota(t, 1.45, 0.2, true)
	acc := &auth.Account{Email: "over@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("over@example.com", acc, []string{
		"composer-2.5", "claude-sonnet-4-6",
	})
	if len(got) != 1 || got[0] != "claude-sonnet-4-6" {
		t.Fatalf("got %v, want claude only", got)
	}
}

func TestFilterModelsForQuotaFailsOpen(t *testing.T) {
	stubQuota(t, 0, 0, false)
	acc := &auth.Account{Email: "blip@example.com", AccessToken: "tok"}
	in := []string{"composer-2.5", "claude-sonnet-4-6"}
	got := filterModelsForQuota("blip@example.com", acc, in)
	if len(got) != 2 {
		t.Fatalf("fail-open filtered to %v", got)
	}
}

func TestDispatch_ModelForAuthHidesOtherWhenAPIExhausted(t *testing.T) {
	withoutCatalogRetries(t)
	stubQuota(t, 0.02, 100, true)
	previous := listModelsForAuth
	t.Cleanup(func() { listModelsForAuth = previous })
	listModelsForAuth = func(*auth.Account) ([]string, error) {
		return []string{"composer-2.5", "claude-sonnet-4-6", "gpt-5.5"}, nil
	}

	ids := forAuthModelIDs(t, forAuthPayload(t, "api-full@example.com"))
	if len(ids) != 1 || ids[0] != "composer-2.5" {
		t.Fatalf("models = %v, want only composer-2.5", ids)
	}
}

func TestExecuteRejectsOtherModelsWhenAPIQuotaExhausted(t *testing.T) {
	stubQuota(t, 0.04, 100, true)
	runnerCalled := false
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) {
			runnerCalled = true
			return &fakeRunner{}, "unit@example.com", nil
		},
		nil,
	)()

	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "AT",
			Email:       "api-full@example.com",
		},
	}
	storage, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	req := executorRequest{
		AuthID:       "api-full@example.com",
		AuthProvider: "cursor",
		Model:        "claude-sonnet-4-6",
		Format:       "claude",
		Payload:      []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON:  storage,
	}
	rawRequest, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, rc := dispatch("executor.execute", rawRequest)
	if rc != 0 {
		t.Fatalf("rc = %d envelope=%s", rc, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected quota skip, got %s", raw)
	}
	if env.Error.Code != "quota_exhausted" || !env.Error.Retryable || env.Error.HTTPStatus != 429 {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if runnerCalled {
		t.Fatal("exhausted other-models request reached upstream")
	}
}

func TestExecuteAllowsComposerWhenOnlyOtherQuotaExhausted(t *testing.T) {
	stubQuota(t, 0.04, 100, true)
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("ok"),
			buildTurnEndedEvent(4, 1),
		},
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) {
			return runner, "unit@example.com", nil
		},
		nil,
	)()

	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: "AT",
			Email:       "composer-ok@example.com",
		},
	}
	storage, err := file.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	req := executorRequest{
		AuthID:       "composer-ok@example.com",
		AuthProvider: "cursor",
		Model:        "composer-2.5",
		Format:       "claude",
		Payload:      []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON:  storage,
	}
	rawRequest, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, rc := dispatch("executor.execute", rawRequest)
	if rc != 0 {
		t.Fatalf("rc = %d envelope=%s", rc, raw)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK {
		t.Fatalf("composer should stay routable: %s", raw)
	}
}
