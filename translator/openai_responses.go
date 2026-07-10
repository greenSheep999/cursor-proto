// openai_responses.go implements the translator layer for OpenAI's
// Responses API (`POST /v1/responses`).
//
// Responses is the wire format the `codex` CLI (and newer OpenAI SDKs) speak.
// Both the streaming and non-streaming shapes differ meaningfully from
// Chat Completions:
//
//   - Streaming: named SSE events (`event: response.created`, ...) with a
//     monotonically increasing `sequence_number` and a two-layer item model
//     (output_item → content_part → output_text.*). Function calls are their
//     own item type (`function_call`) with `.function_call_arguments.delta`
//     frames.
//   - Non-streaming: one JSON blob with `{ id, object:"response", model,
//     output:[...], usage:{...} }`. `output[0]` is a `message` item whose
//     `content[0]` is `output_text`; each tool call is its own `function_call`
//     item appended to `output[]`.
//
// The exact wire order and field layout mirrors CLIProxyAPI's implementation
// (see internal/translator/claude/openai/responses/ and
// internal/runtime/executor/codex_executor_stream_output_test.go over there)
// so real codex clients can consume our stream without a shim.
package translator

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OpenAIResponsesStreamWriter serialises translator Events into OpenAI
// Responses API SSE frames.
//
// It maintains:
//   - a monotonically increasing sequence_number (starts at 0 for
//     response.created and increments on every emitted frame);
//   - one message item that opens lazily on the first EventTextDelta and
//     closes on EventTurnEnded;
//   - one function_call item per unique tool_call_id, tracked by
//     output_index so the client can associate deltas with the item.
type OpenAIResponsesStreamWriter struct {
	Model     string
	ID        string
	CreatedAt int64

	seq int

	// message item state
	messageOpen  bool
	messageIndex int
	messageID    string
	partOpen     bool
	textSoFar    string

	// function-call state
	toolCalls    map[string]*responsesToolCallState
	nextOutIndex int

	// aggregate for response.completed
	completedItems   []map[string]any
	lastUsage        *Usage
	completedEmitted bool
}

type responsesToolCallState struct {
	id          string // fc_<uuid>
	callID      string // caller-visible call id (from event)
	name        string
	outputIndex int
	arguments   string
	opened      bool
	closed      bool
}

// NewOpenAIResponsesStreamWriter returns a writer with sensible defaults.
func NewOpenAIResponsesStreamWriter(model string) *OpenAIResponsesStreamWriter {
	return &OpenAIResponsesStreamWriter{
		Model:     model,
		ID:        "resp_" + uuid.NewString(),
		CreatedAt: time.Now().Unix(),
	}
}

// InitialFrames returns the `response.created` + `response.in_progress` frames
// that must be emitted before any output items. Handlers call this once at the
// top of a stream so headers get flushed with something in the pipe.
func (w *OpenAIResponsesStreamWriter) InitialFrames() []byte {
	created := map[string]any{
		"type":            "response.created",
		"sequence_number": w.nextSeq(),
		"response":        w.responseSkeleton("in_progress"),
	}
	inProgress := map[string]any{
		"type":            "response.in_progress",
		"sequence_number": w.nextSeq(),
		"response":        w.responseSkeleton("in_progress"),
	}
	var buf []byte
	buf = append(buf, encodeResponsesFrame("response.created", created)...)
	buf = append(buf, encodeResponsesFrame("response.in_progress", inProgress)...)
	return buf
}

// Encode returns the SSE frame(s) for one Event. Multi-frame results are
// concatenated. Returns nil for events that don't produce output.
func (w *OpenAIResponsesStreamWriter) Encode(ev *Event) []byte {
	if ev == nil {
		return nil
	}
	switch ev.Kind {
	case EventTextDelta:
		if ev.Text == "" {
			return nil
		}
		return w.encodeTextDelta(ev.Text)

	case EventToolCallStarted:
		return w.encodeToolCallStart(ev)

	case EventToolCallDelta:
		return w.encodeToolCallDelta(ev)

	case EventToolCallCompleted:
		return w.encodeToolCallCompleted(ev)

	case EventTurnEnded:
		if ev.Usage != nil {
			w.lastUsage = ev.Usage
		}
		return w.encodeTurnEnded()
	}
	return nil
}

// FinalCompletedFrame returns the `response.completed` frame. Handlers call
// this immediately before the stream terminator when TurnEnded already
// finalised the message; if Encode already emitted the completed frame
// (because TurnEnded arrived in-stream) this returns nil.
func (w *OpenAIResponsesStreamWriter) FinalCompletedFrame() []byte {
	if w.completedEmitted {
		return nil
	}
	return w.buildCompletedFrame()
}

