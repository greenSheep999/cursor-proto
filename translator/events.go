// Package translator converts Cursor's raw AgentServerMessage stream into
// canonical, provider-neutral events, and then serializes those events into
// OpenAI Chat Completion or Anthropic Messages compatible SSE payloads.
package translator

import (
	"encoding/json"
	"sort"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/router-for-me/cursor-proto/executor"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// EventKind names the discriminator on Event.
type EventKind int

const (
	EventUnknown EventKind = iota
	EventTextDelta
	EventThinkingDelta
	EventSignatureDelta
	EventToolCallStarted
	EventToolCallDelta
	EventToolCallCompleted
	EventServerToolStarted
	EventWebSearchResult
	EventTurnEnded
	EventStepStarted
	EventStepCompleted
	EventHeartbeat
)

// Event is a translator-neutral representation of one meaningful thing that
// happened in a Cursor stream. Fields not relevant to the kind are zero.
type Event struct {
	Kind EventKind

	// Text carries the delta text for EventTextDelta / EventThinkingDelta.
	Text string

	// ToolCallID + ToolName + ToolArgsDelta carry incremental tool-use info
	// for EventToolCall*. ArgsDelta is the JSON fragment; the accumulated
	// arguments assembled by the caller.
	ToolCallID    string
	ToolName      string
	ToolArgsDelta string
	WebResults    []WebSearchResult
	ToolError     string

	// Usage is populated on EventTurnEnded.
	Usage *Usage

	// StopReason lets callers override the stop_reason emitted on
	// EventTurnEnded with a legal provider reason. Transport and upstream
	// failures must use the protocol's error event instead.
	StopReason string
}

type WebSearchResult struct {
	Title string
	URL   string
	Chunk string
}

// Usage aggregates token counters.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64

	// ObservedOutputTokens is a local estimate derived from response text that
	// was actually emitted to the client. Cursor exposes two TurnEnded shapes:
	// most accounts report OutputTokens independently, while some include cache
	// read/write counters in OutputTokens. Zero means no reliable observation is
	// available, in which case the upstream value must be preserved.
	ObservedOutputTokens int64
}

