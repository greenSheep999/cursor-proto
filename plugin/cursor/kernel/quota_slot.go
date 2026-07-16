// quota_slot.go — plugin-quota-slot manifest + /quota-rows handler.
//
// The CPA /quota page is currently a hardcoded list of five built-in
// providers (Antigravity, Claude, Codex, Kimi, xAI). The plugin-quota
// slot proposal (docs/plugin-quota-slot-design.md) lets a plugin
// declare itself as an additional card on that page purely over the
// wire — CPA's frontend renders our data with its own generic Grid
// component, no per-plugin frontend code.
//
// This file contributes two things:
//
//  1. quotaSlotManifest() — the JSON blob added to
//     managementRegisterResult()'s response under the "quota_slot"
//     key. Describes the card title, data endpoint, columns, and
//     row-level actions.
//
//  2. handleQuotaRows(ctx) — the GET /quota-rows handler that turns
//     every registered AccountStatus into one wire row. The row
//     shape is what the design doc's "Data endpoint response shape"
//     section describes; column keys line up with the manifest.
//
// Zero effect on plugins that predate the slot proposal: an older CPA
// simply ignores the extra quota_slot field.
package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"time"
)

// quotaRow is one row on the panel /quota grid, keyed off account
// email. Field names line up with the column `key`s advertised in
// quotaSlotManifest(). Zero values are legitimate — the frontend
// decides how to format them per column type.
type quotaRow struct {
	ID           string  `json:"id"`
	Email        string  `json:"email"`
	Plan         string  `json:"plan,omitempty"`
	Country      string  `json:"country,omitempty"`
	SpendCents   int64   `json:"spend_cents"`
	LimitCents   int64   `json:"limit_cents"`
	TotalPercent float64 `json:"total_percent"`
	AutoPercent  float64 `json:"auto_percent"`
	APIPercent   float64 `json:"api_percent"`
	Spend24h     int64   `json:"spend_24h_cents"`
	Spend7d      int64   `json:"spend_7d_cents"`
	Spend30d     int64   `json:"spend_30d_cents"`
	// CacheRead7d and CacheWrite7d surface Cursor's cache-token
	// economics on the panel — cache_read is effectively free,
	// cache_write is billable at a reduced rate. Both meaningful
	// for cost-per-account inspection at a glance.
	CacheRead7d  int64  `json:"cache_read_7d"`
	CacheWrite7d int64  `json:"cache_write_7d"`
	InSlowPool   bool   `json:"in_slow_pool"`
	ResetAt      string `json:"reset_at,omitempty"` // RFC3339
	FetchedAt    string `json:"fetched_at,omitempty"`

	// Panel-standard status fields. "ok" renders normally; "error"
	// dims the row and shows _error / _error_status. See the
	// design doc's Data endpoint response shape section.
	Status      string `json:"_status,omitempty"`
	Error       string `json:"_error,omitempty"`
	ErrorStatus int    `json:"_error_status,omitempty"`
}

// quotaRowsResponse is the JSON body of /quota-rows.
type quotaRowsResponse struct {
	Rows      []quotaRow `json:"rows"`
	FetchedAt time.Time  `json:"fetched_at"`
	Count     int        `json:"count"`
}

// handleQuotaRows serves GET /quota-rows. Reuses the same
// authRegistry cache as /accounts so a panel refresh doesn't
// re-hit Cursor's API for every account when the /quota page and
// the /plugin-pages/cursor page render side by side.
func handleQuotaRows(ctx context.Context) managementResponse {
	accs := globalRegistry.List()
	rows := make([]quotaRow, 0, len(accs))
	for _, a := range accs {
		s, err := globalRegistry.Status(ctx, a.Email, false)
		if s == nil {
			s = &AccountStatus{Email: a.Email}
		}
		row := rowFromStatus(s)
		if err != nil {
			// Only mark as error when we have literally no data;
			// partial-data + upstream error is still useful to see.
			if s.Plan == "" && s.SpendCents == 0 && s.LimitCents == 0 {
				row.Status = "error"
				row.Error = err.Error()
			}
		}
		rows = append(rows, row)
	}
	body := quotaRowsResponse{
		Rows:      rows,
		FetchedAt: time.Now().UTC(),
		Count:     len(rows),
	}
	return jsonResponse(http.StatusOK, body)
}

