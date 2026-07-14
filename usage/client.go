// Package usage fetches Cursor account usage/quota data from the api2
// DashboardService endpoints.
//
// Cursor does NOT use Claude-Code-style rolling windows. Its model is:
//  1. Billing cycles (typically monthly) with a hard $ spend limit.
//  2. Usage events aggregatable by any date range.
//  3. A "slow pool" state that trips when included quota is exceeded.
//  4. Short-window rate limits (reset_at_ms / reset_days_remaining).
//
// This package consumes six unary RPCs in parallel and returns a Snapshot
// that downstream systems (CLIProxyAPI account list, /v1/usage HTTP endpoint)
// can display as-is.
package usage

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/cursor-proto/executor"
	usagepb "github.com/router-for-me/cursor-proto/usage/pb"
	"google.golang.org/protobuf/proto"
)

// TokenUsage is one window's worth of prompt / response / cache
// token counts. Mirrors the four scalar counters on
// GetAggregatedUsageEventsResponse (total_input_tokens,
// total_output_tokens, total_cache_read_tokens,
// total_cache_write_tokens). All fields are int64 so a downstream
// summing tokens across long horizons doesn't overflow.
type TokenUsage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Snapshot is the aggregated usage view we expose to callers.
//
// All money values are in cents (int64). Zero values are legitimate when the
// account really is at zero — use the *Filled maps below (populated by Fetch)
// to distinguish "field skipped" from "field is zero".
type Snapshot struct {
	// Current period ($ terms) — from GetCurrentPeriodUsage + GetCurrentBillingCycle.
	PeriodStart   time.Time `json:"period_start"`
	PeriodEnd     time.Time `json:"period_end"`
	TotalSpend    int64     `json:"total_spend_cents"`
	IncludedSpend int64     `json:"included_spend_cents"`
	Remaining     int64     `json:"remaining_cents"`
	Limit         int64     `json:"limit_cents"`

	// Categorized spend — Cursor's own dashboard renders these as two
	// progress bars alongside the Total bar. AutoSpend covers the
	// "Auto + Composer" bucket (Cursor merges those upstream into a
	// single scalar auto_spend field, matching the UI label); APISpend
	// covers third-party API-quota usage (Claude/GPT/Gemini families).
	// PercentUsed fields are Cursor's own precomputed doubles from the
	// PlanUsage proto — use them verbatim so downstream matches the IDE
	// dashboard byte-for-byte instead of recomputing from cents.
	AutoSpend        int64   `json:"auto_spend_cents"`
	AutoLimit        int64   `json:"auto_limit_cents"`
	AutoPercentUsed  float64 `json:"auto_percent_used"`
	APISpend         int64   `json:"api_spend_cents"`
	APILimit         int64   `json:"api_limit_cents"`
	APIPercentUsed   float64 `json:"api_percent_used"`
	TotalPercentUsed float64 `json:"total_percent_used"`

	// Windowed spend (cents) — from GetAggregatedUsageEvents with explicit windows.
	Spend24h int64 `json:"spend_24h_cents"`
	Spend7d  int64 `json:"spend_7d_cents"`
	Spend30d int64 `json:"spend_30d_cents"`

	// Token counts, one bucket per window. Populated from the same
	// GetAggregatedUsageEvents RPC that fills Spend*, so no extra
	// upstream round-trips. Zero is legitimate ("no traffic in this
	// window"); use Fetched.Aggregated24h/7d/30d to distinguish
	// "not fetched" from "fetched, actually zero". cache_read tokens
	// are effectively free on Cursor's plans; cache_write is billable
	// but at reduced rate — surfacing both lets downstream compute
	// effective cost per raw prompt token.
	Tokens24h TokenUsage `json:"tokens_24h"`
	Tokens7d  TokenUsage `json:"tokens_7d"`
	Tokens30d TokenUsage `json:"tokens_30d"`

	// Slow pool state — from GetUsageLimitStatusAndActiveGrants.
	InSlowPool bool   `json:"in_slow_pool"`
	SlowReason string `json:"slow_reason,omitempty"`
	SlowDetail string `json:"slow_detail,omitempty"`
	SlownessMs int64  `json:"slowness_ms,omitempty"`

	// Short-window rate limits — from UsageLimitPolicyStatus embedded in the same response.
	// RateLimitResetAt is emitted as a nullable JSON field so an unset zero time
	// serializes as null (not the ugly "0001-01-01T00:00:00Z").
	RateLimitResetAt            *time.Time `json:"rate_limit_reset_at,omitempty"`
	RateLimitResetDaysRemaining int32      `json:"rate_limit_reset_days_remaining,omitempty"`
	// Populated by callers when Cursor returns a 429 for a chat request.
	LastRateLimitError string `json:"last_rate_limit_error,omitempty"`
	LastRateLimitTitle string `json:"last_rate_limit_title,omitempty"`

	// Hard limit — from GetHardLimit.
	HardLimit           int64 `json:"hard_limit_cents"`
	NoUsageBasedAllowed bool  `json:"no_usage_based_allowed"`

	// Premium request flag — from GetUsageBasedPremiumRequests.
	UsageBasedPremiumRequestsEnabled bool `json:"usage_based_premium_requests_enabled"`

	// Identity / account metadata — from GetMe. Country is critical for CPA's
	// per-account model gating (e.g. CN accounts get a restricted model set).
	Email      string `json:"email,omitempty"`
	Country    string `json:"country,omitempty"`
	CreatedAt  string `json:"created_at,omitempty"`
	SignUpType string `json:"sign_up_type,omitempty"`

	// Fetched records which RPC groups succeeded so the JSON consumer can
	// tell "0 = actual 0" from "0 = permission_denied, skipped".
	Fetched Fetched `json:"fetched"`

	// Errors accumulated per RPC (nil-safe map form is easier for JSON).
	Errors map[string]string `json:"errors,omitempty"`
}