// NormalizedOutputTokens returns the generated-output count represented by u.
// Cache counters are subtracted only when they form a material part of the raw
// value and the difference agrees substantially better with the response text
// observed locally. These guards preserve accounts that report a legitimate
// independent output count alongside very large cache-write counters.
func NormalizedOutputTokens(u *Usage) int64 {
	if u == nil || u.OutputTokens <= 0 {
		return 0
	}
	raw := u.OutputTokens
	observed := u.ObservedOutputTokens
	cache := u.CacheReadTokens + u.CacheWriteTokens
	if observed <= 0 || cache <= 0 || raw <= cache {
		return raw
	}

	// A small cache contribution is indistinguishable from ordinary error in
	// the character-based token estimator, so leave that shape untouched.
	if cache < (raw+2)/3 {
		return raw
	}
	candidate := raw - cache

	// The estimator is approximate across English, code and CJK. A 2x window
	// is wide enough for those scripts but rejects unrelated counter shapes.
	if candidate < (observed+1)/2 || candidate > observed*2 {
		return raw
	}
	if raw < (observed*3+1)/2 {
		return raw
	}

	rawErr := absInt64(raw - observed)
	candidateErr := absInt64(candidate - observed)
	if candidateErr*2 > rawErr {
		return raw
	}
	return candidate
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// FromServerMessage extracts an Event from one raw AgentServerMessage. Returns
// nil if the message doesn't carry an event we care about (e.g. bare KV blob).
//
// Note: user-supplied MCP tool calls arrive via `ExecServerMessage.mcp_args`,
// not `InteractionUpdate.tool_call_started`. We surface those here so the
// downstream writers can emit a single tool_call event.
func FromServerMessage(m *cursorpb.AgentV1_AgentServerMessage) *Event {
	if m == nil {
		return nil
	}
	if exec := m.GetExecServerMessage(); exec != nil {
		if mcp := exec.GetMcpArgs(); mcp != nil {
			return &Event{
				Kind:          EventToolCallStarted,
				ToolCallID:    sanitizeToolCallID(mcp.GetToolCallId()),
				ToolName:      executor.RestoreMcpToolName(pickFirstNonEmpty(mcp.GetToolName(), mcp.GetName())),
				ToolArgsDelta: encodeMcpArgs(mcp.GetArgs()),
			}
		}
	}
	iu := m.GetInteractionUpdate()
	if iu == nil {
		return nil
	}
	if td := iu.GetTextDelta(); td != nil {
		return &Event{Kind: EventTextDelta, Text: td.GetText()}
	}
	if td := iu.GetThinkingDelta(); td != nil {
		return &Event{Kind: EventThinkingDelta, Text: td.GetText()}
	}
	if s := iu.GetToolCallStarted(); s != nil {
		tc := s.GetToolCall()
		// MCP tools arrive TWICE from the upstream stream: once as a
		// InteractionUpdate.tool_call_started (with tc.mcp_tool_call
		// carrying the arg map) and again slightly later as an
		// ExecServerMessage.mcp_args with the same map. Emitting both
		// gave the downstream writers two content_block_start frames
		// (streaming) or two tool_use items (non-streaming). We
		// canonicalize on the ExecServerMessage.mcp_args branch —
		// which fires unconditionally for MCP — so skip the IU version
		// for MCP tools. Native tools (shell/edit/etc.) only fire the
		// IU branch, so they still flow through here.
		if tc == nil || tc.GetTool() == nil {
			return nil
		}
		if tc.GetMcpToolCall() != nil {
			return nil
		}
		if webSearch := tc.GetWebSearchToolCall(); webSearch != nil {
			callID := pickFirstNonEmpty(s.GetCallId(), webSearch.GetArgs().GetToolCallId())
			args, _ := json.Marshal(map[string]string{"query": webSearch.GetArgs().GetSearchTerm()})
			return &Event{
				Kind:          EventServerToolStarted,
				ToolCallID:    sanitizeToolCallID(callID),
				ToolName:      "web_search",
				ToolArgsDelta: string(args),
			}
		}
		// Historical note: v0.3.3 briefly intercepted Composer's
		// planning tools (create_plan / update_todos / read_todos /
		// task) here and rendered them as assistant text, because
		// no client harness declared them. That was wrong —
		// Composer waits for a *_response tool_result before
		// emitting pi_write / pi_bash, so text-rendering the plan
		// meant the follow-up execution tool never fired. Now we
		// pass them through as regular tool_calls with snake_case
		// names; downstream (cursor2api) mirrors Cursor's official
		// client schema and acks them normally. See cursor2api's
		// 2026-07-19 report.
		callID := s.GetCallId()
		if callID == "" {
			callID = extractToolCallID(tc)
		}
		return &Event{
			Kind:          EventToolCallStarted,
			ToolCallID:    sanitizeToolCallID(callID),
			ToolName:      extractToolName(tc),
			ToolArgsDelta: extractToolArgsFromStart(tc),
		}
	}
	if d := iu.GetToolCallDelta(); d != nil {
		return &Event{
			Kind:          EventToolCallDelta,
			ToolCallID:    sanitizeToolCallID(d.GetCallId()),
			ToolArgsDelta: extractToolArgsDelta(d.GetToolCallDelta()),
		}
	}
	if c := iu.GetToolCallCompleted(); c != nil {
		tc := c.GetToolCall()
		if tc != nil {
			if webSearch := tc.GetWebSearchToolCall(); webSearch != nil {
				callID := pickFirstNonEmpty(c.GetCallId(), webSearch.GetArgs().GetToolCallId())
				event := &Event{
					Kind:       EventWebSearchResult,
					ToolCallID: sanitizeToolCallID(callID),
					ToolName:   "web_search",
				}
				if result := webSearch.GetResult(); result != nil {
					switch {
					case result.GetSuccess() != nil:
						for _, reference := range result.GetSuccess().GetReferences() {
							event.WebResults = append(event.WebResults, WebSearchResult{
								Title: reference.GetTitle(),
								URL:   reference.GetUrl(),
								Chunk: reference.GetChunk(),
							})
						}
					case result.GetError() != nil:
						event.ToolError = result.GetError().GetError()
					case result.GetRejected() != nil:
						event.ToolError = result.GetRejected().GetReason()
					}
				}
				return event
			}
		}
		// v0.3.3 filtered planning-tool completions here; reverted
		// for the same reason as the tool_call_started branch —
		// planning tools now flow through as real tool_calls, so
		// their completions must reach the client to close the
		// content_block.
		callID := c.GetCallId()
		if callID == "" {
			callID = extractToolCallID(tc)
		}
		return &Event{
			Kind:       EventToolCallCompleted,
			ToolCallID: sanitizeToolCallID(callID),
			ToolName:   extractToolName(tc),
		}
	}
	if te := iu.GetTurnEnded(); te != nil {
		u := &Usage{
			InputTokens:      te.GetInputTokens(),
			OutputTokens:     te.GetOutputTokens(),
			CacheReadTokens:  te.GetCacheReadTokens(),
			CacheWriteTokens: te.GetCacheWriteTokens(),
			ReasoningTokens:  te.GetReasoningTokens(),
		}
		return &Event{Kind: EventTurnEnded, Usage: u}
	}
	if iu.GetStepStarted() != nil {
		return &Event{Kind: EventStepStarted}
	}
	if iu.GetStepCompleted() != nil {
		return &Event{Kind: EventStepCompleted}
	}
	if iu.GetHeartbeat() != nil {
		return &Event{Kind: EventHeartbeat}
	}
	return nil
}

// pickFirstNonEmpty returns the first non-empty string in the list, or "".
func pickFirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// sanitizeToolCallID normalizes Cursor's raw tool_call_id (which sometimes
// arrives as "<index>\n<fc_...>") into a form legal for Anthropic /
// OpenAI tool_use.id. Both APIs reject control characters — a bare
// newline in the ID caused clients that echo the ID back in a
// tool_result to fail validation. We keep the whole payload so the ID
// stays roundtrip-unique, just swap the newline for '-'.
func sanitizeToolCallID(id string) string {
	if id == "" {
		return id
	}
	return strings.ReplaceAll(strings.ReplaceAll(id, "\r", "-"), "\n", "-")
}

// encodeMcpArgs converts McpArgs.args (map<string, bytes>) into a JSON object
// string. Each value is a marshaled google.protobuf.Value on the wire; we
// decode it and re-emit the interior value as JSON.
func encodeMcpArgs(args map[string][]byte) string {
	if len(args) == 0 {
		return "{}"
	}
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("{")
	for i, k := range keys {
		if i > 0 {
			b.WriteString(",")
		}
		kb, _ := json.Marshal(k)
		b.Write(kb)
		b.WriteString(":")
		vb := decodeMcpArgValue(args[k])
		b.Write(vb)
	}
	b.WriteString("}")
	return b.String()
}

// decodeMcpArgValue turns one map value into a JSON fragment. Cursor's MCP
// value bytes may arrive in several shapes depending on the client that
// authored the tool call:
//
//  1. A marshaled google.protobuf.Value ("`\x1a\x07Beijing`" for a string
//     literal, wire tag 0x1a = field 3 (string_value) length 7).
//  2. A JSON fragment ("Beijing" quoted, or {"nested":"struct"}) — the
//     official MCP SDK writes this shape.
//  3. Raw UTF-8 bytes without any protobuf envelope — happens with some
//     older Cursor client versions.
//
// We try (1) first (protojson gives us the cleanest output when it works),
// then (2), then fall back to (3) wrapped as a JSON string. Doing JSON
// first would misparse bare strings like "42" as numbers and doubly-quote
// values.
func decodeMcpArgValue(raw []byte) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	var v structpb.Value
	if err := proto.Unmarshal(raw, &v); err == nil && v.GetKind() != nil {
		if b, err := protojson.Marshal(&v); err == nil {
			return b
		}
	}
	if json.Valid(raw) {
		return raw
	}
	b, _ := json.Marshal(string(raw))
	return b
}

