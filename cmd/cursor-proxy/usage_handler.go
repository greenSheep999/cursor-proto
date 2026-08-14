package main

// usage_handler.go exposes the Cursor account usage/quota snapshot as HTTP.
//
// Endpoints (registered in main.go alongside /v1/models):
//
//	GET  /v1/usage             JSON snapshot (see usage.Snapshot)
//	GET  /v1/usage/prometheus  Prometheus-style metrics
//	GET  /v1/usage/events      Per-request event log (paginated)
//
// The handlers reuse the proxy's already-authenticated executor.Client, so
// no additional auth material is needed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/usage"
	usagepb "github.com/router-for-me/cursor-proto/usage/pb"
)

// usageHandler returns a JSON usage.Snapshot for the proxy's account.
func usageHandler(c *executor.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		snap, err := usage.New(c).Fetch(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		w.Header().Set("content-type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(snap)
	}
}

// usagePrometheusHandler emits a small set of Prometheus text-format metrics.
// It is not a full Prom exporter — it's for scraping the proxy from an already
// running Prom instance without pulling in prometheus/client_golang.
func usagePrometheusHandler(c *executor.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()

		snap, err := usage.New(c).Fetch(ctx)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		w.Header().Set("content-type", "text/plain; version=0.0.4")
		writeMetric(w, "cursor_usage_total_spend_cents", "Total spend in the current billing period (cents)", snap.TotalSpend)
		writeMetric(w, "cursor_usage_included_spend_cents", "Included spend in the current billing period (cents)", snap.IncludedSpend)
		writeMetric(w, "cursor_usage_remaining_cents", "Remaining allowance in the current billing period (cents)", snap.Remaining)
		writeMetric(w, "cursor_usage_limit_cents", "Plan limit for the current billing period (cents)", snap.Limit)
		// Categorized spend — mirrors the three progress bars Cursor's own
		// dashboard renders (Total / Auto + Composer / API). Percent values
		// are Cursor's precomputed doubles from PlanUsage, so downstream
		// gauges match the IDE byte-for-byte instead of recomputing.
		writeMetric(w, "cursor_usage_auto_spend_cents", "Auto + Composer spend in the current billing period (cents)", snap.AutoSpend)
		writeMetric(w, "cursor_usage_auto_limit_cents", "Auto + Composer limit in the current billing period (cents)", snap.AutoLimit)
		writeMetric(w, "cursor_usage_api_spend_cents", "API (Claude/GPT/Gemini) spend in the current billing period (cents)", snap.APISpend)
		writeMetric(w, "cursor_usage_api_limit_cents", "API (Claude/GPT/Gemini) limit in the current billing period (cents)", snap.APILimit)
		writeFloatMetric(w, "cursor_usage_auto_percent_used", "Auto + Composer bucket, fraction of limit consumed (0..1+)", snap.AutoPercentUsed)
		writeFloatMetric(w, "cursor_usage_api_percent_used", "API bucket, fraction of limit consumed (0..1+)", snap.APIPercentUsed)
		writeFloatMetric(w, "cursor_usage_total_percent_used", "Total spend, fraction of plan limit consumed (0..1+)", snap.TotalPercentUsed)
		writeMetric(w, "cursor_usage_hard_limit_cents", "Hard $ cap (cents)", snap.HardLimit)
		writeMetric(w, "cursor_usage_spend_24h_cents", "Spend in the last 24 hours (cents)", snap.Spend24h)
		writeMetric(w, "cursor_usage_spend_7d_cents", "Spend in the last 7 days (cents)", snap.Spend7d)
		writeMetric(w, "cursor_usage_spend_30d_cents", "Spend in the last 30 days (cents)", snap.Spend30d)
		// Token breakdown per window — same 4 counters (input, output,
		// cache_read, cache_write) × 3 windows (24h, 7d, 30d). Sourced
		// from GetAggregatedUsageEventsResponse — no extra upstream
		// calls; the same RPC that fills spend also carries these.
		writeMetric(w, "cursor_usage_tokens_input_24h", "Prompt input tokens in the last 24 hours", snap.Tokens24h.Input)
		writeMetric(w, "cursor_usage_tokens_output_24h", "Model output tokens in the last 24 hours", snap.Tokens24h.Output)
		writeMetric(w, "cursor_usage_tokens_cache_read_24h", "Cache-read tokens in the last 24 hours (effectively free)", snap.Tokens24h.CacheRead)
		writeMetric(w, "cursor_usage_tokens_cache_write_24h", "Cache-write tokens in the last 24 hours (billable at reduced rate)", snap.Tokens24h.CacheWrite)
		writeMetric(w, "cursor_usage_tokens_input_7d", "Prompt input tokens in the last 7 days", snap.Tokens7d.Input)
		writeMetric(w, "cursor_usage_tokens_output_7d", "Model output tokens in the last 7 days", snap.Tokens7d.Output)
		writeMetric(w, "cursor_usage_tokens_cache_read_7d", "Cache-read tokens in the last 7 days", snap.Tokens7d.CacheRead)
		writeMetric(w, "cursor_usage_tokens_cache_write_7d", "Cache-write tokens in the last 7 days", snap.Tokens7d.CacheWrite)
		writeMetric(w, "cursor_usage_tokens_input_30d", "Prompt input tokens in the last 30 days", snap.Tokens30d.Input)
		writeMetric(w, "cursor_usage_tokens_output_30d", "Model output tokens in the last 30 days", snap.Tokens30d.Output)
		writeMetric(w, "cursor_usage_tokens_cache_read_30d", "Cache-read tokens in the last 30 days", snap.Tokens30d.CacheRead)
		writeMetric(w, "cursor_usage_tokens_cache_write_30d", "Cache-write tokens in the last 30 days", snap.Tokens30d.CacheWrite)
		writeMetric(w, "cursor_usage_in_slow_pool", "1 if the account is currently in the slow pool", boolInt(snap.InSlowPool))
		writeMetric(w, "cursor_usage_no_usage_based_allowed", "1 if usage-based billing is disallowed for this account", boolInt(snap.NoUsageBasedAllowed))
		writeMetric(w, "cursor_usage_premium_requests_enabled", "1 if usage-based premium requests are enabled", boolInt(snap.UsageBasedPremiumRequestsEnabled))
		writeMetric(w, "cursor_usage_slowness_ms", "Configured slowness in milliseconds when in slow pool", snap.SlownessMs)
		writeMetric(w, "cursor_usage_rate_limit_reset_days_remaining", "Days until short-window rate limit resets", int64(snap.RateLimitResetDaysRemaining))
		if snap.RateLimitResetAt != nil && !snap.RateLimitResetAt.IsZero() {
			writeMetric(w, "cursor_usage_rate_limit_reset_at_seconds", "Unix seconds when the short-window rate limit resets", snap.RateLimitResetAt.Unix())
		}
		if !snap.PeriodStart.IsZero() {
			writeMetric(w, "cursor_usage_period_start_seconds", "Unix seconds of the current billing period start", snap.PeriodStart.Unix())
		}
		if !snap.PeriodEnd.IsZero() {
			writeMetric(w, "cursor_usage_period_end_seconds", "Unix seconds of the current billing period end", snap.PeriodEnd.Unix())
		}
	}
}