func (w *OpenAIResponsesStreamWriter) buildCompletedFrame() []byte {
	w.completedEmitted = true
	resp := w.responseSkeleton("completed")
	// Materialise output[] from what we saw.
	output := make([]map[string]any, 0, len(w.completedItems))
	output = append(output, w.completedItems...)
	// If no message item was ever emitted but text arrived (defensive), fall
	// back to whatever text we accumulated so downstream tools still see the
	// completion.
	if len(output) == 0 && w.textSoFar != "" {
		output = append(output, w.buildMessageItem(w.textSoFar))
	}
	resp["output"] = output
	if w.lastUsage != nil {
		resp["usage"] = buildResponsesUsage(w.lastUsage)
	}
	obj := map[string]any{
		"type":            "response.completed",
		"sequence_number": w.nextSeq(),
		"response":        resp,
	}
	return encodeResponsesFrame("response.completed", obj)
}

func (w *OpenAIResponsesStreamWriter) encodeTextDelta(text string) []byte {
	var buf []byte
	if !w.messageOpen {
		w.messageOpen = true
		w.messageID = "msg_" + uuid.NewString()
		w.messageIndex = w.nextOutIndex
		w.nextOutIndex++
		itemAdded := map[string]any{
			"type":            "response.output_item.added",
			"sequence_number": w.nextSeq(),
			"output_index":    w.messageIndex,
			"item": map[string]any{
				"id":      w.messageID,
				"type":    "message",
				"status":  "in_progress",
				"content": []any{},
				"role":    "assistant",
			},
		}
		buf = append(buf, encodeResponsesFrame("response.output_item.added", itemAdded)...)
	}
	if !w.partOpen {
		w.partOpen = true
		partAdded := map[string]any{
			"type":            "response.content_part.added",
			"sequence_number": w.nextSeq(),
			"item_id":         w.messageID,
			"output_index":    w.messageIndex,
			"content_index":   0,
			"part": map[string]any{
				"type":        "output_text",
				"annotations": []any{},
				"logprobs":    []any{},
				"text":        "",
			},
		}
		buf = append(buf, encodeResponsesFrame("response.content_part.added", partAdded)...)
	}
	w.textSoFar += text
	delta := map[string]any{
		"type":            "response.output_text.delta",
		"sequence_number": w.nextSeq(),
		"item_id":         w.messageID,
		"output_index":    w.messageIndex,
		"content_index":   0,
		"delta":           text,
		"logprobs":        []any{},
	}
	buf = append(buf, encodeResponsesFrame("response.output_text.delta", delta)...)
	return buf
}

func (w *OpenAIResponsesStreamWriter) encodeToolCallStart(ev *Event) []byte {
	if w.toolCalls == nil {
		w.toolCalls = map[string]*responsesToolCallState{}
	}
	state, seen := w.toolCalls[ev.ToolCallID]
	if !seen {
		state = &responsesToolCallState{
			id:          "fc_" + uuid.NewString(),
			callID:      ev.ToolCallID,
			name:        ev.ToolName,
			outputIndex: w.nextOutIndex,
		}
		w.nextOutIndex++
		w.toolCalls[ev.ToolCallID] = state
	}
	var buf []byte
	if !state.opened {
		state.opened = true
		// If a message content_part was open, we DON'T force-close it here —
		// Cursor's stream may return to text after a tool call and the
		// output_text.done frame is only appropriate on turn end. The
		// downstream OpenAI SDK tolerates interleaved tool_calls between
		// output_text.delta frames as long as sequence_numbers advance.
		itemAdded := map[string]any{
			"type":            "response.output_item.added",
			"sequence_number": w.nextSeq(),
			"output_index":    state.outputIndex,
			"item": map[string]any{
				"id":        state.id,
				"type":      "function_call",
				"status":    "in_progress",
				"arguments": "",
				"call_id":   state.callID,
				"name":      state.name,
			},
		}
		buf = append(buf, encodeResponsesFrame("response.output_item.added", itemAdded)...)
	}
	if ev.ToolArgsDelta != "" {
		state.arguments += ev.ToolArgsDelta
		delta := map[string]any{
			"type":            "response.function_call_arguments.delta",
			"sequence_number": w.nextSeq(),
			"item_id":         state.id,
			"output_index":    state.outputIndex,
			"delta":           ev.ToolArgsDelta,
		}
		buf = append(buf, encodeResponsesFrame("response.function_call_arguments.delta", delta)...)
	}
	return buf
}