// Fetched flags each RPC group by name; a true value means the corresponding
// snapshot fields are authoritative.
type Fetched struct {
	CurrentPeriodUsage bool `json:"current_period_usage"`
	BillingCycle       bool `json:"billing_cycle"`
	Aggregated24h      bool `json:"aggregated_24h"`
	Aggregated7d       bool `json:"aggregated_7d"`
	Aggregated30d      bool `json:"aggregated_30d"`
	SlowPoolStatus     bool `json:"slow_pool_status"`
	HardLimit          bool `json:"hard_limit"`
	PremiumRequests    bool `json:"premium_requests"`
	Me                 bool `json:"me"`
}

// Client wraps an authenticated executor.Client to make usage RPCs.
type Client struct {
	Exec *executor.Client
}

// New returns a Client sharing the given executor's auth + headers.
func New(exec *executor.Client) *Client { return &Client{Exec: exec} }

// Fetch runs all usage RPCs in parallel and returns the aggregated Snapshot.
//
// Team-scoped RPCs on personal-Pro accounts often return permission_denied.
// Those errors are recorded in Snapshot.Errors and don't fail the overall
// fetch; a partially-populated Snapshot is returned.
func (c *Client) Fetch(ctx context.Context) (*Snapshot, error) {
	if c == nil || c.Exec == nil {
		return nil, errors.New("usage: nil client or executor")
	}

	snap := &Snapshot{Errors: map[string]string{}}

	now := time.Now().UTC()
	ms24h := now.Add(-24 * time.Hour).UnixMilli()
	ms7d := now.Add(-7 * 24 * time.Hour).UnixMilli()
	ms30d := now.Add(-30 * 24 * time.Hour).UnixMilli()
	msNow := now.UnixMilli()

	type job struct {
		name string
		run  func() error
	}

	var mu sync.Mutex
	setErr := func(name string, err error) {
		mu.Lock()
		snap.Errors[name] = err.Error()
		mu.Unlock()
	}

	jobs := []job{
		{
			name: "current_period_usage",
			run: func() error {
				req := &usagepb.GetCurrentPeriodUsageRequest{}
				resp := &usagepb.GetCurrentPeriodUsageResponse{}
				if err := c.call(ctx, "aiserver.v1.DashboardService", "GetCurrentPeriodUsage", req, resp); err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				if pu := resp.GetPlanUsage(); pu != nil {
					snap.TotalSpend = int64(pu.GetTotalSpend())
					snap.IncludedSpend = int64(pu.GetIncludedSpend())
					snap.Remaining = int64(pu.GetRemaining())
					snap.Limit = int64(pu.GetLimit())
					// Categorized fields are proto3 `optional`; Get*
					// returns the zero value when absent, which is
					// legitimate ("$0 spent so far this period"). Use
					// Snapshot.Fetched.CurrentPeriodUsage to tell "not
					// fetched" from "fetched, actually zero".
					snap.AutoSpend = int64(pu.GetAutoSpend())
					snap.AutoLimit = int64(pu.GetAutoLimit())
					snap.AutoPercentUsed = pu.GetAutoPercentUsed()
					snap.APISpend = int64(pu.GetApiSpend())
					snap.APILimit = int64(pu.GetApiLimit())
					snap.APIPercentUsed = pu.GetApiPercentUsed()
					snap.TotalPercentUsed = pu.GetTotalPercentUsed()
				}
				if snap.PeriodStart.IsZero() && resp.GetBillingCycleStart() > 0 {
					snap.PeriodStart = time.UnixMilli(resp.GetBillingCycleStart()).UTC()
				}
				if snap.PeriodEnd.IsZero() && resp.GetBillingCycleEnd() > 0 {
					snap.PeriodEnd = time.UnixMilli(resp.GetBillingCycleEnd()).UTC()
				}
				snap.Fetched.CurrentPeriodUsage = true
				return nil
			},
		},
		{
			name: "billing_cycle",
			run: func() error {
				req := &usagepb.GetCurrentBillingCycleRequest{}
				resp := &usagepb.GetCurrentBillingCycleResponse{}
				if err := c.call(ctx, "aiserver.v1.DashboardService", "GetCurrentBillingCycle", req, resp); err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				if resp.GetStartDateEpochMillis() > 0 {
					snap.PeriodStart = time.UnixMilli(resp.GetStartDateEpochMillis()).UTC()
				}
				if resp.GetEndDateEpochMillis() > 0 {
					snap.PeriodEnd = time.UnixMilli(resp.GetEndDateEpochMillis()).UTC()
				}
				snap.Fetched.BillingCycle = true
				return nil
			},
		},
		{
			name: "aggregated_24h",
			run:  c.aggregateJob(ctx, ms24h, msNow, &snap.Spend24h, &snap.Tokens24h, &snap.Fetched.Aggregated24h, &mu),
		},
		{
			name: "aggregated_7d",
			run:  c.aggregateJob(ctx, ms7d, msNow, &snap.Spend7d, &snap.Tokens7d, &snap.Fetched.Aggregated7d, &mu),
		},
		{
			name: "aggregated_30d",
			run:  c.aggregateJob(ctx, ms30d, msNow, &snap.Spend30d, &snap.Tokens30d, &snap.Fetched.Aggregated30d, &mu),
		},
		{
			name: "usage_limit_status",
			run: func() error {
				req := &usagepb.GetUsageLimitStatusAndActiveGrantsRequest{}
				resp := &usagepb.GetUsageLimitStatusAndActiveGrantsResponse{}
				if err := c.call(ctx, "aiserver.v1.DashboardService", "GetUsageLimitStatusAndActiveGrants", req, resp); err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				if pol := resp.GetUsageLimitPolicyStatus(); pol != nil {
					snap.InSlowPool = pol.GetIsInSlowPool()
					snap.SlowReason = pol.GetErrorTitle()
					snap.SlowDetail = pol.GetErrorDetail()
					snap.SlownessMs = int64(pol.GetSlownessMs())
					if r := pol.GetResetAtMs(); r > 0 {
						t := time.UnixMilli(r).UTC()
						snap.RateLimitResetAt = &t
					}
					snap.RateLimitResetDaysRemaining = pol.GetResetDaysRemaining()
				}
				snap.Fetched.SlowPoolStatus = true
				return nil
			},
		},
		{
			name: "hard_limit",
			run: func() error {
				req := &usagepb.GetHardLimitRequest{}
				resp := &usagepb.GetHardLimitResponse{}
				if err := c.call(ctx, "aiserver.v1.DashboardService", "GetHardLimit", req, resp); err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				// hard_limit is dollars (per Cursor's schema); convert to cents.
				snap.HardLimit = int64(resp.GetHardLimit()) * 100
				snap.NoUsageBasedAllowed = resp.GetNoUsageBasedAllowed()
				snap.Fetched.HardLimit = true
				return nil
			},
		},
		{
			name: "premium_requests",
			run: func() error {
				req := &usagepb.GetUsageBasedPremiumRequestsRequest{}
				resp := &usagepb.GetUsageBasedPremiumRequestsResponse{}
				if err := c.call(ctx, "aiserver.v1.DashboardService", "GetUsageBasedPremiumRequests", req, resp); err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				snap.UsageBasedPremiumRequestsEnabled = resp.GetUsageBasedPremiumRequests()
				snap.Fetched.PremiumRequests = true
				return nil
			},
		},
		{
			name: "me",
			run: func() error {
				req := &usagepb.GetMeRequest{}
				resp := &usagepb.GetMeResponse{}
				if err := c.call(ctx, "aiserver.v1.DashboardService", "GetMe", req, resp); err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				snap.Email = resp.GetEmail()
				snap.Country = resp.GetCountry()
				snap.CreatedAt = resp.GetCreatedAt()
				snap.SignUpType = resp.GetEmailDomainType()
				snap.Fetched.Me = true
				return nil
			},
		},
	}

	var wg sync.WaitGroup
	for _, j := range jobs {
		wg.Add(1)
		go func(j job) {
			defer wg.Done()
			if err := j.run(); err != nil {
				setErr(j.name, err)
			}
		}(j)
	}
	wg.Wait()

	if len(snap.Errors) == 0 {
		snap.Errors = nil
	}
	return snap, nil
}

