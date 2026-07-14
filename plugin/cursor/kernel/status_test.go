package kernel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
	"github.com/router-for-me/cursor-proto/usage"
)

// fakeSnapshot builds a fully-populated usage.Snapshot for testing.
func fakeSnapshot() *usage.Snapshot {
	rlReset := time.Now().Add(30 * time.Minute).UTC()
	return &usage.Snapshot{
		PeriodStart:                      time.Now().Add(-15 * 24 * time.Hour).UTC(),
		PeriodEnd:                        time.Now().Add(15 * 24 * time.Hour).UTC(),
		TotalSpend:                       1250,
		IncludedSpend:                    800,
		Remaining:                        1750,
		Limit:                            3000,
		Spend24h:                         120,
		Spend7d:                          450,
		Spend30d:                         1200,
		AutoSpend:                        30,
		AutoLimit:                        6000,
		AutoPercentUsed:                  0.005,
		APISpend:                         1220,
		APILimit:                         1500,
		APIPercentUsed:                   0.81,
		TotalPercentUsed:                 0.42,
		Tokens24h:                        usage.TokenUsage{Input: 10240, Output: 8192, CacheRead: 4096, CacheWrite: 2048},
		Tokens7d:                         usage.TokenUsage{Input: 51200, Output: 40960, CacheRead: 20480, CacheWrite: 10240},
		Tokens30d:                        usage.TokenUsage{Input: 204800, Output: 163840, CacheRead: 81920, CacheWrite: 40960},
		InSlowPool:                       false,
		SlowReason:                       "",
		RateLimitResetAt:                 &rlReset,
		RateLimitResetDaysRemaining:      1,
		HardLimit:                        50000,
		UsageBasedPremiumRequestsEnabled: true,
		Email:                            "pool@example.com",
		Country:                          "US",
		SignUpType:                       "personal",
		Fetched: usage.Fetched{
			CurrentPeriodUsage: true,
			BillingCycle:       true,
			Aggregated24h:      true,
			Aggregated7d:       true,
			Aggregated30d:      true,
			SlowPoolStatus:     true,
			HardLimit:          true,
			PremiumRequests:    true,
			Me:                 true,
		},
	}
}

// makeJWTAccessToken builds a synthetic JWT with the given expiry
// claim so we can exercise decodeJWTExpiry without real Cursor tokens.
func makeJWTAccessToken(t *testing.T, exp time.Time) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	payload := struct {
		Exp int64 `json:"exp"`
	}{Exp: exp.Unix()}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	pb := base64.RawURLEncoding.EncodeToString(body)
	sig := base64.RawURLEncoding.EncodeToString([]byte("sig"))
	return header + "." + pb + "." + sig
}

