package kernel

import (
	"encoding/json"
	"os"
	"strings"
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

// stubQuota installs a canned accountQuota for the duration of a test.
// Tests pass the two orthogonal signals we now key off explicitly:
//
//   - auto: the AutoPercentUsed ratio (0-1 or 0-100; normalized inside).
//   - slowPool: matches snap.InSlowPool.
//   - noUsageBased: matches snap.NoUsageBasedAllowed.
func stubQuota(t *testing.T, auto float64, slowPool, noUsageBased, ok bool) {
	t.Helper()
	previous := fetchAccountQuota
	resetQuotaCache()
	fetchAccountQuota = func(*auth.Account) (accountQuota, bool) {
		return accountQuota{
			Auto:                auto,
			InSlowPool:          slowPool,
			NoUsageBasedAllowed: noUsageBased,
		}, ok
	}
	t.Cleanup(func() {
		fetchAccountQuota = previous
		resetQuotaCache()
	})
}

// stubQuotaBot stubs a healthy cursor/other pair plus an explicit Sand bar
// state, so bot-bucket gating can be exercised on its own. stubQuota leaves
// BotFetched false (bot gating disabled, fail open), which is what keeps the
// pre-Sand tests meaningful.
func stubQuotaBot(t *testing.T, botFetched, botUnlocked, botHasAvailable bool) {
	t.Helper()
	previous := fetchAccountQuota
	resetQuotaCache()
	fetchAccountQuota = func(*auth.Account) (accountQuota, bool) {
		return accountQuota{
			Auto:            0.10,
			BotFetched:      botFetched,
			BotUnlocked:     botUnlocked,
			BotHasAvailable: botHasAvailable,
			BotPercentUsed:  0.42,
			BotPlanLabel:    "Grok Bot Pro",
		}, true
	}
	t.Cleanup(func() {
		fetchAccountQuota = previous
		resetQuotaCache()
	})
}

func TestQuotaExhaustedBot(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		quota accountQuota
		want  bool
	}{
		{"not fetched fails open", accountQuota{BotFetched: false}, false},
		{"locked bar is exhausted", accountQuota{BotFetched: true, BotUnlocked: false}, true},
		{"unlocked with allowance", accountQuota{BotFetched: true, BotUnlocked: true, BotHasAvailable: true}, false},
		{"unlocked but spent", accountQuota{BotFetched: true, BotUnlocked: true, BotHasAvailable: false}, true},
	}
	for _, tc := range cases {
		if got := quotaExhaustedBot(tc.quota); got != tc.want {
			t.Errorf("%s: quotaExhaustedBot() = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFilterModelsForQuotaDropsGrokWhenSandLocked(t *testing.T) {
	// Account never claimed Sand: grok must not be advertised even though
	// the Auto bar is healthy.
	stubQuotaBot(t, true, false, false)
	acc := &auth.Account{Email: "nosand@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("nosand@example.com", acc, []string{
		"composer-2.5", "grok-4.6", "claude-sonnet-4-6",
	})
	for _, id := range got {
		if strings.Contains(id, "grok") {
			t.Fatalf("grok advertised on a Sand-locked account: %v", got)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %v, want composer+claude", got)
	}
}

func TestFilterModelsForQuotaKeepsGrokWhenSandAvailable(t *testing.T) {
	stubQuotaBot(t, true, true, true)
	acc := &auth.Account{Email: "sand@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("sand@example.com", acc, []string{
		"composer-2.5", "grok-4.6", "claude-sonnet-4-6",
	})
	if len(got) != 3 {
		t.Fatalf("got %v, want all three kept", got)
	}
}

func TestFilterModelsForQuotaDropsGrokWhenSandSpent(t *testing.T) {
	stubQuotaBot(t, true, true, false)
	acc := &auth.Account{Email: "spent@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("spent@example.com", acc, []string{
		"grok-4.6", "composer-2.5",
	})
	if len(got) != 1 || got[0] != "composer-2.5" {
		t.Fatalf("got %v, want composer only", got)
	}
}

func TestNormalizeAutoPercent(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in, want float64
	}{
		{0.07, 0.07},
		{1.0, 1.0},
		{1.45, 1.45},
		{100, 1.0},
	}
	for _, tc := range cases {
		if got := normalizeAutoPercent(tc.in); got != tc.want {
			t.Errorf("normalizeAutoPercent(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestQuotaExhaustedAuto(t *testing.T) {
	t.Parallel()
	if quotaExhaustedAuto(0.98) {
		t.Fatal("0.98 auto must still be routable")
	}
	if !quotaExhaustedAuto(1.0) {
		t.Fatal("1.0 auto is exhausted")
	}
}

// TestQuotaExhaustedOther pins the two-signal gate: only when BOTH the
// slow-pool flag and no_usage_based_allowed are set does CPA refuse to
// register Other Models. This is the case cctest.ai regressed: every
// healthy pool account showed api_percent_used=100 but neither flag,
// and CPA still unregistered Claude for them.
func TestQuotaExhaustedOther(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name         string
		slowPool     bool
		noUsageBased bool
		want         bool
	}{
		{"healthy team account", false, false, false},
		{"slow pool only (still has usage-based)", true, false, false},
		{"usage-based off only (not in slow pool)", false, true, false},
		{"fully exhausted", true, true, true},
	}
	for _, tc := range cases {
		got := quotaExhaustedOther(accountQuota{
			InSlowPool: tc.slowPool, NoUsageBasedAllowed: tc.noUsageBased,
		})
		if got != tc.want {
			t.Errorf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// TestFilterModelsForQuotaKeepsHealthyAPI reproduces the cctest.ai regression:
// Cursor reports api_percent_used=100 on every account whose total_spend
// exceeds the plan's free limit, even when the team plan continues to
// serve Other Models via usage-based billing. Without the slow-pool guard
// CPA drops every Claude/GPT/Gemini variant and cctest.ai's request for
// claude-opus-4-8-medium returns "unknown provider".
func TestFilterModelsForQuotaKeepsHealthyAPI(t *testing.T) {
	stubQuota(t, 0.07, false, false, true)
	acc := &auth.Account{Email: "healthy-team@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("healthy-team@example.com", acc, []string{
		"composer-2.5", "claude-sonnet-4-6",
	})
	if len(got) != 2 {
		t.Fatalf("got %v, want both models (in_slow_pool=false must not gate)", got)
	}
}

func TestModelQuotaBucket(t *testing.T) {
	t.Parallel()
	cursor := []string{
		"composer-2.5",
		"composer-2.5-fast",
		"composer-2.5[fast=true]",
		"cursor-small",
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
	// Grok bills against the Sand/Grok Bot bar, not Auto+Composer. The
	// cursor-prefixed id must land in bot too, which is why the grok test
	// runs before the cursor-/composer- prefix tests.
	bot := []string{
		"grok-4.6",
		"grok-4.6-high",
		"cursor-grok-4.6-high-fast",
		"cursor/grok-4.6",
		"grok-4.6[fast=true]",
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
	for _, id := range bot {
		if got := modelQuotaBucket(id); got != quotaBucketBot {
			t.Errorf("modelQuotaBucket(%q) = %s, want bot", id, got)
		}
	}
}

func TestFilterModelsForQuotaDropsExhaustedOther(t *testing.T) {
	// slow_pool=true AND no_usage_based_allowed=true — the account really
	// cannot serve Other Models any more.
	stubQuota(t, 0.07, true, true, true)
	acc := &auth.Account{Email: "pool@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("pool@example.com", acc, []string{
		"composer-2.5", "grok-4.6", "claude-sonnet-4-6", "gpt-5.5",
	})
	if len(got) != 2 || got[0] != "composer-2.5" || got[1] != "grok-4.6" {
		t.Fatalf("got %v, want composer+grok only", got)
	}
}

func TestFilterModelsForQuotaDropsExhaustedCursor(t *testing.T) {
	stubQuota(t, 1.45, false, false, true)
	acc := &auth.Account{Email: "over@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("over@example.com", acc, []string{
		"composer-2.5", "claude-sonnet-4-6",
	})
	if len(got) != 1 || got[0] != "claude-sonnet-4-6" {
		t.Fatalf("got %v, want claude only", got)
	}
}

func TestFilterModelsForQuotaFailsOpen(t *testing.T) {
	stubQuota(t, 0, false, false, false)
	acc := &auth.Account{Email: "blip@example.com", AccessToken: "tok"}
	in := []string{"composer-2.5", "claude-sonnet-4-6"}
	got := filterModelsForQuota("blip@example.com", acc, in)
	if len(got) != 2 {
		t.Fatalf("fail-open filtered to %v", got)
	}
}

func TestDispatch_ModelForAuthHidesOtherWhenExhausted(t *testing.T) {
	withoutCatalogRetries(t)
	stubQuota(t, 0.02, true, true, true)
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

func TestExecuteRejectsOtherModelsWhenQuotaExhausted(t *testing.T) {
	stubQuota(t, 0.04, true, true, true)
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

func TestFilterModelsForQuotaKeepsOtherWhenBotCanOverflow(t *testing.T) {
	previous := fetchAccountQuota
	resetQuotaCache()
	fetchAccountQuota = func(*auth.Account) (accountQuota, bool) {
		return accountQuota{
			Auto:                0.10,
			InSlowPool:          true,
			NoUsageBasedAllowed: true,
			BotFetched:          true,
			BotUnlocked:         true,
			BotHasAvailable:     true,
		}, true
	}
	t.Cleanup(func() {
		fetchAccountQuota = previous
		resetQuotaCache()
	})
	acc := &auth.Account{Email: "overflow@example.com", AccessToken: "tok"}
	got := filterModelsForQuota("overflow@example.com", acc, []string{
		"composer-2.5", "claude-sonnet-4-6", "grok-4.6",
	})
	if len(got) != 3 {
		t.Fatalf("got %v, want composer+claude+grok (other overflows to bot)", got)
	}
}

func TestExecuteOverflowsClaudeToBotWhenOtherExhausted(t *testing.T) {
	previous := fetchAccountQuota
	resetQuotaCache()
	fetchAccountQuota = func(*auth.Account) (accountQuota, bool) {
		return accountQuota{
			Auto:                0.04,
			InSlowPool:          true,
			NoUsageBasedAllowed: true,
			BotFetched:          true,
			BotUnlocked:         true,
			BotHasAvailable:     true,
		}, true
	}
	t.Cleanup(func() {
		fetchAccountQuota = previous
		resetQuotaCache()
	})
	var gotReq *executor.ChatRequest
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("ok"),
			buildTurnEndedEvent(4, 1),
		},
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) {
			return captureRunner{inner: runner, got: &gotReq}, "unit@example.com", nil
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
	if !env.OK {
		t.Fatalf("claude should overflow to bot, got %s", raw)
	}
	if gotReq == nil {
		t.Fatal("runner was not called")
	}
	if gotReq.RPCMode != executor.ChatRPCModeInferenceStream {
		t.Fatalf("RPCMode = %q, want inference_stream", gotReq.RPCMode)
	}
	if gotReq.ClientTypeOverride != "sand" {
		t.Fatalf("ClientTypeOverride = %q, want sand", gotReq.ClientTypeOverride)
	}
}

func TestExecuteAllowsComposerWhenOnlyOtherQuotaExhausted(t *testing.T) {
	stubQuota(t, 0.04, true, true, true)
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
