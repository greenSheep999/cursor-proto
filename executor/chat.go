package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/cursor-proto/auth"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// HistoryTurn is a single prior turn in a multi-turn conversation. Only the
// "user" and "assistant" roles are supported today; tool turns are omitted
// because the executor does not yet drive Cursor's tool-call surface.
type HistoryTurn struct {
	Role    string // "user" | "assistant"
	Content string
}

// ChatRequest carries the minimum parameters needed to start an agent run.
type ChatRequest struct {
	Model          string // e.g. "composer-2.5"
	UserMessage    string // the human turn text
	ConversationID string // optional; auto-generated if empty
	WorkspacePath  string // optional; default os.Getwd
	// Mode selects Cursor's server-side conversation mode. The proto enum is:
	//   0 UNSPECIFIED — server treats as PLAN (verified 2026-07-19 with a
	//                   fresh Composer trace: system_reminder "Plan mode is
	//                   active" injected, tool call blocked, model forced
	//                   into CreatePlan-only mode).
	//   1 AGENT       — the real agentic mode; write_file / bash / etc. fire.
	//   2 ASK         — chat-only, no tools.
	//   3 PLAN        — explicit CreatePlan-only mode.
	//   4 DEBUG / 5 TRIAGE / 6 PROJECT / 7 MULTITASK — specialised, rarely
	//                   used from the wire proxy.
	// Leave as 0 in a ChatRequest to opt into the executor default (AGENT).
	// Historical note: this used to be documented as "1=ask, 3=agent" which
	// was wrong on both counts — that mistake is why every /v1/chat/completions
	// call landed in Plan mode until we caught it.
	Mode uint32

	// History is the prior conversation, in chronological order. Everything
	// before the current turn goes here; UserMessage remains the current
	// turn's user text. Nil / empty History keeps the single-turn behaviour
	// (nothing is added to UserMessageAction.ConversationHistory).
	History []HistoryTurn

	// SystemPrompt overrides Cursor's default coding-assistant prompt with
	// your own. Populates AgentRunRequest.CustomSystemPrompt (field 8).
	// NOTE: Cursor's server rejects this field with `unknown option
	// '--system-prompt'` when the request presents as an IDE. Use Harness=""
	// (or a non-CLI value like "api") if you need a custom system prompt.
	SystemPrompt string

	// Harness overrides the harness label sent to the server. Empty means
	// omit the field entirely. Common values: "cursor-ide", "cursor-cli",
	// or leave empty for API-style access.
	Harness string

	// PureMode strips Cursor-specific env metadata from the request so the
	// server treats us as a bare API caller rather than an IDE. Toggling this
	// on removes workspace paths, shell env, and the "cursor-ide" harness tag.
	PureMode bool

	// AutoStopOnTurnEnd closes the event channel automatically as soon as a
	// turn_ended interaction arrives. Without this the server keeps the SSE
	// open with heartbeats indefinitely.
	AutoStopOnTurnEnd bool

	// AutoStopOnToolCall closes the event channel as soon as a tool-call
	// start arrives — either an ExecServerMessage carrying McpArgs (legacy
	// MCP branch) or an InteractionUpdate carrying ToolCallStarted (the
	// modern generic branch used for client-declared tools). Cursor's
	// server pauses the SSE at that point waiting for a BidiAppend tool
	// result; without this flag the stream would hang until the heartbeat
	// deadline.
	AutoStopOnToolCall bool

	// Tools is the caller-supplied MCP tool list. When non-empty, the tools
	// are advertised to the model both via AgentRunRequest.mcp_tools (routing
	// registration) and RequestContext.tools (model-visible catalog). See
	// docs/phase-7a-mcp.md for the wire-format decisions.
	Tools []ToolDefinition

	// OmitSplicedHistory disables the in-band `<prior_conversation>` block
	// that RunChat normally injects when History is non-empty. Combined with
	// a stable ConversationID this lets callers probe whether Cursor's
	// backend remembers prior turns on its own.
	OmitSplicedHistory bool

	// OmitConversationHistoryWire skips populating
	// UserMessageAction.ConversationHistory. History is still consumed by
	// the splice step (unless OmitSplicedHistory is also set), so callers
	// can experiment with the wire field and the in-band transcript
	// independently.
	OmitConversationHistoryWire bool

	// PrependUserMessages, when non-nil, is written to
	// UserMessageAction.PrependUserMessages (field 4). Used to probe whether
	// Cursor treats that channel as an alternative history transport.
	PrependUserMessages []HistoryTurn

	// SendToInteractionListener toggles UserMessageAction.send_to_interaction_listener
	// (field 3). Nil leaves the field unset. Exposed for research probes.
	SendToInteractionListener *bool
}