func TestFetchAccountStatus_ShapesJSON(t *testing.T) {
	exp := time.Now().Add(2 * time.Hour).UTC()
	tok := makeJWTAccessToken(t, exp)
	acc := &auth.Account{
		Email:        "pool@example.com",
		UserID:       "user_pool",
		AccessToken:  tok,
		RefreshToken: "rt",
		AuthID:       "auth0|user_pool",
	}
	acc.FillSessionDefaults(time.Now())

	fetcher := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		return fakeSnapshot(), nil
	}
	status, err := fetchAccountStatusWith(context.Background(), acc, fetcher)
	if err != nil {
		t.Fatalf("fetchAccountStatusWith: %v", err)
	}
	if status.Email != "pool@example.com" {
		t.Errorf("Email = %q", status.Email)
	}
	if status.Country != "US" {
		t.Errorf("Country = %q", status.Country)
	}
	if !status.CanCallClaude {
		t.Error("CanCallClaude should be true for US country")
	}
	if !status.CanCallComposer {
		t.Error("CanCallComposer must be true for all accounts")
	}
	if !status.Refreshable {
		t.Error("Refreshable should be true when RefreshToken present")
	}
	if status.Plan == "" || status.Plan == "unknown" {
		t.Errorf("Plan should derive to Pro-ish, got %q", status.Plan)
	}
	if status.RemainingCents != 1750 {
		t.Errorf("RemainingCents = %d, want 1750", status.RemainingCents)
	}
	if status.Spend24hCents != 120 {
		t.Errorf("Spend24hCents = %d, want 120", status.Spend24hCents)
	}
	if status.HardLimitCents != 50000 {
		t.Errorf("HardLimitCents = %d, want 50000", status.HardLimitCents)
	}
	// Categorized spend (v0.2.4 wire → v0.3.2 plugin). Auto + API +
	// TotalPercent should all mirror the fake snapshot verbatim.
	if status.AutoSpendCents != 30 || status.AutoLimitCents != 6000 || status.AutoPercentUsed != 0.005 {
		t.Errorf("auto bucket wrong: %+v", status)
	}
	if status.APISpendCents != 1220 || status.APILimitCents != 1500 || status.APIPercentUsed != 0.81 {
		t.Errorf("api bucket wrong: %+v", status)
	}
	if status.TotalPercentUsed != 0.42 {
		t.Errorf("total_percent_used = %v", status.TotalPercentUsed)
	}
	// Token breakdown (v0.2.7 wire → v0.3.2 plugin). All three
	// windows should be filled from the fake snapshot.
	if status.Tokens24h.Input != 10240 || status.Tokens24h.CacheRead != 4096 {
		t.Errorf("tokens_24h: %+v", status.Tokens24h)
	}
	if status.Tokens7d.Input != 51200 || status.Tokens30d.Input != 204800 {
		t.Errorf("tokens_7d/30d wrong: %+v %+v", status.Tokens7d, status.Tokens30d)
	}
	if len(status.Models) == 0 {
		t.Error("Models must be populated")
	}
	// JSON shape check.
	buf, err := json.Marshal(status)
	if err != nil {
		t.Fatalf("marshal status: %v", err)
	}
	// Verify a few key field names exist in the on-wire form.
	for _, key := range []string{
		`"email"`, `"country"`, `"plan"`, `"spend_cents"`, `"remaining_cents"`,
		`"in_slow_pool"`, `"can_call_claude"`, `"jwt_expires_at"`, `"models"`,
		`"auto_spend_cents"`, `"api_spend_cents"`, `"total_percent_used"`,
		`"tokens_24h"`, `"tokens_7d"`, `"tokens_30d"`,
	} {
		if !strings.Contains(string(buf), key) {
			t.Errorf("JSON missing %s: %s", key, string(buf))
		}
	}
}

func TestFetchAccountStatus_NonAllowlistedCountry(t *testing.T) {
	acc := &auth.Account{
		Email:       "cn@example.com",
		AccessToken: makeJWTAccessToken(t, time.Now().Add(time.Hour)),
	}
	acc.FillSessionDefaults(time.Now())
	fetcher := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		snap := fakeSnapshot()
		snap.Country = "CN"
		snap.Email = "cn@example.com"
		return snap, nil
	}
	status, err := fetchAccountStatusWith(context.Background(), acc, fetcher)
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if status.CanCallClaude {
		t.Error("CanCallClaude should be false for CN")
	}
	// Even CN accounts have composer + gemini + gpt access.
	haveClaude := false
	for _, m := range status.Models {
		if strings.HasPrefix(m, "claude-") {
			haveClaude = true
		}
	}
	if haveClaude {
		t.Errorf("Models should not include claude-* for CN: %v", status.Models)
	}
}