// rowFromStatus projects an AccountStatus into the panel wire shape.
// Kept as a pure function so tests can lock down the mapping without
// standing up the registry.
func rowFromStatus(s *AccountStatus) quotaRow {
	row := quotaRow{
		ID:           s.Email,
		Email:        s.Email,
		Plan:         s.Plan,
		Country:      s.Country,
		SpendCents:   s.SpendCents,
		LimitCents:   s.LimitCents,
		TotalPercent: s.TotalPercentUsed,
		AutoPercent:  s.AutoPercentUsed,
		APIPercent:   s.APIPercentUsed,
		Spend24h:     s.Spend24hCents,
		Spend7d:      s.Spend7dCents,
		Spend30d:     s.Spend30dCents,
		CacheRead7d:  s.Tokens7d.CacheRead,
		CacheWrite7d: s.Tokens7d.CacheWrite,
		InSlowPool:   s.InSlowPool,
	}
	if !s.RateLimitResetAt.IsZero() {
		row.ResetAt = s.RateLimitResetAt.UTC().Format(time.RFC3339)
	}
	if !s.FetchedAt.IsZero() {
		row.FetchedAt = s.FetchedAt.UTC().Format(time.RFC3339)
	}
	if s.LastErrorCode != "" && s.Plan == "" {
		row.Status = "error"
		row.Error = s.LastErrorCode
	}
	return row
}

// quotaSlotManifest builds the "quota_slot" JSON injected into
// managementRegisterResult(). Any CPA that predates the slot proposal
// treats this field as unknown and ignores it — fully backward
// compatible.
//
// Column set matches what the panel can meaningfully show at a
// glance: identifier + tags + progress bars + reset time. Deeper
// fields (per-model spend, per-request events) stay on the
// plugin-specific /plugin-pages/cursor page.
func quotaSlotManifest() map[string]any {
	return map[string]any{
		"id":                   pluginName,
		"title_i18n_key":       "quota.cursor.title",
		"title_fallback":       "Cursor accounts",
		"description_i18n_key": "quota.cursor.description",
		"description_fallback": "Cursor accounts — plan, categorised spend (total / auto+composer / API), and 7-day cache-token usage. Refreshed from Cursor's usage API on the same 30-second cache as the plugin page.",
		"data_path":            managementBasePath + routePrefix + "/quota-rows",
		"columns": []map[string]any{
			{"key": "email", "label": "Account", "type": "identifier"},
			{"key": "plan", "label": "Plan", "type": "tag"},
			{"key": "country", "label": "Region", "type": "tag"},
			{"key": "spend_cents", "label": "Spend", "type": "cents"},
			{"key": "limit_cents", "label": "Limit", "type": "cents"},
			{"key": "total_percent", "label": "Total used", "type": "percent_bar"},
			{"key": "auto_percent", "label": "Auto+Composer", "type": "percent_bar"},
			{"key": "api_percent", "label": "API", "type": "percent_bar"},
			{"key": "cache_read_7d", "label": "Cache-read 7d", "type": "int", "hint": "tokens"},
			{"key": "cache_write_7d", "label": "Cache-write 7d", "type": "int", "hint": "tokens"},
			{"key": "in_slow_pool", "label": "Slow pool", "type": "boolean"},
			{"key": "reset_at", "label": "Reset", "type": "iso8601"},
		},
		"row_actions": []map[string]any{
			{
				"id":          "probe",
				"label":       "Refresh account",
				"method":      "POST",
				"path":        managementBasePath + routePrefix + "/account/probe",
				"email_param": "email",
			},
			{
				"id":          "events",
				"label":       "Per-request log",
				"method":      "GET",
				"path":        managementBasePath + routePrefix + "/account/events",
				"email_param": "email",
				"target":      "modal",
			},
		},
	}
}

// mustMarshalQuotaSlot returns the manifest as raw JSON, panicking on
// impossible encode failures (map[string]any of strings/numbers/nested
// maps has no code path to encoding errors). Used to embed the slot
// into managementRegisterResult() without polluting that function
// with error plumbing.
func mustMarshalQuotaSlot() json.RawMessage {
	buf, err := json.Marshal(quotaSlotManifest())
	if err != nil {
		// Truly unreachable: only static string/number/map data.
		panic("quota_slot manifest marshal: " + err.Error())
	}
	return buf
}