// aggregateJob returns a closure that runs GetAggregatedUsageEvents for
// a specific [start,end] window and updates the target spend + tokens
// fields on success. All four token counters come from the same RPC
// response — no extra round-trips.
func (c *Client) aggregateJob(ctx context.Context, startMs, endMs int64, spend *int64, tokens *TokenUsage, filled *bool, mu *sync.Mutex) func() error {
	return func() error {
		start := startMs
		end := endMs
		req := &usagepb.GetAggregatedUsageEventsRequest{
			StartDate: &start,
			EndDate:   &end,
		}
		resp := &usagepb.GetAggregatedUsageEventsResponse{}
		if err := c.call(ctx, "aiserver.v1.DashboardService", "GetAggregatedUsageEvents", req, resp); err != nil {
			return err
		}
		mu.Lock()
		defer mu.Unlock()
		*spend = int64(resp.GetTotalCostCents())
		tokens.Input = resp.GetTotalInputTokens()
		tokens.Output = resp.GetTotalOutputTokens()
		tokens.CacheRead = resp.GetTotalCacheReadTokens()
		tokens.CacheWrite = resp.GetTotalCacheWriteTokens()
		*filled = true
		return nil
	}
}

// -------- per-request event log --------
//
// GetFilteredUsageEvents on aiserver.v1.DashboardService returns the
// paginated event log that backs cursor2api's /usage table. Distinct
// from the aggregation RPC (GetAggregatedUsageEvents) that fills
// Snapshot.Spend* / Tokens*: this one gives you one row per assistant
// response, ordered newest first. See docs/upstream-issues/
// usage-events-list.md in cursor2api for the downstream spec.