func TestDerivePlan(t *testing.T) {
	cases := []struct {
		name string
		snap *usage.Snapshot
		want string
	}{
		{
			name: "team-signup",
			snap: &usage.Snapshot{SignUpType: "team"},
			want: "Team",
		},
		{
			name: "pro-limit",
			snap: &usage.Snapshot{Limit: 2000},
			want: "Pro",
		},
		{
			name: "free-zero-limit",
			snap: &usage.Snapshot{Fetched: usage.Fetched{CurrentPeriodUsage: true}},
			want: "Free",
		},
		{
			name: "unknown-empty",
			snap: &usage.Snapshot{},
			want: "unknown",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := derivePlan(tc.snap); got != tc.want {
				t.Errorf("derivePlan = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestDecodeJWTExpiry(t *testing.T) {
	exp := time.Now().Add(90 * time.Minute).Truncate(time.Second).UTC()
	tok := makeJWTAccessToken(t, exp)
	got, ok := decodeJWTExpiry(tok)
	if !ok {
		t.Fatal("decodeJWTExpiry returned ok=false")
	}
	if !got.Equal(exp) {
		t.Errorf("decoded exp = %s, want %s", got, exp)
	}
	if _, ok := decodeJWTExpiry("not.a.jwt"); ok {
		t.Error("expected ok=false for garbage token")
	}
	// Bearer-prefixed style.
	pref := "auth0|user_pool::" + tok
	got2, ok := decodeJWTExpiry(pref)
	if !ok || !got2.Equal(exp) {
		t.Errorf("prefixed decode failed: ok=%v got=%s", ok, got2)
	}
}

// registryFixture returns a fresh authRegistry pre-loaded with two
// accounts. Callers pass a fetch closure so they can decide which
// snapshot shape each email sees.
func registryFixture(t *testing.T, fetch SnapshotFetcher) *authRegistry {
	t.Helper()
	reg := newAuthRegistry(30 * time.Second)
	reg.SetFetcher(fetch)
	usAcc := &auth.Account{
		Email:        "us@example.com",
		AccessToken:  makeJWTAccessToken(t, time.Now().Add(2*time.Hour)),
		RefreshToken: "rt-us",
	}
	usAcc.FillSessionDefaults(time.Now())
	cnAcc := &auth.Account{
		Email:       "cn@example.com",
		AccessToken: makeJWTAccessToken(t, time.Now().Add(30*time.Minute)),
	}
	cnAcc.FillSessionDefaults(time.Now())
	reg.Register(usAcc)
	reg.Register(cnAcc)
	return reg
}

func TestAuthRegistry_ListAndCache(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		calls++
		snap := fakeSnapshot()
		if a.Email == "cn@example.com" {
			snap.Country = "CN"
			snap.Email = a.Email
		} else {
			snap.Country = "US"
			snap.Email = a.Email
		}
		return snap, nil
	}
	reg := registryFixture(t, fetch)
	if got := len(reg.List()); got != 2 {
		t.Fatalf("List len = %d, want 2", got)
	}
	// First call fetches; second within TTL should be cached.
	if _, err := reg.Status(context.Background(), "us@example.com", false); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.Status(context.Background(), "us@example.com", false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("expected 1 fetch after cache hit, got %d", calls)
	}
	// Force bypasses cache.
	if _, err := reg.Status(context.Background(), "us@example.com", true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force=true should bypass cache, got calls=%d", calls)
	}
	// Invalidate then fetch.
	reg.Invalidate("us@example.com")
	if _, err := reg.Status(context.Background(), "us@example.com", false); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Errorf("invalidate should force refetch, got calls=%d", calls)
	}
}

func TestAuthRegistry_LoadFromDisk(t *testing.T) {
	tmp := t.TempDir()
	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:        cpaformat.ProviderType,
			AccessToken: makeJWTAccessToken(t, time.Now().Add(time.Hour)),
			Email:       "disk@example.com",
		},
	}
	if _, err := file.WriteToDir(tmp); err != nil {
		t.Fatalf("write: %v", err)
	}
	reg := newAuthRegistry(30 * time.Second)
	reg.SetAuthDir(tmp)
	n, err := reg.LoadFromDisk()
	if err != nil {
		t.Fatalf("LoadFromDisk: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 account, got %d", n)
	}
	if _, ok := reg.Get("disk@example.com"); !ok {
		t.Error("disk@example.com not registered")
	}
}

// helper for management tests: run routeManagement against a fixture
// registry so we can exercise the routes without the global singleton.
func withFixture(t *testing.T, fetch SnapshotFetcher, run func()) {
	t.Helper()
	prev := globalRegistry
	globalRegistry = registryFixture(t, fetch)
	defer func() { globalRegistry = prev }()
	run()
}

func TestManagement_ListAccounts_JSONShape(t *testing.T) {
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		snap := fakeSnapshot()
		snap.Email = a.Email
		if a.Email == "cn@example.com" {
			snap.Country = "CN"
		}
		return snap, nil
	}
	withFixture(t, fetch, func() {
		resp := routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   managementBasePath + routePrefix + "/accounts",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("StatusCode = %d, want 200 (body=%s)", resp.StatusCode, string(resp.Body))
		}
		var body struct {
			Accounts []*AccountStatus `json:"accounts"`
			Count    int              `json:"count"`
		}
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Count != 2 {
			t.Errorf("count = %d, want 2", body.Count)
		}
		if len(body.Accounts) != 2 {
			t.Errorf("len(accounts) = %d, want 2", len(body.Accounts))
		}
	})
}