// extractToolName walks a ToolCall oneof for the string tool name.
// ToolCall in Cursor is a large union — we defensively probe common shapes.
// If the struct changes we return "" and let the caller fall back gracefully.
//
// For McpToolCall (the branch user-supplied MCP tools travel through), we
// return the caller's original tool name (undoing the `mcp_` prefix we may
// have added on the way out).
func extractToolName(tc *cursorpb.AgentV1_ToolCall) string {
	if tc == nil {
		return ""
	}
	if mcp := tc.GetMcpToolCall(); mcp != nil {
		if a := mcp.GetArgs(); a != nil {
			// The wire carries the sanitized name in Name; ToolName mirrors
			// it in newer builds. Prefer ToolName when set — it stays stable
			// even if the server rewrites Name.
			name := a.GetToolName()
			if name == "" {
				name = a.GetName()
			}
			return executor.RestoreMcpToolName(name)
		}
		return ""
	}
	// Probe the union for known native tool getters. Cursor's ToolCall
	// is a large oneof; we surface the tools most commonly seen in
	// agent mode and translate the Cursor exec type into the client-
	// visible name (Bash / Read / Write / Grep / Glob / LS / WebFetch)
	// via native_tool_adapter.go. Without this remap, clients see
	// "Invalid tool 'shell'" and reject the whole turn.
	if tc.GetShellToolCall() != nil {
		return clientNameForCursorTool("shell")
	}
	if tc.GetReadToolCall() != nil {
		return clientNameForCursorTool("read")
	}
	if tc.GetDeleteToolCall() != nil {
		return clientNameForCursorTool("delete")
	}
	if tc.GetGrepToolCall() != nil {
		return clientNameForCursorTool("grep")
	}
	if tc.GetLsToolCall() != nil {
		return clientNameForCursorTool("ls")
	}
	if tc.GetGlobToolCall() != nil {
		return clientNameForCursorTool("glob")
	}
	if tc.GetFetchToolCall() != nil {
		return clientNameForCursorTool("fetch")
	}
	if tc.GetEditToolCall() != nil {
		return clientNameForCursorTool("edit")
	}
	if tc.GetAskQuestionToolCall() != nil {
		return clientNameForCursorTool("ask_question")
	}
	// Pi* family — Composer / planning-intelligence tools. Composer
	// prefers these over shell/edit/read even when the caller
	// registered an MCP tool set (downstream reported this on
	// 2026-07-18: write_file requests hit PiWriteToolCall and our
	// switch didn't enumerate it, giving the client name:"").
	if tc.GetPiWriteToolCall() != nil {
		return clientNameForCursorTool("pi_write")
	}
	if tc.GetPiBashToolCall() != nil {
		return clientNameForCursorTool("pi_bash")
	}
	if tc.GetPiEditToolCall() != nil {
		return clientNameForCursorTool("pi_edit")
	}
	if tc.GetPiReadToolCall() != nil {
		return clientNameForCursorTool("pi_read")
	}
	if tc.GetPiFindToolCall() != nil {
		return clientNameForCursorTool("pi_find")
	}
	if tc.GetPiGrepToolCall() != nil {
		return clientNameForCursorTool("pi_grep")
	}
	if tc.GetPiLsToolCall() != nil {
		return clientNameForCursorTool("pi_ls")
	}
	// Composer planning-intelligence tools. These are real tool_calls
	// on the wire — Composer waits for a *_response tool_result before
	// continuing to pi_write / pi_bash. Return snake_case names so
	// clients that declared them (Cursor's official client, cursor2api
	// harnesses that mirror its schema) dispatch correctly.
	if tc.GetCreatePlanToolCall() != nil {
		return clientNameForCursorTool("create_plan")
	}
	if tc.GetUpdateTodosToolCall() != nil {
		return clientNameForCursorTool("update_todos")
	}
	if tc.GetReadTodosToolCall() != nil {
		return clientNameForCursorTool("read_todos")
	}
	if tc.GetTaskToolCall() != nil {
		return clientNameForCursorTool("task")
	}
	// Any Cursor tool type not enumerated above (SemSearch, WebSearch,
	// ListMcpResources, and 40+ others). Return the oneof branch name
	// via protoreflect so clients see SOMETHING they can log or alias
	// instead of the empty string — the downstream report noted "a
	// client that sees 'shell' can decide what to do; a client that
	// sees '' has zero information".
	if raw := oneofBranchName(tc); raw != "" {
		return raw
	}
	// Last-ditch: scan the envelope for a top-level name string
	// field. Current proto doesn't expose one, but future revisions
	// might.
	if raw := probeUnknownToolName(tc); raw != "" {
		return raw
	}
	return ""
}

