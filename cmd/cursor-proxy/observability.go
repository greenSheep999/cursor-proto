package main

// /v1/capabilities and /v1/introspect/* endpoints.
//
// Both bypass -api-keys auth (metadata + observability, no secrets in
// the response). See auth_middleware.go for the bypass list — this
// file adds the paths there.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// -------- shared package-level state --------

// observedTools is the single ring buffer populated by every chat
// handler. Package-level so recordToolsFromRequest() can be called
// without threading a pointer through the whole request path.
var observedTools = newToolRing(4096)

// recordToolsFromRequest is the observation entry point. Chat handlers
// call this with the list of tool names they extracted from the
// incoming request body. It's a no-op if names is empty — most chat
// requests don't declare tools.
func recordToolsFromRequest(names []string) {
	if len(names) == 0 {
		return
	}
	observedTools.record(names, time.Now())
}

// -------- capabilities handler --------

// Capabilities describes the protocol features this proxy supports on
// the /v1/messages and /v1/chat/completions endpoints. Values here
// are compile-time facts about this build — they change when the
// codebase changes, not per-request. cursor2api reads this once at
// sidecar spawn to decide which UI toggles to show.
//
// JSON shape is intentionally flat (booleans + small sub-objects) so
// downstream can render it as a checkbox list without post-processing.
type Capabilities struct {
	// Streaming: /v1/messages and /v1/chat/completions both support
	// SSE. False would mean this proxy is a batch-only build (we
	// have never shipped one).
	Streaming bool `json:"streaming"`

	// ToolUseJSONInput: tool_use.input is delivered as a valid JSON
	// object. Was false before v0.2.3 (protobuf debug bytes leaked
	// through); permanently true from v0.2.3 onward.
	ToolUseJSONInput bool `json:"tool_use_json_input"`

	// MultiTurnToolLoop: /v1/messages accepts assistant tool_use +
	// user tool_result in history[] and threads them into a coherent
	// conversation. True since v0.2.3.
	MultiTurnToolLoop bool `json:"multi_turn_tool_loop"`

	// Thinking: Extended Thinking content blocks are emitted for
	// models that support reasoning. True since v0.2.2.
	Thinking bool `json:"thinking"`

	// PromptCaching describes cache_control / cache_read_input_tokens
	// / cache_creation_input_tokens support on requests and responses.
	PromptCaching CapPromptCaching `json:"prompt_caching"`

	// ServerTools: whether supported Anthropic server-side tools are mapped
	// onto Cursor's native agent tools. WebSearch and WebFetch are supported;
	// unrelated Anthropic-hosted tools still return a clear HTTP 400.
	ServerTools bool `json:"server_tools"`

	// MCPTools: whether tool names using the mcp__server__tool
	// convention flow through unchanged. True — Cursor backend
	// accepts arbitrary tool names, we don't rewrite them.
	MCPTools bool `json:"mcp_tools"`

	// EffortMapping: whether Anthropic's output_config.effort and
	// thinking.budget_tokens map onto Cursor's model tier suffixes
	// (-low/-medium/-high). True since v0.2.2.
	EffortMapping bool `json:"effort_mapping"`

	// AnthropicModelAliases: whether canonical Anthropic bare names
	// (claude-sonnet-4-5-20250929 etc.) are rewritten to Cursor's
	// tier-suffixed names on the way through. True since v0.2.2.
	AnthropicModelAliases bool `json:"anthropic_model_aliases"`

	// HTTPVersion: which upstream wire versions this build can pin
	// to. Present since v0.2.5.
	HTTPVersion []string `json:"http_version_options"`

	// Agents: describes /v1/agents/* availability and the sub-
	// features it enables. `supported: false` means this build was
	// deployed without agent mode; the sub-flags are then all false
	// so downstream can check one boolean.
	Agents CapAgents `json:"agents"`
}

// CapAgents groups the agent-mode feature flags. When Supported is
// false the sub-flags are all false regardless of what the code
// COULD do — cursor2api treats Supported as the master switch.
type CapAgents struct {
	// Supported: this cursor-proxy has agent mode wired at build
	// time AND (crucially) has a live Node runner running now. If
	// the operator turned off agent mode via missing -cursor-api-key
	// or -node-runner, this reflects reality, not build capability.
	Supported bool `json:"supported"`

	// Runtimes: local + cloud, per the SDK. Empty when Supported is false.
	Runtimes []string `json:"runtimes"`

	// MCPManagement: whether MCP servers can be configured via
	// agent-config.yaml. False in the Phase 3 MVP; landing later.
	MCPManagement bool `json:"mcp_management"`

	// Skills / Hooks / Subagents / Artifacts: SDK features that the
	// agent runtime honors. All follow-on-Phase work; false today.
	Skills    bool `json:"skills"`
	Hooks     bool `json:"hooks"`
	Subagents bool `json:"subagents"`
	Artifacts bool `json:"artifacts"`
}