// ChatEvent is one decoded server-side envelope from the SSE stream.
type ChatEvent struct {
	// Trailer, if this frame carried grpc-status metadata (end of stream).
	Trailer bool
	// Server → client message, if the frame was a data frame.
	// Exactly one of the oneof fields will be populated.
	Server *cursorpb.AgentV1_AgentServerMessage
	// Raw payload bytes (in case Server failed to unmarshal).
	Raw []byte
	// Status carries the parsed grpc-status trailer when Trailer is true.
	// A non-OK Status (code != 0) is the signal handlers should surface as
	// an HTTP error rather than an empty-content 200. Nil on data frames.
	Status *TrailerStatus
}

// RunChat starts an agent run and yields decoded server messages until the
// stream closes. The caller consumes events by iterating over the returned
// channel; the channel closes on stream end.
//
// Two HTTP calls happen in parallel:
//  1. POST api3/agent.v1.AgentService/RunSSE  — envelope-framed BidiRequestId
//     as body, SSE stream response.
//  2. POST api2/aiserver.v1.BidiService/BidiAppend  — envelope-framed
//     AgentClientMessage (wrapping the full AgentRunRequest).
//
// Cursor pairs the two via the shared request-id string.
func (c *Client) RunChat(ctx context.Context, req *ChatRequest) (<-chan ChatEvent, error) {
	// Historical bug (verified fixed 2026-07-19): this used to set
	// `req.Mode = 3` when unset, on the assumption that "3 = agent". The
	// proto enum actually maps 3 to AGENT_MODE_PLAN, so every default call
	// landed in Plan mode — Composer's server injected a "Plan mode is
	// active" system_reminder that blocked write_file / bash / edit and
	// forced CreatePlan-only behaviour. Downstream cursor2api's 2026-07-19
	// report ("CLI 0/4 built note.txt") traced back to this line. Leave
	// req.Mode alone here; buildAgentRunRequest promotes UNSPECIFIED (0)
	// to AGENT (1) with the correct enum value.
	if req.Model == "" {
		req.Model = "claude-4.5-sonnet"
	}
	requestID := auth.GenerateRequestID()
	if req.ConversationID == "" {
		req.ConversationID = auth.GenerateSessionID()
	}
	messageID := auth.GenerateRequestID()

	// If the caller supplied a SystemPrompt, prepend it to the user turn.
	// Cursor's backend rejects custom_system_prompt outright; splicing works
	// because the model treats the leading block as high-priority instruction.
	if req.SystemPrompt != "" {
		req.UserMessage = spliceSystemPrompt(req.SystemPrompt, req.UserMessage)
	}

	// Cursor's backend accepts UserMessageAction.ConversationHistory on the
	// wire but does not fold those messages into the prompt fed to the model
	// (verified 2026-07-10 with composer-2.5 — the assistant continues to
	// answer as if no prior turns existed). We keep populating that field for
	// forward-compat but also splice the history in-band into the current
	// user turn so the model actually sees it. This mirrors how SystemPrompt
	// is handled above.
	//
	// Callers probing server-side memory can set OmitSplicedHistory to skip
	// the in-band splice; ConversationHistory-only behaviour is controlled
	// by OmitConversationHistoryWire (consumed in chat_build.go).
	if len(req.History) > 0 && !req.OmitSplicedHistory {
		req.UserMessage = spliceHistory(req.History, req.UserMessage)
	}

	// Build the full AgentRunRequest. The BidiAppend payload wraps it as
	// field 1 of a manually-built "AgentClientMessage" (Cursor doesn't ship a
	// dedicated proto message for this outer envelope; per CursorGateway's
	// reverse-engineering the wrapper is just `{ field 1: AgentRunRequest }`).
	agentRun, err := c.buildAgentRunRequest(req, messageID)
	if err != nil {
		return nil, err
	}
	agentRunBytes, err := proto.Marshal(agentRun)
	if err != nil {
		return nil, fmt.Errorf("marshal AgentRunRequest: %w", err)
	}
	agentClientMsg := appendMessageField(nil, 1, agentRunBytes)

	// Build the RunSSE body: envelope + BidiRequestId proto.
	bidiRequestID := &cursorpb.AiserverV1_BidiRequestId{RequestId: requestID}
	bidiRequestIDBytes, err := proto.Marshal(bidiRequestID)
	if err != nil {
		return nil, fmt.Errorf("marshal BidiRequestId: %w", err)
	}
	sseBody := addConnectEnvelope(bidiRequestIDBytes, false)

	// Kick off the SSE request first (does not block on body; we'll read below).
	sseURL := fmt.Sprintf("%s/agent.v1.AgentService/RunSSE", c.API3)
	sseReq, err := http.NewRequestWithContext(ctx, "POST", sseURL, bytes.NewReader(sseBody))
	if err != nil {
		return nil, err
	}
	sseReq.Header.Set("content-type", "application/grpc-web+proto")
	ApplyCommonHeaders(sseReq, c.CurrentAccount(), requestID)

	// Use a client without a body timeout — the stream can be long.
	// c.NewStreamClient() honors -http-version so operators can force
	// HTTP/1.1 when a middlebox mangles h2 SSE frames.
	sseClient := c.NewStreamClient()
	sseResp, err := sseClient.Do(sseReq)
	if err != nil {
		return nil, fmt.Errorf("RunSSE dial: %w", err)
	}
	if sseResp.StatusCode != 200 {
		body, _ := io.ReadAll(io.LimitReader(sseResp.Body, 64<<10))
		sseResp.Body.Close()
		return nil, fmt.Errorf("RunSSE http %d: %s", sseResp.StatusCode, string(body))
	}

	// Send the first BidiAppend carrying the AgentClientMessage.
	if err := c.bidiAppend(ctx, requestID, 0, agentClientMsg); err != nil {
		sseResp.Body.Close()
		return nil, fmt.Errorf("BidiAppend seed: %w", err)
	}

	events := make(chan ChatEvent, 32)
	go readSSEStream(sseResp.Body, events, req.AutoStopOnTurnEnd, req.AutoStopOnToolCall)
	return events, nil
}

