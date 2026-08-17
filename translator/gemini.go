// gemini.go implements the translator layer for Google Gemini's native
// wire shape (`/v1beta/models/{id}:generateContent` and
// `:streamGenerateContent`).
//
// Direction of translation:
//
//   - Request in: Gemini's `contents:[{role, parts:[{text}]}]` + optional
//     `systemInstruction` + `tools:[{functionDeclarations:[...]}]` →
//     folded into the same (SystemPrompt, History[], UserMessage) triple
//     the executor already consumes.
//
//   - Response out (non-stream): one JSON blob shaped like
//     `{candidates:[{content:{parts:[{text}],role:"model"},
//     finishReason:"STOP", index:0, safetyRatings:[]}],
//     usageMetadata:{promptTokenCount, candidatesTokenCount, totalTokenCount},
//     modelVersion:"..."}`.
//
//   - Response out (stream): SSE frames with `data: {candidates:[...]}\n\n`
//     newline-terminated. Terminal frame carries `finishReason:"STOP"` and
//     the full `usageMetadata`. Gemini clients treat each newline-delimited
//     JSON chunk as a self-contained partial GenerateContentResponse.
//
// Function calls: Gemini's `functionCall` part is emitted in candidate
// parts alongside text — the caller sees
// `parts:[{text},{functionCall:{name,args}}]`. We map executor tool_call
// events onto that shape.
package translator

import (
	"encoding/json"
	"fmt"
	"strings"
)

// GeminiUsage aggregates prompt/candidate/total tokens for the
// usageMetadata block. Field names mirror Gemini's wire keys.
type GeminiUsage struct {
	PromptTokenCount     int64
	CandidatesTokenCount int64
	TotalTokenCount      int64
	CachedContentTokens  int64
}

// GeminiUsageFromTranslator converts an internal Usage record onto the
// Gemini-shaped usageMetadata map. Handlers call this once per response.
func GeminiUsageFromTranslator(u *Usage) map[string]any {
	if u == nil {
		return map[string]any{
			"promptTokenCount":     0,
			"candidatesTokenCount": 0,
			"totalTokenCount":      0,
		}
	}
	output := NormalizedOutputTokens(u)
	total := u.InputTokens + output
	out := map[string]any{
		"promptTokenCount":     u.InputTokens,
		"candidatesTokenCount": output,
		"totalTokenCount":      total,
	}
	if u.CacheReadTokens > 0 {
		out["cachedContentTokenCount"] = u.CacheReadTokens
	}
	return out
}

// GeminiStreamWriter serialises translator Events into Gemini
// streamGenerateContent SSE frames.
//
// The wire shape is `data: {json}\n\n` per frame; each frame carries a
// partial GenerateContentResponse whose `candidates[0].content.parts`
// holds one text delta (or a `functionCall` part on tool calls). The
// terminal frame carries `finishReason:"STOP"` plus the accumulated
// usageMetadata.
type GeminiStreamWriter struct {
	Model string

	sentAny        bool
	seenToolCall   bool
	toolCallStates map[string]*geminiToolCallState
	textAcc        string
	lastUsage      *Usage
}

type geminiToolCallState struct {
	name      string
	arguments string
}

// NewGeminiStreamWriter returns a writer ready to encode frames for `model`.
func NewGeminiStreamWriter(model string) *GeminiStreamWriter {
	return &GeminiStreamWriter{Model: model}
}

// Encode returns one or more SSE frames for `ev`. For text deltas it emits
// one frame carrying the delta text; for tool calls it aggregates arguments
// and emits nothing until a completion is signalled by turn end (Gemini
// clients expect a single functionCall part per call, not incremental
// arg deltas).
func (w *GeminiStreamWriter) Encode(ev *Event) []byte {
	if ev == nil {
		return nil
	}
	switch ev.Kind {
	case EventTextDelta:
		if ev.Text == "" {
			return nil
		}
		w.sentAny = true
		w.textAcc += ev.Text
		return w.encodePartFrame(map[string]any{"text": ev.Text}, "")

	case EventToolCallStarted:
		if w.toolCallStates == nil {
			w.toolCallStates = map[string]*geminiToolCallState{}
		}
		state, exists := w.toolCallStates[ev.ToolCallID]
		if !exists {
			state = &geminiToolCallState{name: ev.ToolName}
			w.toolCallStates[ev.ToolCallID] = state
		}
		state.arguments += ev.ToolArgsDelta
		w.seenToolCall = true
		// Do not flush yet — Gemini clients want the complete argument
		// object in one functionCall part.
		return nil

	case EventToolCallDelta:
		if state, ok := w.toolCallStates[ev.ToolCallID]; ok {
			state.arguments += ev.ToolArgsDelta
		}
		return nil

	case EventTurnEnded:
		if ev.Usage != nil {
			w.lastUsage = ev.Usage
		}
		return w.flushFinal()
	}
	return nil
}