// oneofBranchName returns the CamelCase suffix of the ToolCall
// oneof branch when the typed switch above didn't match. For
// example a ToolCall carrying CreatePlanToolCall returns
// "CreatePlan". Clients can then treat unknown tool names as
// opaque strings — better than "" which every harness rejects
// outright.
func oneofBranchName(tc *cursorpb.AgentV1_ToolCall) string {
	if tc == nil {
		return ""
	}
	msg := tc.ProtoReflect()
	// Walk the ToolCall's oneof descriptors — every native tool
	// branch is one field in the `tool` oneof (see gen/cursor/*.go
	// AgentV1_ToolCall). WhichOneof returns the populated field
	// descriptor when exactly one is set.
	oneofs := msg.Descriptor().Oneofs()
	for i := 0; i < oneofs.Len(); i++ {
		od := oneofs.Get(i)
		fd := msg.WhichOneof(od)
		if fd == nil {
			continue
		}
		// Field JSON name is e.g. "createPlanToolCall". Strip the
		// trailing "ToolCall" so we surface "createPlan" — closer to
		// what a harness would recognise or alias.
		name := fd.JSONName()
		return trimSuffixCase(name, "ToolCall")
	}
	return ""
}

// trimSuffixCase strips a suffix from a string when present,
// mirroring strings.TrimSuffix. Local helper so this file's
// dependency footprint stays tight.
func trimSuffixCase(s, suffix string) string {
	if len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix {
		return s[:len(s)-len(suffix)]
	}
	return s
}

