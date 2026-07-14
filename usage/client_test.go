package usage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	usagepb "github.com/router-for-me/cursor-proto/usage/pb"
	"google.golang.org/protobuf/proto"
)

// newFakeServer stands up an httptest.Server that speaks the Cursor unary
// proto shape. It routes on the request path and returns the given proto
// message (or 403 permission_denied).
func newFakeServer(t *testing.T, routes map[string]proto.Message, denied map[string]bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		if _, err := io.ReadAll(r.Body); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		if denied[path] {
			http.Error(w, "permission_denied: team scope required", http.StatusForbidden)
			return
		}
		msg, ok := routes[path]
		if !ok {
			http.Error(w, "not found: "+path, http.StatusNotFound)
			return
		}
		buf, err := proto.Marshal(msg)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		w.Header().Set("content-type", "application/proto")
		w.Write(buf)
	}))
}

func newTestClient(server string) *Client {
	acc := &auth.Account{Email: "test@example.com", AccessToken: "test"}
	acc.FillSessionDefaults(time.Now())
	c := executor.NewClient(acc)
	c.API2 = server
	c.API3 = server
	c.HTTP = &http.Client{Timeout: 5 * time.Second}
	return New(c)
}

func TestFetch_HappyPath(t *testing.T) {
	now := time.Now().UTC()
	periodStart := now.AddDate(0, -1, 0).UnixMilli()
	periodEnd := now.AddDate(0, 1, 0).UnixMilli()

	routes := map[string]proto.Message{
		"aiserver.v1.DashboardService/GetCurrentPeriodUsage": &usagepb.GetCurrentPeriodUsageResponse{
			BillingCycleStart: periodStart,
			BillingCycleEnd:   periodEnd,
			PlanUsage: &usagepb.GetCurrentPeriodUsageResponse_PlanUsage{
				TotalSpend:       1234,
				IncludedSpend:    1000,
				Remaining:        776,
				Limit:            2010,
				AutoSpend:        proto.Int32(120),
				AutoLimit:        proto.Int32(6000),
				AutoPercentUsed:  proto.Float64(0.02),
				ApiSpend:         proto.Int32(1114),
				ApiLimit:         proto.Int32(1114),
				ApiPercentUsed:   proto.Float64(1.0),
				TotalPercentUsed: proto.Float64(0.27),
			},
			Enabled:        true,
			DisplayMessage: "ok",
		},
		"aiserver.v1.DashboardService/GetCurrentBillingCycle": &usagepb.GetCurrentBillingCycleResponse{
			StartDateEpochMillis: periodStart,
			EndDateEpochMillis:   periodEnd,
		},
		"aiserver.v1.DashboardService/GetAggregatedUsageEvents": &usagepb.GetAggregatedUsageEventsResponse{
			TotalCostCents:        500,
			TotalInputTokens:      10240,
			TotalOutputTokens:     8192,
			TotalCacheReadTokens:  4096,
			TotalCacheWriteTokens: 2048,
		},
		"aiserver.v1.DashboardService/GetHardLimit": &usagepb.GetHardLimitResponse{
			HardLimit:                  20,
			NoUsageBasedAllowed:        false,
			PerUserMonthlyLimitDollars: 20,
		},
		"aiserver.v1.DashboardService/GetUsageBasedPremiumRequests": &usagepb.GetUsageBasedPremiumRequestsResponse{
			UsageBasedPremiumRequests: true,
		},
		"aiserver.v1.DashboardService/GetMe": &usagepb.GetMeResponse{
			AuthId:          "auth0|xyz",
			UserId:          42,
			Email:           proto.String("test@example.com"),
			Country:         proto.String("US"),
			CreatedAt:       proto.String("2025-01-02T03:04:05Z"),
			EmailDomainType: proto.String("non-professional"),
		},
		"aiserver.v1.DashboardService/GetUsageLimitStatusAndActiveGrants": &usagepb.GetUsageLimitStatusAndActiveGrantsResponse{
			UsageLimitPolicyStatus: &usagepb.GetUsageLimitStatusAndActiveGrantsResponse_UsageLimitPolicyStatus{
				IsInSlowPool:           false,
				ErrorTitle:             proto.String(""),
				CanConfigureSpendLimit: true,
				ResetAtMs:              proto.Int64(now.Add(24 * time.Hour).UnixMilli()),
				ResetDaysRemaining:     proto.Int32(1),
			},
		},
	}
	srv := newFakeServer(t, routes, nil)
	defer srv.Close()

	c := newTestClient(srv.URL)
	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}

	if snap.TotalSpend != 1234 {
		t.Errorf("total_spend: got %d want 1234", snap.TotalSpend)
	}
	if snap.Limit != 2010 {
		t.Errorf("limit: got %d want 2010", snap.Limit)
	}
	// Categorized spend — the three-bucket view Cursor's dashboard renders.
	if snap.AutoSpend != 120 {
		t.Errorf("auto_spend: got %d want 120", snap.AutoSpend)
	}
	if snap.AutoLimit != 6000 {
		t.Errorf("auto_limit: got %d want 6000", snap.AutoLimit)
	}
	if snap.APISpend != 1114 {
		t.Errorf("api_spend: got %d want 1114", snap.APISpend)
	}
	if snap.APILimit != 1114 {
		t.Errorf("api_limit: got %d want 1114", snap.APILimit)
	}
	if snap.AutoPercentUsed != 0.02 {
		t.Errorf("auto_percent_used: got %v want 0.02", snap.AutoPercentUsed)
	}
	if snap.APIPercentUsed != 1.0 {
		t.Errorf("api_percent_used: got %v want 1.0", snap.APIPercentUsed)
	}
	if snap.TotalPercentUsed != 0.27 {
		t.Errorf("total_percent_used: got %v want 0.27", snap.TotalPercentUsed)
	}
	if snap.HardLimit != 2000 { // 20 dollars → 2000 cents
		t.Errorf("hard_limit: got %d want 2000", snap.HardLimit)
	}
	if !snap.UsageBasedPremiumRequestsEnabled {
		t.Error("premium requests should be enabled")
	}
	if snap.Spend24h != 500 || snap.Spend7d != 500 || snap.Spend30d != 500 {
		t.Errorf("aggregated spends unexpected: 24h=%d 7d=%d 30d=%d", snap.Spend24h, snap.Spend7d, snap.Spend30d)
	}
	// Same GetAggregatedUsageEvents fixture drives all three windows,
	// so each of the four token counters should mirror across the
	// three TokenUsage buckets. If any of them diverge, aggregateJob
	// dropped a field somewhere.
	for name, tu := range map[string]TokenUsage{
		"24h": snap.Tokens24h, "7d": snap.Tokens7d, "30d": snap.Tokens30d,
	} {
		if tu.Input != 10240 || tu.Output != 8192 || tu.CacheRead != 4096 || tu.CacheWrite != 2048 {
			t.Errorf("tokens %s: got %+v want {10240,8192,4096,2048}", name, tu)
		}
	}
	if snap.PeriodStart.IsZero() || snap.PeriodEnd.IsZero() {
		t.Errorf("period bounds not set: start=%v end=%v", snap.PeriodStart, snap.PeriodEnd)
	}
	if snap.RateLimitResetDaysRemaining != 1 {
		t.Errorf("reset days: got %d want 1", snap.RateLimitResetDaysRemaining)
	}
	if snap.RateLimitResetAt == nil || snap.RateLimitResetAt.IsZero() {
		t.Error("rate_limit_reset_at should be set")
	}
	if !snap.Fetched.CurrentPeriodUsage || !snap.Fetched.HardLimit || !snap.Fetched.SlowPoolStatus || !snap.Fetched.Me {
		t.Errorf("fetched flags not all set: %+v", snap.Fetched)
	}
	if snap.Country != "US" || snap.SignUpType != "non-professional" || snap.Email != "test@example.com" {
		t.Errorf("GetMe fields not populated: country=%q sign_up_type=%q email=%q", snap.Country, snap.SignUpType, snap.Email)
	}
	if len(snap.Errors) != 0 {
		t.Errorf("unexpected errors: %v", snap.Errors)
	}
}

