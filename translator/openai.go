package translator

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OpenAIStreamWriter serialises translator Events into OpenAI Chat Completion
// v1 SSE frames (`data: {...}` followed by a blank line, terminating with
// `data: [DONE]`).
//
// When the client passed `stream_options: {"include_usage": true}` on the
// request, callers should set IncludeUsage=true so a final usage-only frame
// (choices: [], usage: {...}) is emitted before `data: [DONE]`.
type OpenAIStreamWriter struct {
	Model        string
	ID           string
	Created      int64
	SentStart    bool
	SawToolCall  bool
	IncludeUsage bool
	// LastUsage is the usage captured from the most recent EventTurnEnded;
	// callers can inspect it after the stream, or the writer will use it to
	// produce the trailing usage frame from FinalUsageFrame.
	LastUsage   *Usage
	toolIndexes map[string]int
}

// NewOpenAIStreamWriter creates a writer with sensible defaults.
func NewOpenAIStreamWriter(model string) *OpenAIStreamWriter {
	return &OpenAIStreamWriter{
		Model:   model,
		ID:      "chatcmpl-" + uuid.NewString(),
		Created: time.Now().Unix(),
	}
}

// Encode returns the SSE frame(s) for one Event. Multi-frame results (e.g.
// initial role delta on the very first chunk) are concatenated.
func (w *OpenAIStreamWriter) Encode(ev *Event) []byte {
	if ev == nil {
		return nil
	}
	switch ev.Kind {
	case EventTextDelta:
		var buf []byte
		if !w.SentStart {
			w.SentStart = true
			buf = append(buf, w.frame(map[string]any{
				"index":         0,
				"delta":         map[string]string{"role": "assistant"},
				"finish_reason": nil,
			}, "")...)
		}
		buf = append(buf, w.frame(map[string]any{
			"index":         0,
			"delta":         map[string]string{"content": ev.Text},
			"finish_reason": nil,
		}, "")...)
		return buf

	case EventToolCallStarted:
		if !w.SentStart {
			w.SentStart = true
		}
		idx := w.toolIndex(ev.ToolCallID)
		w.SawToolCall = true
		// Emit the tool_call header (id + name) and, when the args arrived
		// in the same envelope (MCP calls do this), the arguments string.
		var buf []byte
		buf = append(buf, w.frame(map[string]any{
			"index": 0,
			"delta": map[string]any{
				"role": "assistant",
				"tool_calls": []map[string]any{{
					"index": idx,
					"id":    ev.ToolCallID,
					"type":  "function",
					"function": map[string]any{
						"name":      ev.ToolName,
						"arguments": "",
					},
				}},
			},
			"finish_reason": nil,
		}, "")...)
		if ev.ToolArgsDelta != "" {
			buf = append(buf, w.frame(map[string]any{
				"index": 0,
				"delta": map[string]any{
					"tool_calls": []map[string]any{{
						"index": idx,
						"function": map[string]any{
							"arguments": ev.ToolArgsDelta,
						},
					}},
				},
				"finish_reason": nil,
			}, "")...)
		}
		return buf

	case EventToolCallDelta:
		if ev.ToolArgsDelta == "" {
			return nil
		}
		idx := w.toolIndex(ev.ToolCallID)
		return w.frame(map[string]any{
			"index": 0,
			"delta": map[string]any{
				"tool_calls": []map[string]any{{
					"index": idx,
					"function": map[string]any{
						"arguments": ev.ToolArgsDelta,
					},
				}},
			},
			"finish_reason": nil,
		}, "")

	case EventTurnEnded:
		if ev.Usage != nil {
			w.LastUsage = ev.Usage
		}
		finish := "stop"
		if w.SawToolCall {
			finish = "tool_calls"
		}
		return w.frame(map[string]any{
			"index":         0,
			"delta":         map[string]any{},
			"finish_reason": finish,
		}, "")
	}
	return nil
}