// CapPromptCaching splits prompt-cache support into "we surface
// upstream numbers" vs "we simulate cache reads locally". Keeps the
// UI honest about what's real vs. inferred.
type CapPromptCaching struct {
	// ReadTokensReported: usage.cache_read_input_tokens is populated
	// on responses. Always true — we forward whatever upstream gives.
	ReadTokensReported bool `json:"read_tokens_reported"`

	// WriteTokensReported: usage.cache_creation_input_tokens is
	// populated. Always true.
	WriteTokensReported bool `json:"write_tokens_reported"`

	// LocalSimulator: if the built-in simcache is running, we blend
	// its estimates into the cache_read counters. Toggled off with
	// -simulate-cache=false.
	LocalSimulator bool `json:"local_simulator"`

	// CacheControlHonored: whether request-side cache_control markers
	// on system / message blocks are forwarded upstream. False today
	// — we strip them; Cursor's caching is opaque and controlled
	// server-side, so honoring the marker would be misleading.
	CacheControlHonored bool `json:"cache_control_honored"`
}

// currentCapabilities returns the static feature matrix. simcacheOn is
// wired from cmd/cursor-proxy/main.go via setSimCacheEnabled — the
// only field that actually varies at runtime.
var simcacheOn = true

func setSimCacheEnabled(on bool) { simcacheOn = on }

func currentCapabilities() Capabilities {
	// Agent mode reports live availability (was the runner spawned
	// AND is it currently answering pings). We keep sub-flags in
	// lock-step with runtime state so cursor2api's UI toggles match
	// what the endpoints actually deliver.
	agents := CapAgents{}
	if agentSupervisor != nil {
		info := currentAgentModeInfo()
		if info.Available {
			agents = CapAgents{
				Supported: true,
				Runtimes:  info.Runtimes,
				// MCP / skills / hooks / subagents / artifacts land
				// in Phase 4b+ (agent-config layer). Advertise them
				// as false until they actually work end-to-end —
				// the honest signal beats aspirational marketing.
				MCPManagement: false,
				Skills:        false,
				Hooks:         false,
				Subagents:     false,
				Artifacts:     false,
			}
		}
	}
	return Capabilities{
		Streaming:             true,
		ToolUseJSONInput:      true,
		MultiTurnToolLoop:     true,
		Thinking:              true,
		ServerTools:           true,
		MCPTools:              true,
		EffortMapping:         true,
		AnthropicModelAliases: true,
		PromptCaching: CapPromptCaching{
			ReadTokensReported:  true,
			WriteTokensReported: true,
			LocalSimulator:      simcacheOn,
			CacheControlHonored: false,
		},
		HTTPVersion: []string{"auto", "http1.1", "http1.0"},
		Agents:      agents,
	}
}

func capabilitiesHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(currentCapabilities())
}

// -------- introspection handlers --------

// parseSince accepts either a Go duration ("60s", "5m") or a bare
// integer of seconds. Empty / missing / unparseable → default 60s
// so a naive caller (curl without query params) still gets useful
// output.
func parseSince(raw string, def time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	if d, err := time.ParseDuration(raw); err == nil && d > 0 {
		return d
	}
	if n, err := strconv.Atoi(raw); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return def
}

// RecentTool is one entry in /v1/introspect/recent-tools.
type RecentTool struct {
	Name     string   `json:"name"`
	Requests int      `json:"requests"`
	Kind     toolKind `json:"kind"`
	Server   string   `json:"server,omitempty"`
}

// RecentToolsResponse is the aggregated shape.
type RecentToolsResponse struct {
	// SinceSeconds is the window size we aggregated over. Reflects
	// the query, not the buffer capacity — a client asking for 1h
	// on a proxy that only has 3 minutes of history will see the
	// full 3 minutes and their SinceSeconds echo will still be 3600
	// so they know their query was accepted verbatim.
	SinceSeconds  float64      `json:"since_seconds"`
	SampleSize    int          `json:"sample_size"` // total observations in window
	UniqueTools   []RecentTool `json:"unique_tools"`
	OldestSeconds float64      `json:"oldest_seconds"` // age of oldest observation
}