func TestFetch_TeamScopedPermissionDenied(t *testing.T) {
	// Simulate a personal Pro account: personal endpoints succeed, team-scoped
	// endpoints return 403. Fetch should NOT fail overall — errors go into the
	// Errors map and personal fields stay filled.
	routes := map[string]proto.Message{
		"aiserver.v1.DashboardService/GetCurrentPeriodUsage": &usagepb.GetCurrentPeriodUsageResponse{
			PlanUsage: &usagepb.GetCurrentPeriodUsageResponse_PlanUsage{TotalSpend: 42},
			Enabled:   true,
		},
		"aiserver.v1.DashboardService/GetCurrentBillingCycle": &usagepb.GetCurrentBillingCycleResponse{
			StartDateEpochMillis: 1000,
			EndDateEpochMillis:   2000,
		},
		"aiserver.v1.DashboardService/GetAggregatedUsageEvents": &usagepb.GetAggregatedUsageEventsResponse{TotalCostCents: 7},
		"aiserver.v1.DashboardService/GetUsageLimitStatusAndActiveGrants": &usagepb.GetUsageLimitStatusAndActiveGrantsResponse{
			UsageLimitPolicyStatus: &usagepb.GetUsageLimitStatusAndActiveGrantsResponse_UsageLimitPolicyStatus{IsInSlowPool: false},
		},
	}
	// Deny the team-scoped ones we DIDN'T register.
	denied := map[string]bool{
		"aiserver.v1.DashboardService/GetHardLimit":                 true,
		"aiserver.v1.DashboardService/GetUsageBasedPremiumRequests": true,
		"aiserver.v1.DashboardService/GetMe":                        true,
	}
	srv := newFakeServer(t, routes, denied)
	defer srv.Close()

	c := newTestClient(srv.URL)
	snap, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if snap.TotalSpend != 42 {
		t.Errorf("personal total_spend not filled: %d", snap.TotalSpend)
	}
	if snap.Fetched.HardLimit || snap.Fetched.PremiumRequests || snap.Fetched.Me {
		t.Errorf("denied groups should NOT be marked fetched: %+v", snap.Fetched)
	}
	if len(snap.Errors) != 3 {
		t.Errorf("expected 3 errors, got %d: %v", len(snap.Errors), snap.Errors)
	}
	if !IsPermissionDenied(errors.New(snap.Errors["hard_limit"])) {
		t.Errorf("hard_limit error should classify as permission denied: %q", snap.Errors["hard_limit"])
	}
}