func TestManagement_AccountDetail(t *testing.T) {
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		return fakeSnapshot(), nil
	}
	withFixture(t, fetch, func() {
		resp := routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   managementBasePath + routePrefix + "/account",
			Query:  map[string][]string{"email": {"us@example.com"}},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("StatusCode = %d (body=%s)", resp.StatusCode, string(resp.Body))
		}
		// Missing email should be 400.
		resp = routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   managementBasePath + routePrefix + "/account",
		})
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("empty email StatusCode = %d, want 400", resp.StatusCode)
		}
		// Unknown account should be 404.
		resp = routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   managementBasePath + routePrefix + "/account",
			Query:  map[string][]string{"email": {"nope@example.com"}},
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("unknown email StatusCode = %d, want 404", resp.StatusCode)
		}
	})
}

func TestManagement_PoolSummary(t *testing.T) {
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		snap := fakeSnapshot()
		snap.Email = a.Email
		if a.Email == "cn@example.com" {
			snap.Country = "CN"
			snap.InSlowPool = true
			snap.SlowReason = "included spend exceeded"
		}
		return snap, nil
	}
	withFixture(t, fetch, func() {
		resp := routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   managementBasePath + routePrefix + "/pool-summary",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("StatusCode = %d (body=%s)", resp.StatusCode, string(resp.Body))
		}
		var body struct {
			Total            int            `json:"total"`
			Active           int            `json:"active"`
			SlowPool         int            `json:"slow_pool"`
			ExpiringSoon     int            `json:"expiring_soon"`
			CountryBreakdown map[string]int `json:"country_breakdown"`
			PlanBreakdown    map[string]int `json:"plan_breakdown"`
		}
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Total != 2 {
			t.Errorf("total = %d, want 2", body.Total)
		}
		if body.SlowPool != 1 {
			t.Errorf("slow_pool = %d, want 1", body.SlowPool)
		}
		if body.Active != 1 {
			t.Errorf("active = %d, want 1", body.Active)
		}
		if body.CountryBreakdown["US"] != 1 || body.CountryBreakdown["CN"] != 1 {
			t.Errorf("country_breakdown = %+v", body.CountryBreakdown)
		}
	})
}

func TestManagement_AccountProbe_InvalidatesCache(t *testing.T) {
	calls := 0
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		calls++
		snap := fakeSnapshot()
		snap.Email = a.Email
		return snap, nil
	}
	withFixture(t, fetch, func() {
		// warm cache
		resp := routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   managementBasePath + routePrefix + "/account",
			Query:  map[string][]string{"email": {"us@example.com"}},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("warm-up failed: %d (%s)", resp.StatusCode, string(resp.Body))
		}
		firstCalls := calls
		// probe -> forces new fetch
		resp = routeManagement(context.Background(), managementRequest{
			Method: http.MethodPost,
			Path:   managementBasePath + routePrefix + "/account/probe",
			Query:  map[string][]string{"email": {"us@example.com"}},
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("probe failed: %d (%s)", resp.StatusCode, string(resp.Body))
		}
		if calls <= firstCalls {
			t.Error("probe should re-fetch")
		}
	})
}

func TestManagement_Register_HasRoutes(t *testing.T) {
	raw := managementRegisterResult()
	var body struct {
		Routes []map[string]any `json:"routes"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Routes) == 0 {
		t.Fatal("no routes advertised")
	}
	menuFound := false
	for _, r := range body.Routes {
		if r["Menu"] == pluginMenuLabel {
			menuFound = true
		}
	}
	if !menuFound {
		t.Errorf("no route carries the sidebar menu label %q", pluginMenuLabel)
	}
}

func TestManagement_Dispatch_Register(t *testing.T) {
	raw, rc := dispatch("management.register", nil)
	if rc != 0 {
		t.Fatalf("rc = %d, body = %s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
}

func TestManagement_Dispatch_Handle(t *testing.T) {
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		return fakeSnapshot(), nil
	}
	withFixture(t, fetch, func() {
		payload, err := json.Marshal(managementRequest{
			Method: http.MethodGet,
			Path:   managementBasePath + routePrefix + "/pool-summary",
		})
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		raw, rc := dispatch("management.handle", payload)
		if rc != 0 {
			t.Fatalf("rc = %d, body = %s", rc, string(raw))
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if !env.OK {
			t.Fatalf("envelope not OK: %+v", env.Error)
		}
	})
}

func TestSanitiseErrCode(t *testing.T) {
	if got := sanitiseErrCode(nil); got != "" {
		t.Errorf("nil err = %q", got)
	}
	if got := sanitiseErrCode(errors.New("Snapshot Failed: 500")); got != "snapshot_failed" {
		t.Errorf("got %q", got)
	}
}
