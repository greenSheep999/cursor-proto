package main

import (
	"testing"
	"time"
)

func TestClassifyTool(t *testing.T) {
	cases := []struct {
		name       string
		wantKind   toolKind
		wantServer string
	}{
		// Canonical MCP naming (Claude Code default).
		{"mcp__filesystem__read_file", toolKindMCP, "filesystem"},
		{"mcp__github__create_issue", toolKindMCP, "github"},
		{"mcp__slack__post_message", toolKindMCP, "slack"},

		// MCP with a single-segment tail: still MCP; server = tail.
		{"mcp__filesystem", toolKindMCP, "filesystem"},

		// Loose form: mcp_<server>_<tool> (aider variant).
		{"mcp_filesystem_read", toolKindMCP, "filesystem"},

		// Custom tools — anything without an mcp prefix.
		{"Bash", toolKindCustom, ""},
		{"get_weather", toolKindCustom, ""},
		{"Read", toolKindCustom, ""},

		// Ambiguous names that LOOK like the loose form but should
		// stay classified as custom. We ruled out these when the
		// candidate server contains dots/slashes.
		{"mcp_upload_v2", toolKindMCP, "upload"}, // still MCP under loose rule; documented behavior
		{"mcp_./bad_thing", toolKindCustom, ""},  // dots in server → not MCP

		// Empty edge cases.
		{"", toolKindCustom, ""},
		{"mcp", toolKindCustom, ""},
		{"mcp_", toolKindCustom, ""},
		{"mcp__", toolKindMCP, ""}, // ambiguous — MCP prefix with empty rest, we accept it
	}
	for _, c := range cases {
		gotKind, gotServer := classifyTool(c.name)
		if gotKind != c.wantKind || gotServer != c.wantServer {
			t.Errorf("classifyTool(%q) = (%v,%q), want (%v,%q)",
				c.name, gotKind, gotServer, c.wantKind, c.wantServer)
		}
	}
}

func TestRingRecordAndSnapshot(t *testing.T) {
	r := newToolRing(8)
	now := time.Now()

	// Batch 1: two tools, 30s ago.
	r.record([]string{"Bash", "mcp__filesystem__read_file"}, now.Add(-30*time.Second))
	// Batch 2: three tools, 10s ago.
	r.record([]string{"Read", "Grep", "mcp__github__create_issue"}, now.Add(-10*time.Second))

	// A 60s window should return all 5 observations, newest first.
	obs := r.snapshotSince(now.Add(-60 * time.Second))
	if len(obs) != 5 {
		t.Fatalf("got %d observations, want 5", len(obs))
	}
	if obs[0].name != "mcp__github__create_issue" {
		t.Errorf("newest observation should be mcp__github__create_issue, got %s", obs[0].name)
	}
	// Newest-first ordering.
	for i := 1; i < len(obs); i++ {
		if obs[i].seenAt.After(obs[i-1].seenAt) {
			t.Errorf("observations not newest-first at index %d", i)
		}
	}

	// A 15s window should only see the second batch (3 tools).
	obs = r.snapshotSince(now.Add(-15 * time.Second))
	if len(obs) != 3 {
		t.Errorf("15s window: got %d, want 3", len(obs))
	}

	// A 0-second window should return nothing.
	obs = r.snapshotSince(now.Add(1 * time.Second))
	if len(obs) != 0 {
		t.Errorf("future window: got %d, want 0", len(obs))
	}
}

func TestRingWrapAround(t *testing.T) {
	// Ring of 3. Record 7 observations. Only the last 3 should survive.
	r := newToolRing(3)
	base := time.Now()
	for i := 0; i < 7; i++ {
		r.record([]string{"tool_" + string(rune('A'+i))}, base.Add(time.Duration(i)*time.Second))
	}

	obs := r.snapshotSince(time.Time{})
	if len(obs) != 3 {
		t.Fatalf("wrap-around retained %d observations, want 3 (buffer size)", len(obs))
	}
	// The three surviving should be the newest three: tool_G, tool_F, tool_E.
	want := []string{"tool_G", "tool_F", "tool_E"}
	for i, w := range want {
		if obs[i].name != w {
			t.Errorf("obs[%d] = %s, want %s", i, obs[i].name, w)
		}
	}
}

func TestRingIgnoresEmptyBatchAndBlankNames(t *testing.T) {
	r := newToolRing(8)
	r.record(nil, time.Now())
	r.record([]string{}, time.Now())
	r.record([]string{"", "  ", "\t"}, time.Now())
	obs := r.snapshotSince(time.Time{})
	if len(obs) != 0 {
		t.Errorf("empty/whitespace inputs recorded %d entries", len(obs))
	}
}
