// handleAccountEvents backs
// GET /v0/management/cli-proxy-api/cursor/account/events. Same shape
// as cursor-proxy's GET /v1/usage/events (introduced in
// cursor3.11/v0.3.1), scoped to one account looked up by ?email=.
//
// Why a plugin route instead of proxying to /v1/usage/events:
// CPA's admin panel talks to the plugin via ABI, not HTTP, so it
// can't reach the cursor-proxy binary. This handler goes straight
// to usage.Client.ListEvents against the account's own Cursor
// credentials — exactly what cursor-proxy would have done.

package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/usage"
	usagepb "github.com/router-for-me/cursor-proto/usage/pb"
)

// Query-parameter defaults / caps, kept in lockstep with
// cmd/cursor-proxy/usage_handler.go so the two surfaces behave
// identically from the caller's perspective.
const (
	defaultEventsSince    = 24 * time.Hour
	maxEventsSince        = 30 * 24 * time.Hour
	defaultEventsPageSize = 100
	maxEventsPageSize     = 500
	maxEventsPage         = 10000
)

// UsageEventItem is the flattened, snake_case JSON row we hand
// back to CPA's admin panel. Identical to cursor-proxy's
// usageEventItem — kept as a separate type here so plugin
// consumers don't have to depend on cmd/cursor-proxy.
type UsageEventItem struct {
	Timestamp       string       `json:"timestamp"` // ISO 8601
	TimestampMs     int64        `json:"timestamp_ms"`
	Model           string       `json:"model"`
	Kind            string       `json:"kind"` // enum name minus prefix
	MaxMode         bool         `json:"max_mode,omitempty"`
	RequestsCosts   float64      `json:"requests_costs,omitempty"`
	UsageBasedCosts string       `json:"usage_based_costs,omitempty"`
	IsTokenBased    bool         `json:"is_token_based,omitempty"`
	Tokens          *EventTokens `json:"tokens,omitempty"`
	ChargedCents    float64      `json:"charged_cents,omitempty"`
	IsChargeable    bool         `json:"is_chargeable,omitempty"`
	ConversationID  string       `json:"conversation_id,omitempty"`
	CloudAgentID    string       `json:"cloud_agent_id,omitempty"`
	AutomationID    string       `json:"automation_id,omitempty"`
	ClientType      string       `json:"client_type,omitempty"`
	IsHeadless      bool         `json:"is_headless,omitempty"`
	UserEmail       string       `json:"user_email,omitempty"`
	ServiceAccount  string       `json:"service_account_name,omitempty"`
}

type EventTokens struct {
	Input      int32   `json:"input"`
	Output     int32   `json:"output"`
	CacheRead  int32   `json:"cache_read"`
	CacheWrite int32   `json:"cache_write"`
	TotalCents float64 `json:"total_cents"`
}

// UsageEventsResponse is the JSON body of /account/events.
type UsageEventsResponse struct {
	Email        string           `json:"email"`
	Events       []UsageEventItem `json:"events"`
	Page         int32            `json:"page"`
	PageSize     int32            `json:"page_size"`
	TotalCount   int32            `json:"total_count"`
	HasNext      bool             `json:"has_next"`
	SinceSeconds float64          `json:"since_seconds"`
}

// handleAccountEvents dispatches from routeManagement. Query
// parsing is intentionally forgiving — garbage values fall back
// to safe defaults rather than 400, matching cursor-proxy's own
// /v1/usage/events behaviour.
func handleAccountEvents(ctx context.Context, q url.Values) managementResponse {
	email := strings.TrimSpace(q.Get("email"))
	if email == "" {
		return jsonErrorResponse(http.StatusBadRequest, "missing_email", "?email= is required")
	}
	acc, ok := globalRegistry.Get(email)
	if !ok {
		return jsonErrorResponse(http.StatusNotFound, "unknown_account",
			fmt.Sprintf("no cursor account tracked for %s", email))
	}

	since := parseEventsSince(q.Get("since"))
	page := parseIntClamped(q.Get("page"), 1, maxEventsPage)
	pageSize := parseIntClamped(q.Get("limit"), defaultEventsPageSize, maxEventsPageSize)
	model := strings.TrimSpace(q.Get("model"))

	now := time.Now()
	start := now.Add(-since)

	exec := executor.NewClient(acc)
	client := usage.New(exec)
	result, err := client.ListEvents(ctx, usage.EventListOptions{
		StartMs:  start.UnixMilli(),
		EndMs:    now.UnixMilli(),
		Model:    model,
		Page:     page,
		PageSize: pageSize,
	})
	if err != nil {
		if usage.IsPermissionDenied(err) {
			return jsonErrorResponse(http.StatusForbidden, "permission_denied", err.Error())
		}
		return jsonErrorResponse(http.StatusBadGateway, "fetch_failed", err.Error())
	}

	items := make([]UsageEventItem, 0, len(result.Events))
	for _, ev := range result.Events {
		items = append(items, projectEvent(ev))
	}
	resp := UsageEventsResponse{
		Email:        email,
		Events:       items,
		Page:         page,
		PageSize:     pageSize,
		TotalCount:   result.TotalCount,
		HasNext:      int64(page)*int64(pageSize) < int64(result.TotalCount),
		SinceSeconds: since.Seconds(),
	}
	return jsonResponse(http.StatusOK, resp)
}

// projectEvent flattens a UsageEventDisplay into the panel-friendly
// UsageEventItem. Kind is stripped of its USAGE_EVENT_KIND_ prefix
// so downstream sees "USAGE_BASED" / "INCLUDED_IN_PRO" / etc.
func projectEvent(ev *usagepb.UsageEventDisplay) UsageEventItem {
	ts := ev.GetTimestamp()
	item := UsageEventItem{
		Timestamp:       time.UnixMilli(ts).UTC().Format(time.RFC3339Nano),
		TimestampMs:     ts,
		Model:           ev.GetModel(),
		Kind:            strings.TrimPrefix(ev.GetKind().String(), "USAGE_EVENT_KIND_"),
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
		item.Tokens = &EventTokens{
			Input:      tu.GetInputTokens(),
			Output:     tu.GetOutputTokens(),
			CacheRead:  tu.GetCacheReadTokens(),
			CacheWrite: tu.GetCacheWriteTokens(),
			TotalCents: float64(tu.GetTotalCents()),
		}
	}
	return item
}

// parseEventsSince accepts either a Go duration ("24h", "5m") or a
// bare integer of seconds. Empty / bogus → default; overflow → cap.
// Same semantics as cmd/cursor-proxy/usage_handler.go's parser.
func parseEventsSince(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultEventsSince
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		if d > maxEventsSince {
			return maxEventsSince
		}
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		d := time.Duration(n) * time.Second
		if d > maxEventsSince {
			return maxEventsSince
		}
		return d
	}
	return defaultEventsSince
}

// parseIntClamped parses raw as an int, defaulting to def when
// empty/bogus, capping at cap otherwise.
func parseIntClamped(raw string, def, cap int) int32 {
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

// Silence unused-import checks for encoding/json — it's used
// indirectly via jsonResponse in the shared helpers.
var _ = json.Marshal
