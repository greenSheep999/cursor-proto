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
	EventToolCallStarted
	EventToolCallDelta
	EventToolCallCompleted
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

	// Usage is populated on EventTurnEnded.
	Usage *Usage

	// StopReason lets callers override the stop_reason emitted on
	// EventTurnEnded. Empty means "let the writer choose" (end_turn or
	// tool_use, depending on state). Currently used by the streaming
	// paths to surface a trailer error as stop_reason="error" instead
	// of a misleading end_turn.
	StopReason string
}

// Usage aggregates token counters.
type Usage struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	ReasoningTokens  int64
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
	// Grok variant + any new Cursor tool type not yet enumerated
	// above emit a ToolCall whose oneof branch we don't know. The
	// pre-fix behaviour returned "" here and the client rejected it
	// ("Invalid tool ''"). Fall back to whatever raw name the proto
	// surfaces on the ToolCall envelope — better an approximate
	// name the client can log than an empty one it rejects outright.
	if raw := probeUnknownToolName(tc); raw != "" {
		return raw
	}
	return ""
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
