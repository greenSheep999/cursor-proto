package translator

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// AnthropicStreamWriter serialises translator Events into Anthropic Messages
// v1 SSE frames.
//
// Anthropic's stream shape:
//
//	event: message_start
//	data: {...}
//
//	event: content_block_start
//	data: {"index":0,"content_block":{"type":"text","text":""}}
//
//	event: content_block_delta
//	data: {"index":0,"delta":{"type":"text_delta","text":"Hello"}}
//
//	event: content_block_stop
//	data: {"index":0}
//
//	event: message_delta
//	data: {"delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":N}}
//
//	event: message_stop
//	data: {}
type AnthropicStreamWriter struct {
	Model      string
	ID         string
	blockOpen  bool
	blockIndex int
	sentStart  bool
	// blockType tracks the type of the currently-open content block
	// ("text", "thinking", "tool_use", or "" when closed) so we can
	// close-and-reopen when the stream switches modalities mid-turn.
	blockType string
	// toolBlocks maps tool_call_id -> block index for its content_block.
	toolBlocks  map[string]int
	sawToolCall bool
	text        strings.Builder
}

func NewAnthropicStreamWriter(model string) *AnthropicStreamWriter {
	return &AnthropicStreamWriter{
		Model: model,
		ID:    "msg_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
	}
}