// FinalUsageFrame returns an OpenAI-style trailing chunk carrying a `usage`
// block and an empty `choices` array. Emit this immediately before
// FinalDone() when the client asked for stream_options.include_usage=true.
// Returns nil when no usage was ever seen or IncludeUsage is false.
func (w *OpenAIStreamWriter) FinalUsageFrame() []byte {
	if !w.IncludeUsage || w.LastUsage == nil {
		return nil
	}
	obj := map[string]any{
		"id":      w.ID,
		"object":  "chat.completion.chunk",
		"created": w.Created,
		"model":   w.Model,
		"choices": []any{},
		"usage":   buildOpenAIUsage(w.LastUsage),
	}
	b, _ := json.Marshal(obj)
	return []byte(fmt.Sprintf("data: %s\n\n", string(b)))
}

// buildOpenAIUsage renders a translator.Usage as an OpenAI-shaped `usage`
// object, including the prompt_tokens_details / completion_tokens_details
// sub-objects that OpenAI uses to surface cache and reasoning breakdowns.
func buildOpenAIUsage(u *Usage) map[string]any {
	if u == nil {
		return map[string]any{}
	}
	output := NormalizedOutputTokens(u)
	usage := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": output,
		"total_tokens":      u.InputTokens + output,
	}
	promptDetails := map[string]any{
		"cached_tokens": u.CacheReadTokens,
	}
	usage["prompt_tokens_details"] = promptDetails
	completionDetails := map[string]any{
		"reasoning_tokens": u.ReasoningTokens,
	}
	usage["completion_tokens_details"] = completionDetails
	return usage
}

// toolIndex assigns a stable, dense OpenAI tool_calls index per tool_call_id.
// OpenAI's Chat Completion API keys tool call deltas by an integer "index"
// rather than the id, so we track the first-seen order and echo it back.
func (w *OpenAIStreamWriter) toolIndex(callID string) int {
	if w.toolIndexes == nil {
		w.toolIndexes = map[string]int{}
	}
	if idx, ok := w.toolIndexes[callID]; ok {
		return idx
	}
	idx := len(w.toolIndexes)
	w.toolIndexes[callID] = idx
	return idx
}

// FinalDone returns the terminal `data: [DONE]\n\n` frame.
func (w *OpenAIStreamWriter) FinalDone() []byte {
	return []byte("data: [DONE]\n\n")
}

// frame renders one SSE frame with the standard OpenAI ChatCompletionChunk envelope.
func (w *OpenAIStreamWriter) frame(choice map[string]any, extraKey string) []byte {
	obj := map[string]any{
		"id":      w.ID,
		"object":  "chat.completion.chunk",
		"created": w.Created,
		"model":   w.Model,
		"choices": []any{choice},
	}
	b, _ := json.Marshal(obj)
	return []byte(fmt.Sprintf("data: %s\n\n", string(b)))
}

// EncodeNonStreaming builds a full OpenAI Chat Completion response (non-streaming
// mode). Call once after consuming the whole event stream, accumulating text
// and usage as you go.
type NonStreamingAccumulator struct {
	Model      string
	Text       string
	ToolCalls  []map[string]any
	Usage      *Usage
	FinishStop bool
}

func (a *NonStreamingAccumulator) Consume(ev *Event) {
	if ev == nil {
		return
	}
	switch ev.Kind {
	case EventTextDelta:
		a.Text += ev.Text
	case EventToolCallStarted:
		args := ev.ToolArgsDelta
		if args == "" {
			args = "{}"
		}
		a.ToolCalls = append(a.ToolCalls, map[string]any{
			"id":   ev.ToolCallID,
			"type": "function",
			"function": map[string]any{
				"name":      ev.ToolName,
				"arguments": args,
			},
		})
	case EventTurnEnded:
		a.FinishStop = true
		a.Usage = ev.Usage
	}
}

func (a *NonStreamingAccumulator) Response(id string) []byte {
	usage := buildOpenAIUsage(a.Usage)
	msg := map[string]any{
		"role":    "assistant",
		"content": a.Text,
	}
	finish := "stop"
	if len(a.ToolCalls) > 0 {
		msg["tool_calls"] = a.ToolCalls
		finish = "tool_calls"
	}
	obj := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   a.Model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
	b, _ := json.Marshal(obj)
	return b
}