func TestSnapshot_JSONRoundTrip(t *testing.T) {
	snap := &Snapshot{
		PeriodStart:                      time.UnixMilli(1_000_000_000).UTC(),
		PeriodEnd:                        time.UnixMilli(2_000_000_000).UTC(),
		TotalSpend:                       550,
		Limit:                            2000,
		Spend24h:                         50,
		HardLimit:                        2000,
		UsageBasedPremiumRequestsEnabled: true,
		Fetched:                          Fetched{CurrentPeriodUsage: true, HardLimit: true},
	}
	b, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"total_spend_cents":550`) {
		t.Errorf("cents field not present in JSON: %s", b)
	}
	var back Snapshot
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.TotalSpend != 550 || back.HardLimit != 2000 {
		t.Errorf("roundtrip mismatch: %+v", back)
	}
}

func TestFormatCents(t *testing.T) {
	cases := map[int64]string{
		0:      "$0.00",
		1:      "$0.01",
		100:    "$1.00",
		1234:   "$12.34",
		-42:    "-$0.42",
		200000: "$2000.00",
	}
	for cents, want := range cases {
		if got := FormatCents(cents); got != want {
			t.Errorf("FormatCents(%d) = %q, want %q", cents, got, want)
		}
	}
}

func TestIsPermissionDenied(t *testing.T) {
	yes := []error{
		errors.New("http 403: permission_denied"),
		errors.New("PERMISSION DENIED"),
		errors.New("http 403 forbidden"),
	}
	no := []error{
		nil,
		errors.New("http 500: internal"),
		errors.New("network unreachable"),
	}
	for _, e := range yes {
		if !IsPermissionDenied(e) {
			t.Errorf("expected true for %v", e)
		}
	}
	for _, e := range no {
		if IsPermissionDenied(e) {
			t.Errorf("expected false for %v", e)
		}
	}
}