func writeMetric(w http.ResponseWriter, name, help string, value int64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s gauge\n", name)
	fmt.Fprintf(w, "%s %d\n", name, value)
}

// writeFloatMetric emits a Prometheus gauge with a float value — used for
// the percent-used fields Cursor's backend hands us as doubles.
func writeFloatMetric(w http.ResponseWriter, name, help string, value float64) {
	fmt.Fprintf(w, "# HELP %s %s\n", name, help)
	fmt.Fprintf(w, "# TYPE %s gauge\n", name)
	fmt.Fprintf(w, "%s %g\n", name, value)
}

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// -------- /v1/usage/events --------
//
// Per-request event log. Cursor's dashboard shows this as the
// "Requests" table below the aggregate cards; cursor2api's /usage
// page mirrors it. Paginated because a busy account produces
// >1k events/day.
//
// Query parameters:
//
//	?since=<duration>   Go duration ("24h", "7d", "30d"). Default 24h.
//	?limit=<int>        Page size. Default 100, max 500.
//	?page=<int>         1-based page number. Default 1.
//	?model=<string>     Exact model_id filter. Empty means all.

const (
	defaultEventsSinceWindow = 24 * time.Hour
	maxEventsSinceWindow     = 30 * 24 * time.Hour
	defaultEventsPageSize    = 100
	maxEventsPageSize        = 500
)

// usageEventItem is the HTTP-facing projection of one UsageEventDisplay.
// snake_case matches the rest of /v1/usage/*; the raw pb.go type is
// camelCase (Go convention) so we spell fields out explicitly.
type usageEventItem struct {
	Timestamp       string    `json:"timestamp"`    // ISO 8601
	TimestampMs     int64     `json:"timestamp_ms"` // raw for sorting
	Model           string    `json:"model"`
	Kind            string    `json:"kind"` // enum name minus prefix
	MaxMode         bool      `json:"max_mode,omitempty"`
	RequestsCosts   float64   `json:"requests_costs,omitempty"`
	UsageBasedCosts string    `json:"usage_based_costs,omitempty"`
	IsTokenBased    bool      `json:"is_token_based,omitempty"`
	Tokens          *tokenBrk `json:"tokens,omitempty"`
	ChargedCents    float64   `json:"charged_cents,omitempty"`
	IsChargeable    bool      `json:"is_chargeable,omitempty"`
	ConversationID  string    `json:"conversation_id,omitempty"`
	CloudAgentID    string    `json:"cloud_agent_id,omitempty"`
	AutomationID    string    `json:"automation_id,omitempty"`
	ClientType      string    `json:"client_type,omitempty"`
	IsHeadless      bool      `json:"is_headless,omitempty"`
	UserEmail       string    `json:"user_email,omitempty"`
	ServiceAccount  string    `json:"service_account_name,omitempty"`
}

