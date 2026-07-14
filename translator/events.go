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
				ToolCallID:    mcp.GetToolCallId(),
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
		// MCP tools arrive twice from the upstream stream: once as a
		// bare InteractionUpdate.tool_call_started (with no ToolCall
		// oneof populated — the tc.GetTool() is nil), and again
		// slightly later as an ExecServerMessage.mcp_args carrying the
		// actual arg map. If we emit the bare one first, downstream
		// writers register the tool_use content_block with no `input`
		// and the follow-up MCP frame gets dropped by their duplicate
		// guard. Skip the bare one and let the MCP branch drive it.
		if tc == nil || tc.GetTool() == nil {
			return nil
		}
		callID := s.GetCallId()
		if callID == "" {
			callID = extractToolCallID(tc)
		}
		return &Event{
			Kind:          EventToolCallStarted,
			ToolCallID:    callID,
			ToolName:      extractToolName(tc),
			ToolArgsDelta: extractToolArgsFromStart(tc),
		}
	}
	if d := iu.GetToolCallDelta(); d != nil {
		return &Event{
			Kind:          EventToolCallDelta,
			ToolCallID:    d.GetCallId(),
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
			ToolCallID: callID,
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
	// Probe the union for known native tool getters. Cursor's ToolCall is a
	// large oneof; we only surface the tools most commonly seen in agent mode.
	if tc.GetShellToolCall() != nil {
		return "shell"
	}
	if tc.GetReadToolCall() != nil {
		return "read"
	}
	if tc.GetDeleteToolCall() != nil {
		return "delete"
	}
	if tc.GetGrepToolCall() != nil {
		return "grep"
	}
	if tc.GetLsToolCall() != nil {
		return "ls"
	}
	if tc.GetGlobToolCall() != nil {
		return "glob"
	}
	if tc.GetFetchToolCall() != nil {
		return "fetch"
	}
	if tc.GetEditToolCall() != nil {
		return "edit"
	}
	if tc.GetAskQuestionToolCall() != nil {
		return "ask_question"
	}
	return ""
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
	// Native tools: each has an `Args` submessage. protojson serialization
	// gives us the canonical JSON shape.
	var argsMsg proto.Message
	switch {
	case tc.GetShellToolCall() != nil:
		argsMsg = tc.GetShellToolCall().GetArgs()
	case tc.GetReadToolCall() != nil:
		argsMsg = tc.GetReadToolCall().GetArgs()
	case tc.GetGrepToolCall() != nil:
		argsMsg = tc.GetGrepToolCall().GetArgs()
	case tc.GetLsToolCall() != nil:
		argsMsg = tc.GetLsToolCall().GetArgs()
	case tc.GetGlobToolCall() != nil:
		argsMsg = tc.GetGlobToolCall().GetArgs()
	case tc.GetFetchToolCall() != nil:
		argsMsg = tc.GetFetchToolCall().GetArgs()
	case tc.GetEditToolCall() != nil:
		argsMsg = tc.GetEditToolCall().GetArgs()
	case tc.GetDeleteToolCall() != nil:
		argsMsg = tc.GetDeleteToolCall().GetArgs()
	case tc.GetAskQuestionToolCall() != nil:
		argsMsg = tc.GetAskQuestionToolCall().GetArgs()
	}
	if argsMsg == nil {
		return "{}"
	}
	// EmitUnpopulated keeps optional zero-value fields in the JSON so a
	// client that expects a fixed shape (e.g. Claude tool loop) sees the
	// full object. UseProtoNames=false emits camelCase (Anthropic clients
	// expect that; snake_case would surprise the SDKs).
	m := protojson.MarshalOptions{
		UseProtoNames:   false,
		EmitUnpopulated: false,
	}
	b, err := m.Marshal(argsMsg)
	if err != nil || len(b) == 0 {
		return "{}"
	}
	return string(b)
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