// probeUnknownToolName scans a ToolCall envelope for any string
// field that looks like a tool name. Used as the last-ditch
// fallback when the model uses a tool type this build doesn't
// enumerate in extractToolName (Grok variants, new Cursor
// releases). Returns "" if nothing usable is on the envelope.
//
// The current ToolCall struct doesn't expose a top-level name
// field — every native tool has its name implicit in the oneof
// branch. But protoreflect.Descriptor lets us walk the message and
// look for any populated string field named `name`, `tool_name`,
// or `type`. Cheap defensive scan; runs only when the typed
// switch found nothing.
func probeUnknownToolName(tc *cursorpb.AgentV1_ToolCall) string {
	if tc == nil {
		return ""
	}
	msg := tc.ProtoReflect()
	fields := msg.Descriptor().Fields()
	var found string
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		name := string(fd.Name())
		if name != "name" && name != "tool_name" && name != "type" {
			continue
		}
		if !msg.Has(fd) {
			continue
		}
		v := msg.Get(fd).String()
		if v != "" && found == "" {
			found = v
		}
	}
	return found
}

// extractToolCallID returns the tool_call_id embedded in a ToolCall envelope
// when present. For MCP tool calls this echoes back what the server assigned;
// for other tool types the top-level ToolCall.tool_call_id is the source.
func extractToolCallID(tc *cursorpb.AgentV1_ToolCall) string {
	if tc == nil {
		return ""
	}
	if id := tc.GetToolCallId(); id != "" {
		return id
	}
	if mcp := tc.GetMcpToolCall(); mcp != nil {
		if a := mcp.GetArgs(); a != nil {
			return a.GetToolCallId()
		}
	}
	return ""
}

// extractToolArgsFromStart returns the full JSON-encoded arguments for a
// tool_call_started envelope. Cursor delivers the complete argument struct
// on the started event for every tool type (MCP and native), so downstream
// writers can emit a single argument delta with no further partial-JSON
// concatenation — the ToolCallDelta stream that follows describes tool
// EXECUTION progress (shell stdout, edit stream_content, task interaction),
// NOT argument fragments.
func extractToolArgsFromStart(tc *cursorpb.AgentV1_ToolCall) string {
	if tc == nil {
		return ""
	}
	// MCP: args is a map<string, bytes> of marshaled Value proto.
	if mcp := tc.GetMcpToolCall(); mcp != nil {
		return mcpArgsToJSON(mcp.GetArgs())
	}
	// Native tools: run through the adapter so field names match the
	// client-visible schema (Claude Code Bash/Read/Write/Grep/Glob/…)
	// instead of Cursor's internal names. Previously we emitted
	// protojson.Marshal output which used Cursor field names like
	// `glob_pattern` / `working_directory` / `stream_content` — the
	// client's tool loop then rejected the input as malformed. See
	// translator/native_tool_adapter.go for the mapping.
	if native := mapNativeToolArgsJSON(tc); native != "" {
		return native
	}
	return "{}"
}

// mcpArgsToJSON is the MCP branch of extractToolArgsFromStart, factored out
// so the native-tool switch stays readable. Delegates value decoding to
// decodeMcpArgValue so both call sites (encodeMcpArgs and this one) use
// the same protobuf-Value-first strategy — without this, string values
// arrived to Anthropic clients wrapped in their raw wire tag prefix
// (e.g. "Beijing" for "Beijing"), and JSON.parse choked.
func mcpArgsToJSON(a *cursorpb.AgentV1_McpArgs) string {
	if a == nil {
		return ""
	}
	return encodeMcpArgs(a.GetArgs())
}

// extractToolArgsDelta USED to try to surface incremental JSON from a
// ToolCallDelta, but Cursor's ToolCallDelta doesn't carry argument
// fragments — it carries EXECUTION progress (shell stdout, edit
// stream_content, task interaction updates). Feeding that as
// `input_json_delta` to Claude clients produces protobuf-debug garbage in
// tool_use.input and hangs the client's JSON.parse. Returning "" here
// suppresses those deltas; the full argument object was already delivered
// on tool_call_started via extractToolArgsFromStart.
func extractToolArgsDelta(d *cursorpb.AgentV1_ToolCallDelta) string {
	_ = d
	return ""
}