type tokenBrk struct {
	Input      int32   `json:"input"`
	Output     int32   `json:"output"`
	CacheRead  int32   `json:"cache_read"`
	CacheWrite int32   `json:"cache_write"`
	TotalCents float64 `json:"total_cents"`
}

// usageEventsResponse is the JSON body of GET /v1/usage/events.
type usageEventsResponse struct {
	Events       []usageEventItem `json:"events"`
	Page         int32            `json:"page"`
	PageSize     int32            `json:"page_size"`
	TotalCount   int32            `json:"total_count"`
	HasNext      bool             `json:"has_next"`
	SinceSeconds float64          `json:"since_seconds"`
}

// kindName strips the USAGE_EVENT_KIND_ prefix so downstream can render
// "USAGE_BASED" / "INCLUDED_IN_PRO" / etc. directly. Full enum name
// stays reachable via the raw proto if a caller ever needs it.
func kindName(k usagepb.UsageEventKind) string {
	name := k.String()
	return strings.TrimPrefix(name, "USAGE_EVENT_KIND_")
}

func projectEvent(ev *usagepb.UsageEventDisplay) usageEventItem {
	ts := ev.GetTimestamp()
	item := usageEventItem{
		Timestamp:       time.UnixMilli(ts).UTC().Format(time.RFC3339Nano),
		TimestampMs:     ts,
		Model:           ev.GetModel(),
		Kind:            kindName(ev.GetKind()),
		MaxMode:         ev.GetMaxMode(),
		RequestsCosts:   float64(ev.GetRequestsCosts()),
		UsageBasedCosts: ev.GetUsageBasedCosts(),
		IsTokenBased:    ev.GetIsTokenBasedCall(),
		IsChargeable:    ev.GetIsChargeable(),
		ChargedCents:    float64(ev.GetChargedCents()),
		ConversationID:  ev.GetConversationId(),
		CloudAgentID:    ev.GetCloudAgentId(),
		AutomationID:    ev.GetAutomationId(),
		ClientType:      ev.GetClientType(),
		IsHeadless:      ev.GetIsHeadless(),
		UserEmail:       ev.GetUserEmail(),
		ServiceAccount:  ev.GetServiceAccountName(),
	}
	if tu := ev.GetTokenUsage(); tu != nil {
		item.Tokens = &tokenBrk{
			Input:      tu.GetInputTokens(),
			Output:     tu.GetOutputTokens(),
			CacheRead:  tu.GetCacheReadTokens(),
			CacheWrite: tu.GetCacheWriteTokens(),
			TotalCents: float64(tu.GetTotalCents()),
		}
	}
	return item
}

// parseEventsSince accepts a Go duration string or raw seconds and
// clamps to [1s, maxEventsSinceWindow]. Garbage / empty → default.
func parseEventsSince(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultEventsSinceWindow
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		if n, ierr := strconv.Atoi(raw); ierr == nil && n > 0 {
			d = time.Duration(n) * time.Second
		} else {
			return defaultEventsSinceWindow
		}
	}
	if d <= 0 {
		return defaultEventsSinceWindow
	}
	if d > maxEventsSinceWindow {
		d = maxEventsSinceWindow
	}
	return d
}

func parseEventsInt(raw string, def, cap int) int32 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return int32(def)
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return int32(def)
	}
	if n > cap {
		n = cap
	}
	return int32(n)
}

func usageEventsHandler(c *executor.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		since := parseEventsSince(q.Get("since"))
		page := parseEventsInt(q.Get("page"), 1, 10000)
		pageSize := parseEventsInt(q.Get("limit"), defaultEventsPageSize, maxEventsPageSize)
		model := strings.TrimSpace(q.Get("model"))

		now := time.Now()
		start := now.Add(-since)

		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()

		client := usage.New(c)
		result, err := client.ListEvents(ctx, usage.EventListOptions{
			StartMs:  start.UnixMilli(),
			EndMs:    now.UnixMilli(),
			Model:    model,
			Page:     page,
			PageSize: pageSize,
		})
		if err != nil {
			// Permission-denied usually means the account can't call
			// this RPC on its plan; surface as 502 with the message so
			// downstream can render a helpful error card. Other errors
			// are transport failures.
			status := http.StatusBadGateway
			if usage.IsPermissionDenied(err) {
				status = http.StatusForbidden
			}
			http.Error(w, err.Error(), status)
			return
		}

		items := make([]usageEventItem, 0, len(result.Events))
		for _, ev := range result.Events {
			items = append(items, projectEvent(ev))
		}
		resp := usageEventsResponse{
			Events:       items,
			Page:         page,
			PageSize:     pageSize,
			TotalCount:   result.TotalCount,
			HasNext:      int64(page)*int64(pageSize) < int64(result.TotalCount),
			SinceSeconds: since.Seconds(),
		}
		w.Header().Set("content-type", "application/json")
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(resp)
	}
}