func recentToolsHandler(w http.ResponseWriter, r *http.Request) {
	since := parseSince(r.URL.Query().Get("since"), 60*time.Second)
	now := time.Now()
	cutoff := now.Add(-since)
	obs := observedTools.snapshotSince(cutoff)

	// Aggregate by name.
	byName := map[string]*RecentTool{}
	for _, o := range obs {
		if e, ok := byName[o.name]; ok {
			e.Requests++
			continue
		}
		byName[o.name] = &RecentTool{
			Name:     o.name,
			Requests: 1,
			Kind:     o.kind,
			Server:   o.mcpServer,
		}
	}
	list := make([]RecentTool, 0, len(byName))
	for _, e := range byName {
		list = append(list, *e)
	}
	// Sort: most-called first, then alphabetical for stable output.
	sort.Slice(list, func(i, j int) bool {
		if list[i].Requests != list[j].Requests {
			return list[i].Requests > list[j].Requests
		}
		return list[i].Name < list[j].Name
	})

	var oldest float64
	if len(obs) > 0 {
		oldest = now.Sub(obs[len(obs)-1].seenAt).Seconds()
	}
	resp := RecentToolsResponse{
		SinceSeconds:  since.Seconds(),
		SampleSize:    len(obs),
		UniqueTools:   list,
		OldestSeconds: oldest,
	}
	w.Header().Set("content-type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// RecentMCPServer is one entry in /v1/introspect/recent-mcp-servers.
// Requests counts distinct tool observations that named this server
// (so calling two different tools from `filesystem` twice each = 4).
// ToolNames is deduplicated per server.
type RecentMCPServer struct {
	Server    string   `json:"server"`
	Requests  int      `json:"requests"`
	ToolNames []string `json:"tool_names"`
}

type RecentMCPServersResponse struct {
	SinceSeconds float64           `json:"since_seconds"`
	SampleSize   int               `json:"sample_size"`
	Servers      []RecentMCPServer `json:"servers"`
}

func recentMCPServersHandler(w http.ResponseWriter, r *http.Request) {
	since := parseSince(r.URL.Query().Get("since"), 60*time.Second)
	obs := observedTools.snapshotSince(time.Now().Add(-since))
	// sample_size counts the whole window (matches
	// /v1/introspect/recent-tools' meaning) so downstream can tell
	// "no MCP traffic" from "no traffic at all" using ratios.
	totalSamples := len(obs)

	byServer := map[string]*RecentMCPServer{}
	toolSets := map[string]map[string]struct{}{}
	for _, o := range obs {
		if o.kind != toolKindMCP || o.mcpServer == "" {
			continue
		}
		if _, ok := byServer[o.mcpServer]; !ok {
			byServer[o.mcpServer] = &RecentMCPServer{Server: o.mcpServer}
			toolSets[o.mcpServer] = map[string]struct{}{}
		}
		byServer[o.mcpServer].Requests++
		toolSets[o.mcpServer][o.name] = struct{}{}
	}
	list := make([]RecentMCPServer, 0, len(byServer))
	for name, e := range byServer {
		for t := range toolSets[name] {
			e.ToolNames = append(e.ToolNames, t)
		}
		sort.Strings(e.ToolNames)
		list = append(list, *e)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].Requests != list[j].Requests {
			return list[i].Requests > list[j].Requests
		}
		return list[i].Server < list[j].Server
	})

	resp := RecentMCPServersResponse{
		SinceSeconds: since.Seconds(),
		SampleSize:   totalSamples,
		Servers:      list,
	}
	w.Header().Set("content-type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// -------- helpers for chat handlers to feed the ring --------

// extractAnthropicToolNames returns tool names from an Anthropic
// request's tools[] array, skipping server-tool entries (we already
// reject those with 400 upstream; recording them would fake activity
// that never happened).
func extractAnthropicToolNames(tools []anthropicTool) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if isAnthropicServerTool(t) {
			continue
		}
		if n := strings.TrimSpace(t.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// extractOpenAIToolNames returns tool names from an OpenAI Chat
// Completion request's tools[]. Only type=="function" tools carry a
// name we can record.
func extractOpenAIToolNames(tools []openaiTool) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Function == nil {
			continue
		}
		if n := strings.TrimSpace(t.Function.Name); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Sanity: keep fmt import used even if none of the diagnostic helpers
// are wired into a specific error path yet — future patches may add
// them and we don't want a build blip.
var _ = fmt.Sprintf