// EventListOptions bounds a ListEvents query. All fields are optional.
// Zero values fall back to Cursor's defaults (all users, all models,
// page 1, a server-chosen page_size).
type EventListOptions struct {
	// Time window: unix millis. Zero means "let Cursor pick" (usually
	// current billing period). Cursor rejects windows > 30 days.
	StartMs int64
	EndMs   int64

	// Model filter — exact match on the model_id string Cursor stores
	// (e.g. "claude-sonnet-4-5-20250929"). Empty means all models.
	Model string

	// Pagination. Page is 1-based. PageSize defaults to 50 server-side;
	// the RPC accepts up to a few hundred but very large pages get
	// truncated silently.
	Page     int32
	PageSize int32
}

// EventPage is one page of results. Events carries the UI-ready
// projection (UsageEventDisplay) — it's what cursor2api's table
// renders. TotalCount is authoritative for pagination controls
// (has_next = Page * PageSize < TotalCount).
type EventPage struct {
	Events     []*usagepb.UsageEventDisplay
	TotalCount int32
}

// ListEvents fetches one page of the per-request event log.
//
// Returns EventPage with the UsageEventDisplay projection populated
// (the raw UsageEvent[] carrying oneof details is not surfaced — a
// 17-way discriminated union whose leaves shift release-to-release,
// and every field cursor2api's UI wants is already on the Display
// side. Add a raw-events accessor here if a caller ever needs it.)
func (c *Client) ListEvents(ctx context.Context, opts EventListOptions) (*EventPage, error) {
	req := &usagepb.GetFilteredUsageEventsRequest{}
	if opts.StartMs > 0 {
		s := opts.StartMs
		req.StartDate = &s
	}
	if opts.EndMs > 0 {
		e := opts.EndMs
		req.EndDate = &e
	}
	if opts.Model != "" {
		m := opts.Model
		req.ModelId = &m
	}
	if opts.Page > 0 {
		p := opts.Page
		req.Page = &p
	}
	if opts.PageSize > 0 {
		ps := opts.PageSize
		req.PageSize = &ps
	}

	resp := &usagepb.GetFilteredUsageEventsResponse{}
	if err := c.call(ctx, "aiserver.v1.DashboardService", "GetFilteredUsageEvents", req, resp); err != nil {
		return nil, err
	}
	return &EventPage{
		Events:     resp.GetUsageEventsDisplay(),
		TotalCount: resp.GetTotalUsageEventsCount(),
	}, nil
}

// call is a thin wrapper around executor.Client.UnaryCall that classifies
// permission_denied errors so callers can distinguish them from transport
// failures. The error is left unchanged; only the message is used for the
// classification helper below.
func (c *Client) call(ctx context.Context, service, method string, req, resp proto.Message) error {
	// The executor.Client uses net/http directly with no context, so ctx is
	// currently only used for cancellation of retries at a higher level.
	// Preserving ctx in the signature keeps room for future context-aware
	// implementations without an API break.
	_ = ctx
	return c.Exec.UnaryCall(service, method, req, resp)
}

// IsPermissionDenied returns true if the error text matches Cursor's typical
// permission-denied response (team_scope endpoints for personal-plan users).
func IsPermissionDenied(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "permission_denied") ||
		strings.Contains(s, "permission denied") ||
		strings.Contains(s, "http 403")
}

// FormatCents renders a cents-int as a $x.xx string.
func FormatCents(cents int64) string {
	sign := ""
	if cents < 0 {
		sign = "-"
		cents = -cents
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}