func (w *OpenAIResponsesStreamWriter) encodeToolCallDelta(ev *Event) []byte {
	if w.toolCalls == nil {
		return nil
	}
	state, ok := w.toolCalls[ev.ToolCallID]
	if !ok || ev.ToolArgsDelta == "" {
		return nil
	}
	state.arguments += ev.ToolArgsDelta
	delta := map[string]any{
		"type":            "response.function_call_arguments.delta",
		"sequence_number": w.nextSeq(),
		"item_id":         state.id,
		"output_index":    state.outputIndex,
		"delta":           ev.ToolArgsDelta,
	}
	return encodeResponsesFrame("response.function_call_arguments.delta", delta)
}

func (w *OpenAIResponsesStreamWriter) encodeToolCallCompleted(ev *Event) []byte {
	if w.toolCalls == nil {
		return nil
	}
	state, ok := w.toolCalls[ev.ToolCallID]
	if !ok || state.closed {
		return nil
	}
	return w.closeToolCall(state)
}

// encodeTurnEnded finalises the message item (if one is open), synthesises
// content_part.done / output_text.done / output_item.done frames, closes any
// still-open tool calls, and emits `response.completed`.
func (w *OpenAIResponsesStreamWriter) encodeTurnEnded() []byte {
	var buf []byte
	if w.messageOpen {
		// output_text.done
		if w.partOpen {
			textDone := map[string]any{
				"type":            "response.output_text.done",
				"sequence_number": w.nextSeq(),
				"item_id":         w.messageID,
				"output_index":    w.messageIndex,
				"content_index":   0,
				"text":            w.textSoFar,
				"logprobs":        []any{},
			}
			buf = append(buf, encodeResponsesFrame("response.output_text.done", textDone)...)
			partDone := map[string]any{
				"type":            "response.content_part.done",
				"sequence_number": w.nextSeq(),
				"item_id":         w.messageID,
				"output_index":    w.messageIndex,
				"content_index":   0,
				"part": map[string]any{
					"type":        "output_text",
					"annotations": []any{},
					"logprobs":    []any{},
					"text":        w.textSoFar,
				},
			}
			buf = append(buf, encodeResponsesFrame("response.content_part.done", partDone)...)
			w.partOpen = false
		}
		item := w.buildMessageItem(w.textSoFar)
		itemDone := map[string]any{
			"type":            "response.output_item.done",
			"sequence_number": w.nextSeq(),
			"output_index":    w.messageIndex,
			"item":            item,
		}
		buf = append(buf, encodeResponsesFrame("response.output_item.done", itemDone)...)
		// Aggregate message item at output_index position. Since output_index
		// starts at 0 and we push in order, appending keeps the correct order.
		w.completedItems = insertAtOutputIndex(w.completedItems, w.messageIndex, item)
		w.messageOpen = false
	}
	// Close any tool calls that never received an explicit completed event.
	for _, state := range w.toolCalls {
		if !state.opened || state.closed {
			continue
		}
		buf = append(buf, w.closeToolCall(state)...)
	}
	buf = append(buf, w.buildCompletedFrame()...)
	return buf
}

// insertAtOutputIndex places item at position idx in items, growing the slice
// with nil placeholders as needed. Callers rely on output_index being unique;
// on collision the existing entry is overwritten so the last write wins (which
// matches how OpenAI's own Responses stream reports final items).
func insertAtOutputIndex(items []map[string]any, idx int, item map[string]any) []map[string]any {
	for len(items) <= idx {
		items = append(items, nil)
	}
	items[idx] = item
	return items
}

func (w *OpenAIResponsesStreamWriter) buildMessageItem(text string) map[string]any {
	return map[string]any{
		"id":     w.messageID,
		"type":   "message",
		"status": "completed",
		"content": []any{
			map[string]any{
				"type":        "output_text",
				"annotations": []any{},
				"logprobs":    []any{},
				"text":        text,
			},
		},
		"role": "assistant",
	}
}

func (w *OpenAIResponsesStreamWriter) responseSkeleton(status string) map[string]any {
	return map[string]any{
		"id":         w.ID,
		"object":     "response",
		"created_at": w.CreatedAt,
		"status":     status,
		"background": false,
		"error":      nil,
		"model":      w.Model,
		"output":     []any{},
	}
}

func (w *OpenAIResponsesStreamWriter) nextSeq() int {
	s := w.seq
	w.seq++
	return s
}

// encodeResponsesFrame writes one SSE frame with a named event.
func encodeResponsesFrame(event string, data map[string]any) []byte {
	b, _ := json.Marshal(data)
	return []byte(fmt.Sprintf("event: %s\ndata: %s\n\n", event, string(b)))
}