// postAssistantGrace is how long the reader keeps going after seeing the
// terminal assistant-role blob. If turn_ended also arrived we close after this.
const postAssistantGrace = 1 * time.Second

// heartbeatDeadline caps the total read time when only heartbeats have been
// arriving — protects against a server that never sends turn_ended.
const heartbeatDeadline = 60 * time.Second

func readSSEStream(body io.ReadCloser, out chan<- ChatEvent, autoStopOnTurnEnd, autoStopOnToolCall bool) {
	defer close(out)
	defer body.Close()

	deadline := make(chan struct{})
	var deadlineOnce bool
	setDeadline := func(d time.Duration) {
		if deadlineOnce {
			return
		}
		deadlineOnce = true
		go func() {
			time.Sleep(d)
			close(deadline)
			body.Close()
		}()
	}

	turnEnded := false
	sawAssistant := false

	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	for {
		select {
		case <-deadline:
			return
		default:
		}
		n, err := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				payload, isTrailer, rest, ok := splitConnectFrame(buf)
				if !ok {
					break
				}
				// IMPORTANT: `payload` and `rest` both alias the underlying
				// `buf` array. If we compact `buf` before consuming
				// `payload`, the memmove that `append(buf[:0], rest...)`
				// performs overwrites the bytes that `payload` points at,
				// silently tripping proto.Unmarshal and dropping the
				// frame. That is the root cause of Claude's turns going
				// through with empty content: Claude packs the assistant
				// text into short back-to-back frames, and the first
				// frame's payload gets clobbered before we can decode it.
				// `rest` itself survives because `append` is memmove
				// (overlap-safe); we only need to detach `payload`.
				payload = append([]byte(nil), payload...)
				buf = append(buf[:0], rest...)

				ev := ChatEvent{Trailer: isTrailer, Raw: payload}
				if isTrailer {
					ev.Status = ParseTrailer(payload)
				} else {
					msg := &cursorpb.AgentV1_AgentServerMessage{}
					if e := proto.Unmarshal(payload, msg); e == nil {
						ev.Server = msg
						// Watch for terminal signals so we can close eagerly
						// without waiting for the server's idle heartbeats.
						if msg.GetInteractionUpdate().GetTurnEnded() != nil {
							turnEnded = true
						}
						if autoStopOnTurnEnd && sniffAssistantBlob(msg) {
							sawAssistant = true
						}
						if autoStopOnTurnEnd && turnEnded && sawAssistant {
							setDeadline(postAssistantGrace)
						} else if autoStopOnTurnEnd && turnEnded && !deadlineOnce {
							// turn_ended came first; wait for the assistant
							// blob up to a bounded window.
							setDeadline(heartbeatDeadline / 6) // 10s
						}
						// A user-tool call parks the SSE stream while the
						// server waits for a BidiAppend tool result. Callers
						// can opt into short-circuiting that wait so the
						// tool_calls response returns to the client promptly.
						//
						// Cursor emits tool-call starts on two distinct
						// message paths (matching translator/events.go):
						//   1. ExecServerMessage.McpArgs — legacy MCP tool
						//      branch. Non-nil only for MCP-routed tools.
						//   2. InteractionUpdate.ToolCallStarted — the modern
						//      generic path used for client-declared tools
						//      like write_file / glob / grep. Cursor v3.11+
						//      routes non-MCP tools exclusively via this
						//      branch, so watching only path #1 left
						//      OpenAI-compat callers hanging until the 60s
						//      heartbeat deadline (see cursor3.11/v0.3.2).
						if autoStopOnToolCall &&
							(msg.GetExecServerMessage().GetMcpArgs() != nil ||
								msg.GetInteractionUpdate().GetToolCallStarted() != nil) {
							setDeadline(postAssistantGrace)
						}
					}
				}
				out <- ev
				if isTrailer {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// sniffAssistantBlob returns true when the message is a KV SetBlobArgs whose
// payload contains an AI-SDK style `"role":"assistant"` (and/or
// `"role": "assistant"` with a space). This is the terminal marker Cursor
// emits at the end of a turn (see docs/phase-6).
func sniffAssistantBlob(m *cursorpb.AgentV1_AgentServerMessage) bool {
	kv := m.GetKvServerMessage()
	if kv == nil {
		return false
	}
	sb := kv.GetSetBlobArgs()
	if sb == nil {
		return false
	}
	data := sb.GetBlobData()
	if len(data) == 0 || len(data) > 200_000 {
		return false
	}
	// Cursor's assistant blob starts with `{"role":"assistant"` (no space) in
	// captured samples. Scan the whole payload to be safe.
	needle := []byte(`"role":"assistant"`)
	return bytesContains(data, needle)
}

// bytesContains reports whether haystack contains needle. Avoids importing
// `bytes` for one function to keep this file dependency-light.
func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	if len(haystack) < len(needle) {
		return false
	}
	last := len(haystack) - len(needle)
	for i := 0; i <= last; i++ {
		if haystack[i] == needle[0] {
			match := true
			for j := 1; j < len(needle); j++ {
				if haystack[i+j] != needle[j] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// bidiAppend sends one AgentClientMessage (or any data blob) to BidiAppend.
// The request-id + seq no. glues it to the concurrent RunSSE.
//
// BidiAppend is a Connect *unary* RPC (server_streaming is only the RunSSE half):
// the request body is the raw protobuf-serialised message (no 5-byte envelope
// framing) and the content-type is `application/proto`. This matches
// mitmproxy captures from 2026-07-09 which showed no envelope prefix and the
// content-type `application/proto` for CppAppend / FileSync / etc.
func (c *Client) bidiAppend(ctx context.Context, requestID string, seq int64, payload []byte) error {
	body, err := proto.Marshal(&cursorpb.AiserverV1_BidiAppendRequest{
		Data:        hexEncode(payload), // legacy field, CursorGateway still populates this
		DataBinary:  payload,            // 3.10 preferred wire form
		RequestId:   &cursorpb.AiserverV1_BidiRequestId{RequestId: requestID},
		AppendSeqno: seq,
	})
	if err != nil {
		return fmt.Errorf("marshal BidiAppendRequest: %w", err)
	}

	url := fmt.Sprintf("%s/aiserver.v1.BidiService/BidiAppend", c.API2)
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("content-type", "application/proto")
	ApplyCommonHeaders(req, c.CurrentAccount(), auth.GenerateRequestID())

	cli := c.NewUnaryClient(30 * time.Second)
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := readBody(resp)
	if resp.StatusCode != 200 {
		return fmt.Errorf("BidiAppend http %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// encodeBidiAppendRequest produces the wire form for a BidiAppendRequest.
// Field layout (matches CursorGateway):
//
//	1: string data      — hex-encoded payload
//	2: message request_id (BidiRequestId { 1: string request_id })
//	3: int64 seqno
func encodeBidiAppendRequest(payload []byte, requestID string, seq int64) []byte {
	dataHex := hexEncode(payload)
	var out []byte
	out = appendStringField(out, 1, dataHex)
	// nested request_id message
	inner := appendStringField(nil, 1, requestID)
	out = appendMessageField(out, 2, inner)
	out = appendInt64Field(out, 3, seq)
	return out
}

// ---- Minimal proto wire encoding helpers (varint + length-delimited) ----

func appendVarint(buf []byte, v uint64) []byte {
	for v >= 0x80 {
		buf = append(buf, byte(v)|0x80)
		v >>= 7
	}
	return append(buf, byte(v))
}

func appendTag(buf []byte, field int, wire int) []byte {
	return appendVarint(buf, uint64(field)<<3|uint64(wire))
}

func appendStringField(buf []byte, field int, s string) []byte {
	buf = appendTag(buf, field, 2) // wire type 2 = length-delimited
	buf = appendVarint(buf, uint64(len(s)))
	return append(buf, s...)
}

func appendMessageField(buf []byte, field int, msg []byte) []byte {
	buf = appendTag(buf, field, 2)
	buf = appendVarint(buf, uint64(len(msg)))
	return append(buf, msg...)
}

func appendInt64Field(buf []byte, field int, v int64) []byte {
	buf = appendTag(buf, field, 0) // wire type 0 = varint
	return appendVarint(buf, uint64(v))
}

func appendBytesField(buf []byte, field int, b []byte) []byte {
	buf = appendTag(buf, field, 2)
	buf = appendVarint(buf, uint64(len(b)))
	return append(buf, b...)
}

func hexEncode(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = hex[v>>4]
		out[i*2+1] = hex[v&0x0f]
	}
	return string(out)
}

// Silence unused warnings if new callers appear; used by executor/chat_build.go.
var _ = appendBytesField
var _ = binary.BigEndian