// flushFinal returns the terminal frame(s): any deferred function calls
// followed by the final `finishReason:"STOP"` frame with usageMetadata.
func (w *GeminiStreamWriter) flushFinal() []byte {
	var buf []byte
	// Emit each deferred function call as its own frame so clients can
	// distinguish per-call boundaries.
	for _, state := range w.toolCallStates {
		var args any
		if state.arguments != "" {
			var parsed any
			if err := json.Unmarshal([]byte(state.arguments), &parsed); err == nil {
				args = parsed
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		part := map[string]any{
			"functionCall": map[string]any{
				"name": state.name,
				"args": args,
			},
		}
		buf = append(buf, w.encodePartFrame(part, "")...)
	}
	finish := "STOP"
	if w.seenToolCall {
		// Gemini uses STOP even when the turn ended on a tool call; the
		// tool call itself signals the boundary. Older Gemini responses
		// used `TOOL_CALL` but current SDKs treat that as an unknown enum.
		finish = "STOP"
	}
	final := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": []any{},
					"role":  "model",
				},
				"finishReason":  finish,
				"index":         0,
				"safetyRatings": []any{},
			},
		},
		"usageMetadata": GeminiUsageFromTranslator(w.lastUsage),
		"modelVersion":  w.Model,
	}
	b, _ := json.Marshal(final)
	buf = append(buf, []byte(fmt.Sprintf("data: %s\n\n", string(b)))...)
	return buf
}

// encodePartFrame emits a single streaming frame carrying one part inside
// `candidates[0].content.parts`.
func (w *GeminiStreamWriter) encodePartFrame(part map[string]any, finishReason string) []byte {
	candidate := map[string]any{
		"content": map[string]any{
			"parts": []any{part},
			"role":  "model",
		},
		"index": 0,
	}
	if finishReason != "" {
		candidate["finishReason"] = finishReason
	}
	obj := map[string]any{
		"candidates":   []any{candidate},
		"modelVersion": w.Model,
	}
	b, _ := json.Marshal(obj)
	return []byte(fmt.Sprintf("data: %s\n\n", string(b)))
}

// ---------- Non-streaming builder ----------

// GeminiNonStreamingAccumulator collects events into a Gemini
// GenerateContentResponse body.
type GeminiNonStreamingAccumulator struct {
	Model string

	Text      string
	ToolCalls []geminiToolCallState
	Usage     *Usage
}

// Consume folds one event into the accumulator.
func (a *GeminiNonStreamingAccumulator) Consume(ev *Event) {
	if ev == nil {
		return
	}
	switch ev.Kind {
	case EventTextDelta:
		a.Text += ev.Text
	case EventToolCallStarted:
		a.ToolCalls = append(a.ToolCalls, geminiToolCallState{
			name:      ev.ToolName,
			arguments: ev.ToolArgsDelta,
		})
	case EventToolCallDelta:
		// Match by name+arg-suffix append. Gemini accumulators are simpler
		// than OpenAI's because a single call always resolves fully here.
		if len(a.ToolCalls) > 0 {
			a.ToolCalls[len(a.ToolCalls)-1].arguments += ev.ToolArgsDelta
		}
	case EventTurnEnded:
		a.Usage = ev.Usage
	}
}

// HasOutput reports whether the accumulator captured any user-visible output.
// Handlers use it to decide whether an upstream trailer error should be
// surfaced as an HTTP error instead of an empty 200.
func (a *GeminiNonStreamingAccumulator) HasOutput() bool {
	return a.Text != "" || len(a.ToolCalls) > 0
}

// Response returns the JSON body for a non-streaming call.
func (a *GeminiNonStreamingAccumulator) Response() []byte {
	parts := []any{}
	if a.Text != "" {
		parts = append(parts, map[string]any{"text": a.Text})
	}
	for _, tc := range a.ToolCalls {
		var args any
		if tc.arguments != "" {
			var parsed any
			if err := json.Unmarshal([]byte(tc.arguments), &parsed); err == nil {
				args = parsed
			}
		}
		if args == nil {
			args = map[string]any{}
		}
		parts = append(parts, map[string]any{
			"functionCall": map[string]any{
				"name": tc.name,
				"args": args,
			},
		})
	}
	obj := map[string]any{
		"candidates": []any{
			map[string]any{
				"content": map[string]any{
					"parts": parts,
					"role":  "model",
				},
				"finishReason":  "STOP",
				"index":         0,
				"safetyRatings": []any{},
			},
		},
		"usageMetadata": GeminiUsageFromTranslator(a.Usage),
		"modelVersion":  a.Model,
	}
	b, _ := json.Marshal(obj)
	return b
}

// ---------- Model catalog reshaper ----------

// BuildGeminiModelsList returns the Gemini-shaped models list payload from
// a canonical id/owned_by list. Each entry becomes a Gemini `Model`
// resource with `name: models/<id>`, `baseModelId`, and the standard
// `supportedGenerationMethods` set.
func BuildGeminiModelsList(ids []string) map[string]any {
	models := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		models = append(models, map[string]any{
			"name":                       "models/" + id,
			"baseModelId":                id,
			"version":                    "001",
			"displayName":                id,
			"description":                "Served via cursor-proxy",
			"inputTokenLimit":            1000000,
			"outputTokenLimit":           8192,
			"supportedGenerationMethods": []string{"generateContent", "streamGenerateContent"},
		})
	}
	return map[string]any{"models": models}
}

// StripGeminiModelPrefix removes the leading `models/` from a Gemini model
// name and returns the bare id. Used by handlers that route by path suffix
// (`:generateContent` / `:streamGenerateContent`).
func StripGeminiModelPrefix(name string) string {
	return strings.TrimPrefix(name, "models/")
}