// buildResponsesUsage maps translator.Usage onto the OpenAI Responses
// `usage` object shape.
func buildResponsesUsage(u *Usage) map[string]any {
	if u == nil {
		return map[string]any{
			"input_tokens":  0,
			"output_tokens": 0,
			"total_tokens":  0,
		}
	}
	inputDetails := map[string]any{
		"cached_tokens": u.CacheReadTokens,
	}
	outputDetails := map[string]any{
		"reasoning_tokens": u.ReasoningTokens,
	}
	return map[string]any{
		"input_tokens":          u.InputTokens,
		"input_tokens_details":  inputDetails,
		"output_tokens":         u.OutputTokens,
		"output_tokens_details": outputDetails,
		"total_tokens":          u.InputTokens + u.OutputTokens,
	}
}

// ---------- Non-streaming builder ----------

// ResponsesNonStreamingAccumulator collects events into an OpenAI Responses
// non-streaming response body.
type ResponsesNonStreamingAccumulator struct {
	Model string

	// text is the assistant's assembled text.
	Text string

	// toolCalls preserves function-call items in the order they arrived.
	toolCalls []responsesToolCallState

	Usage *Usage
}

// Consume folds one Event into the accumulator.
func (a *ResponsesNonStreamingAccumulator) Consume(ev *Event) {
	if ev == nil {
		return
	}
	switch ev.Kind {
	case EventTextDelta:
		a.Text += ev.Text
	case EventToolCallStarted:
		a.toolCalls = append(a.toolCalls, responsesToolCallState{
			id:        "fc_" + uuid.NewString(),
			callID:    ev.ToolCallID,
			name:      ev.ToolName,
			arguments: ev.ToolArgsDelta,
		})
	case EventToolCallDelta:
		if len(a.toolCalls) == 0 {
			return
		}
		// Find the matching call.
		for i := range a.toolCalls {
			if a.toolCalls[i].callID == ev.ToolCallID {
				a.toolCalls[i].arguments += ev.ToolArgsDelta
				return
			}
		}
	case EventTurnEnded:
		a.Usage = ev.Usage
	}
}

// HasOutput reports whether the accumulator captured any user-visible output
// (text or tool call). Handlers use it to distinguish an empty-content success
// from a hard-fail upstream error worth surfacing as HTTP 4xx/5xx.
func (a *ResponsesNonStreamingAccumulator) HasOutput() bool {
	return a.Text != "" || len(a.toolCalls) > 0
}

// Response builds the full non-streaming JSON payload.
func (a *ResponsesNonStreamingAccumulator) Response(id string) []byte {
	output := []map[string]any{}
	if a.Text != "" {
		output = append(output, map[string]any{
			"id":     "msg_" + uuid.NewString(),
			"type":   "message",
			"status": "completed",
			"content": []any{
				map[string]any{
					"type":        "output_text",
					"annotations": []any{},
					"logprobs":    []any{},
					"text":        a.Text,
				},
			},
			"role": "assistant",
		})
	}
	for _, tc := range a.toolCalls {
		args := tc.arguments
		if args == "" {
			args = "{}"
		}
		output = append(output, map[string]any{
			"id":        tc.id,
			"type":      "function_call",
			"status":    "completed",
			"arguments": args,
			"call_id":   tc.callID,
			"name":      tc.name,
		})
	}
	obj := map[string]any{
		"id":         id,
		"object":     "response",
		"created_at": time.Now().Unix(),
		"status":     "completed",
		"background": false,
		"error":      nil,
		"model":      a.Model,
		"output":     output,
		"usage":      buildResponsesUsage(a.Usage),
	}
	b, _ := json.Marshal(obj)
	return b
}

func (w *OpenAIResponsesStreamWriter) closeToolCall(state *responsesToolCallState) []byte {
	state.closed = true
	var buf []byte
	argsDone := map[string]any{
		"type":            "response.function_call_arguments.done",
		"sequence_number": w.nextSeq(),
		"item_id":         state.id,
		"output_index":    state.outputIndex,
		"arguments":       state.arguments,
	}
	buf = append(buf, encodeResponsesFrame("response.function_call_arguments.done", argsDone)...)
	itemDone := map[string]any{
		"type":            "response.output_item.done",
		"sequence_number": w.nextSeq(),
		"output_index":    state.outputIndex,
		"item": map[string]any{
			"id":        state.id,
			"type":      "function_call",
			"status":    "completed",
			"arguments": state.arguments,
			"call_id":   state.callID,
			"name":      state.name,
		},
	}
	buf = append(buf, encodeResponsesFrame("response.output_item.done", itemDone)...)
	w.completedItems = append(w.completedItems, map[string]any{
		"id":        state.id,
		"type":      "function_call",
		"status":    "completed",
		"arguments": state.arguments,
		"call_id":   state.callID,
		"name":      state.name,
	})
	return buf
}
