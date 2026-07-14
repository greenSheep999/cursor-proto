package main

// usage_handler.go exposes the Cursor account usage/quota snapshot as HTTP.
//
// Endpoints (registered in main.go alongside /v1/models):
//
//	GET  /v1/usage             JSON snapshot (see usage.Snapshot)
//	GET  /v1/usage/prometheus  Prometheus-style metrics
//
// The handlers reuse the proxy's already-authenticated executor.Client, so
// no additional auth material is needed.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/usage"
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
