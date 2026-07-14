package usage

import (
	"context"
	"testing"

	usagepb "github.com/router-for-me/cursor-proto/usage/pb"
	"google.golang.org/protobuf/proto"
)

// TestListEvents_HappyPath exercises the RPC glue against a fake
// GetFilteredUsageEventsResponse and checks:
//   - The Display projection flows through unchanged (fields, count).
//   - The TotalCount echo is passed to the caller as-is.
func TestListEvents_HappyPath(t *testing.T) {
	routes := map[string]proto.Message{
		"aiserver.v1.DashboardService/GetFilteredUsageEvents": &usagepb.GetFilteredUsageEventsResponse{
			TotalUsageEventsCount: 137,
			UsageEventsDisplay: []*usagepb.UsageEventDisplay{
				{
					Timestamp: 1783000000000,
					Model:     "claude-sonnet-4-5-20250929",
					Kind:      usagepb.UsageEventKind_USAGE_EVENT_KIND_USAGE_BASED,
					MaxMode:   true,
					TokenUsage: &usagepb.TokenUsage{
						InputTokens:      10240,
						OutputTokens:     1024,
						CacheReadTokens:  4096,
						CacheWriteTokens: 2048,
						TotalCents:       12,
					},
				},
				{
					Timestamp: 1783000060000,
					Model:     "cursor-grok-4.5-low-fast",
					Kind:      usagepb.UsageEventKind_USAGE_EVENT_KIND_INCLUDED_IN_PRO,
				},
			},
		},
	}
	srv := newFakeServer(t, routes, nil)
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.ListEvents(context.Background(), EventListOptions{
		StartMs:  1782900000000,
		EndMs:    1783100000000,
		Page:     1,
		PageSize: 50,
	})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if page.TotalCount != 137 {
		t.Errorf("total count: got %d want 137", page.TotalCount)
	}
	if len(page.Events) != 2 {
		t.Fatalf("events count: got %d want 2", len(page.Events))
	}
	if page.Events[0].GetModel() != "claude-sonnet-4-5-20250929" {
		t.Errorf("event[0] model: got %q", page.Events[0].GetModel())
	}
	if !page.Events[0].GetMaxMode() {
		t.Error("event[0] max_mode should be true")
	}
	if got := page.Events[0].GetTokenUsage().GetInputTokens(); got != 10240 {
		t.Errorf("event[0] input tokens: got %d want 10240", got)
	}
	if page.Events[1].GetKind() != usagepb.UsageEventKind_USAGE_EVENT_KIND_INCLUDED_IN_PRO {
		t.Errorf("event[1] kind: got %v", page.Events[1].GetKind())
	}
}

// TestListEvents_EmptyResponseIsOK verifies zero-events response
// deserializes cleanly and returns a well-formed empty page rather
// than nil.
func TestListEvents_EmptyResponseIsOK(t *testing.T) {
	routes := map[string]proto.Message{
		"aiserver.v1.DashboardService/GetFilteredUsageEvents": &usagepb.GetFilteredUsageEventsResponse{
			TotalUsageEventsCount: 0,
		},
	}
	srv := newFakeServer(t, routes, nil)
	defer srv.Close()

	c := newTestClient(srv.URL)
	page, err := c.ListEvents(context.Background(), EventListOptions{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if page.TotalCount != 0 {
		t.Errorf("total: got %d", page.TotalCount)
	}
	if len(page.Events) != 0 {
		t.Errorf("events: got %d", len(page.Events))
	}
}

// TestListEvents_OptionalFieldsOmitted verifies the caller can leave
// filter fields unset and the request goes through with pb3 optional
// semantics (no bogus zero-value filters land server-side).
func TestListEvents_OptionalFieldsOmitted(t *testing.T) {
	routes := map[string]proto.Message{
		"aiserver.v1.DashboardService/GetFilteredUsageEvents": &usagepb.GetFilteredUsageEventsResponse{},
	}
	srv := newFakeServer(t, routes, nil)
	defer srv.Close()

	c := newTestClient(srv.URL)
	if _, err := c.ListEvents(context.Background(), EventListOptions{}); err != nil {
		t.Fatalf("zero options should be legal: %v", err)
	}
}