// Encode returns the SSE frame(s) for one Event, potentially emitting several
// concatenated blocks (start-of-message + start-of-block + delta on first
// chunk).
func (w *AnthropicStreamWriter) Encode(ev *Event) []byte {
	if ev == nil {
		return nil
	}
	var buf []byte
	switch ev.Kind {
	case EventTextDelta:
		w.text.WriteString(ev.Text)
		if !w.sentStart {
			w.sentStart = true
			buf = append(buf, w.frame("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            w.ID,
					"type":          "message",
					"role":          "assistant",
					"model":         w.Model,
					"content":       []any{},
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
				},
			})...)
		}
		// If a thinking block is currently open, close it before opening
		// the text block — Anthropic streams one content block at a time.
		if w.blockOpen && w.blockType != "text" {
			buf = append(buf, w.frame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": w.blockIndex,
			})...)
			w.blockOpen = false
			w.blockIndex++
		}
		if !w.blockOpen {
			w.blockOpen = true
			w.blockType = "text"
			buf = append(buf, w.frame("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": w.blockIndex,
				"content_block": map[string]any{
					"type": "text",
					"text": "",
				},
			})...)
		}
		buf = append(buf, w.frame("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": w.blockIndex,
			"delta": map[string]any{
				"type": "text_delta",
				"text": ev.Text,
			},
		})...)
		return buf

	case EventThinkingDelta:
		// Anthropic Extended Thinking: emit a `thinking` content block
		// with `thinking_delta` fragments. Cursor doesn't ship the
		// signed cryptographic `signature` field the official API
		// includes, so downstream clients that require it (e.g. the
		// beta thinking passthrough loop) will treat the block as an
		// unsigned thinking preview — sufficient for UI display, not
		// for re-submission back to Anthropic.
		if !w.sentStart {
			w.sentStart = true
			buf = append(buf, w.frame("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            w.ID,
					"type":          "message",
					"role":          "assistant",
					"model":         w.Model,
					"content":       []any{},
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
				},
			})...)
		}
		// Close any non-thinking block that's open.
		if w.blockOpen && w.blockType != "thinking" {
			buf = append(buf, w.frame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": w.blockIndex,
			})...)
			w.blockOpen = false
			w.blockIndex++
		}
		if !w.blockOpen {
			w.blockOpen = true
			w.blockType = "thinking"
			buf = append(buf, w.frame("content_block_start", map[string]any{
				"type":  "content_block_start",
				"index": w.blockIndex,
				"content_block": map[string]any{
					"type":     "thinking",
					"thinking": "",
				},
			})...)
		}
		buf = append(buf, w.frame("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": w.blockIndex,
			"delta": map[string]any{
				"type":     "thinking_delta",
				"thinking": ev.Text,
			},
		})...)
		return buf

	case EventToolCallStarted:
		if !w.sentStart {
			w.sentStart = true
			buf = append(buf, w.frame("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id":            w.ID,
					"type":          "message",
					"role":          "assistant",
					"model":         w.Model,
					"content":       []any{},
					"stop_reason":   nil,
					"stop_sequence": nil,
					"usage":         map[string]int{"input_tokens": 0, "output_tokens": 0},
				},
			})...)
		}
		// Close any open text block before opening the tool_use block —
		// Anthropic streams have one content block open at a time.
		if w.blockOpen {
			buf = append(buf, w.frame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": w.blockIndex,
			})...)
			w.blockOpen = false
			w.blockIndex++
		}
		if w.toolBlocks == nil {
			w.toolBlocks = map[string]int{}
		}
		toolIdx, seen := w.toolBlocks[ev.ToolCallID]
		if seen {
			// Upstream sent tool_call_started twice for the same call_id
			// (happens when Cursor re-emits during retries or when a
			// nested step re-announces the tool). Anthropic clients
			// treat a second content_block_start on the same index as a
			// protocol error, so we drop the duplicate here — the block
			// is already open and callers keep streaming into it.
			return nil
		}
		toolIdx = w.blockIndex
		w.toolBlocks[ev.ToolCallID] = toolIdx
		w.blockIndex++
		w.sawToolCall = true
		buf = append(buf, w.frame("content_block_start", map[string]any{
			"type":  "content_block_start",
			"index": toolIdx,
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    ev.ToolCallID,
				"name":  ev.ToolName,
				"input": map[string]any{},
			},
		})...)
		if ev.ToolArgsDelta != "" {
			buf = append(buf, w.frame("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": toolIdx,
				"delta": map[string]any{
					"type":         "input_json_delta",
					"partial_json": ev.ToolArgsDelta,
				},
			})...)
		}
		return buf

	case EventToolCallDelta:
		if ev.ToolArgsDelta == "" || w.toolBlocks == nil {
			return nil
		}
		toolIdx, ok := w.toolBlocks[ev.ToolCallID]
		if !ok {
			return nil
		}
		return w.frame("content_block_delta", map[string]any{
			"type":  "content_block_delta",
			"index": toolIdx,
			"delta": map[string]any{
				"type":         "input_json_delta",
				"partial_json": ev.ToolArgsDelta,
			},
		})

	case EventToolCallCompleted:
		if w.toolBlocks == nil {
			return nil
		}
		toolIdx, ok := w.toolBlocks[ev.ToolCallID]
		if !ok {
			return nil
		}
		return w.frame("content_block_stop", map[string]any{
			"type":  "content_block_stop",
			"index": toolIdx,
		})

	case EventTurnEnded:
		if w.blockOpen {
			buf = append(buf, w.frame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": w.blockIndex,
			})...)
			w.blockOpen = false
		}
		// Close any tool_use blocks that were opened but never received an
		// explicit tool_call_completed event. Cursor doesn't send one when
		// the SSE stalls waiting for a tool result, so we synthesize the
		// content_block_stop frames here.
		for _, idx := range w.toolBlocks {
			buf = append(buf, w.frame("content_block_stop", map[string]any{
				"type":  "content_block_stop",
				"index": idx,
			})...)
		}
		w.toolBlocks = nil
		usage := map[string]any{"output_tokens": 0}
		if ev.Usage != nil {
			usage = BuildAnthropicUsage(ev.Usage)
		}
		stopReason := AnthropicStopReason(w.text.String(), w.sawToolCall)
		// Callers can force a specific stop_reason (e.g. "error" when the
		// upstream trailer surfaced a grpc-status != 0 after we'd already
		// written the SSE headers). Otherwise fall through to the
		// state-derived default.
		if ev.StopReason != "" {
			stopReason = ev.StopReason
		}
		buf = append(buf, w.frame("message_delta", map[string]any{
			"type": "message_delta",
			"delta": map[string]any{
				"stop_reason":   stopReason,
				"stop_sequence": nil,
			},
			"usage": usage,
		})...)
		buf = append(buf, w.frame("message_stop", map[string]any{
			"type": "message_stop",
		})...)
		return buf
	}
	return nil
}

func (w *AnthropicStreamWriter) frame(event string, data map[string]any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(b)))
}

// BuildAnthropicUsage renders a translator.Usage as an Anthropic-shaped
// `usage` object.
//
// Anthropic's `input_tokens` counter reports the number of NON-cached input
// tokens (that's how they price the reads separately). Cursor's TurnEnded
// reports the pre-subtraction total, so we subtract cache_read_tokens before
// exposing it. Never fall below 0.
//
// cache_read_input_tokens / cache_creation_input_tokens are always emitted
// (as 0 when unset) so downstream clients can rely on a stable shape.
func BuildAnthropicUsage(u *Usage) map[string]any {
	if u == nil {
		return map[string]any{
			"input_tokens":                0,
			"output_tokens":               0,
			"cache_read_input_tokens":     0,
			"cache_creation_input_tokens": 0,
		}
	}
	input := u.InputTokens - u.CacheReadTokens
	if input < 0 {
		input = 0
	}
	return map[string]any{
		"input_tokens":                input,
		"output_tokens":               u.OutputTokens,
		"cache_read_input_tokens":     u.CacheReadTokens,
		"cache_creation_input_tokens": u.CacheWriteTokens,
	}
}
