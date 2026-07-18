// responses_handler.go implements `POST /v1/responses` — the OpenAI Responses
// API endpoint. Responses is the wire format the `codex` CLI (and newer
// OpenAI SDKs like `openai>=1.60`) speak.
//
// Request shape (input we accept):
//
//	{
//	  "model": "...",
//	  "instructions": "system prompt (optional)",
//	  "input": "string" | [
//	    { "type": "message", "role": "user|assistant|system",
//	      "content": "string" | [
//	        { "type": "input_text",  "text": "..." },
//	        { "type": "output_text", "text": "..." }
//	      ]
//	    },
//	    { "type": "function_call", "call_id":"...", "name":"...", "arguments":"..." },
//	    { "type": "function_call_output", "call_id":"...", "output":"..." }
//	  ],
//	  "tools": [ { "type":"function", "name":"...", "description":"...", "parameters":{...} } ],
//	  "stream": true
//	}
//
// The handler reuses executor.Client.RunChat and translates the Cursor
// event stream through translator.OpenAIResponsesStreamWriter.
//
// Function calls arrive back as translator.EventToolCallStarted; we forward
// them as `function_call` output items with `response.function_call_arguments.delta`
// frames. Non-streaming callers get the same items in the top-level `output[]`.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/simcache"
	"github.com/router-for-me/cursor-proto/translator"
)

// chatRunner is the small subset of executor.Client that the responses/
// completions handlers depend on. Extracting it as an interface lets us
// swap in a stub for unit tests without booting a real Cursor session.
type chatRunner interface {
	RunChat(ctx context.Context, req *executor.ChatRequest) (<-chan executor.ChatEvent, error)
}

// ---------- Request schema ----------

type responsesRequest struct {
	Model        string          `json:"model"`
	Instructions string          `json:"instructions"`
	Input        json.RawMessage `json:"input"`
	Tools        []responsesTool `json:"tools"`
	Stream       bool            `json:"stream"`
}

// responsesTool matches Responses' flat tool schema. Unlike Chat Completions
// (`{type:"function", function:{...}}`), Responses puts the function fields
// on the tool object itself.
type responsesTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// responsesInputItem covers the three item variants we care about. Tool
// output items (`function_call_output`) are flattened into the assistant
// history transcript so the model sees prior tool results as context.
type responsesInputItem struct {
	Type    string          `json:"type"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`

	// function_call fields
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`

	// function_call_output fields
	Output string `json:"output"`
}

// ---------- Handler ----------

func responsesHandler(c chatRunner, cacheStore *simcache.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req responsesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		systemPrompt, history, userText, err := parseResponsesInput(&req)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if userText == "" {
			http.Error(w, "no user input", 400)
			return
		}

		prefix := prefixFromOpenAI(strings.TrimSpace(systemPrompt), history)
		decision := decideSimCache(cacheStore, prefix)

		tools := convertResponsesTools(req.Tools)
		events, err := c.RunChat(r.Context(), &executor.ChatRequest{
			Model:              req.Model,
			UserMessage:        userText,
			SystemPrompt:       systemPrompt,
			History:            history,
			ConversationID:     r.Header.Get("x-conversation-id"),
			PureMode:           true,
			AutoStopOnTurnEnd:  true,
			AutoStopOnToolCall: len(tools) > 0,
			Tools:              tools,
		})
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}

		clientToolNames := toolDefNames(tools)
		if req.Stream {
			w.Header().Set("x-cursor-cache-source", decision.headerBeforeStream())
			streamResponses(w, req.Model, events, decision, clientToolNames)
			return
		}
		nonStreamResponses(w, req.Model, events, decision, clientToolNames)
	}
}

// parseResponsesInput folds the Responses `instructions` + `input` into the
// (system, history, userText) triple that executor.RunChat consumes.
//
//   - If `input` is a string, it is treated as a single user turn.
//   - If `input` is an array, we walk items in order. All items but the last
//     message-typed user item become history; the last user message becomes
//     userText. Assistant messages, function_calls, and function_call_outputs
//     land in history as best-effort text.
func parseResponsesInput(req *responsesRequest) (systemPrompt string, history []executor.HistoryTurn, userText string, err error) {
	systemPrompt = strings.TrimSpace(req.Instructions)

	// Fast path: string input.
	if len(req.Input) > 0 && req.Input[0] == '"' {
		var s string
		if err := json.Unmarshal(req.Input, &s); err != nil {
			return "", nil, "", err
		}
		return systemPrompt, nil, s, nil
	}
	if len(req.Input) == 0 || string(req.Input) == "null" {
		return systemPrompt, nil, "", nil
	}

	var items []responsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return "", nil, "", err
	}

	// Find the index of the last message-typed user item; that becomes
	// userText. Everything else feeds history.
	lastUserIdx := -1
	for i := len(items) - 1; i >= 0; i-- {
		it := items[i]
		if it.Type == "" || it.Type == "message" {
			if it.Role == "user" {
				lastUserIdx = i
				break
			}
		}
	}

	built := make([]executor.HistoryTurn, 0, len(items))
	var trailingUser string
	for i, it := range items {
		switch it.Type {
		case "", "message":
			text := flattenResponsesContent(it.Content)
			switch it.Role {
			case "system":
				if text != "" {
					if systemPrompt != "" {
						systemPrompt += "\n"
					}
					systemPrompt += text
				}
			case "user":
				if i == lastUserIdx {
					trailingUser = text
					continue
				}
				if text != "" {
					built = append(built, executor.HistoryTurn{Role: "user", Content: text})
				}
			case "assistant":
				if text != "" {
					built = append(built, executor.HistoryTurn{Role: "assistant", Content: text})
				}
			}
		case "function_call":
			// Represent prior tool calls in transcript-form so the model can
			// see what it decided last time. Cursor's executor doesn't yet
			// carry a first-class tool-call history channel.
			line := "[tool_call name=" + it.Name + " arguments=" + it.Arguments + "]"
			built = append(built, executor.HistoryTurn{Role: "assistant", Content: line})
		case "function_call_output":
			line := "[tool_result call_id=" + it.CallID + " output=" + it.Output + "]"
			built = append(built, executor.HistoryTurn{Role: "user", Content: line})
		}
	}

	return systemPrompt, built, trailingUser, nil
}

// flattenResponsesContent turns Responses' polymorphic `content` field
// (string | []{type,text,...}) into plain text.
func flattenResponsesContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	// String content
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s
		}
	}
	// Array content
	var parts []map[string]any
	if err := json.Unmarshal(raw, &parts); err == nil {
		var out strings.Builder
		for _, p := range parts {
			if t, _ := p["text"].(string); t != "" {
				if out.Len() > 0 {
					out.WriteString("\n")
				}
				out.WriteString(t)
			}
		}
		return out.String()
	}
	return ""
}

// convertResponsesTools flattens Responses' tool schema onto executor.ToolDefinition.
func convertResponsesTools(in []responsesTool) []executor.ToolDefinition {
	if len(in) == 0 {
		return nil
	}
	out := make([]executor.ToolDefinition, 0, len(in))
	for _, t := range in {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Name == "" {
			continue
		}
		out = append(out, executor.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Parameters,
		})
	}
	return out
}

// ---------- Streaming path ----------

func streamResponses(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision, clientToolNames []string) {
	flusher, _ := w.(http.Flusher)
	headersWritten := false
	commit := func() {
		if headersWritten {
			return
		}
		headersWritten = true
		w.Header().Set("content-type", "text/event-stream")
		w.Header().Set("cache-control", "no-cache")
		w.Header().Set("x-accel-buffering", "no")
	}
	writeSSE := func(payload []byte) {
		if len(payload) == 0 {
			return
		}
		commit()
		w.Write(payload)
		if flusher != nil {
			flusher.Flush()
		}
	}

	tr := translator.NewOpenAIResponsesStreamWriter(model)
	// response.created + response.in_progress: delayed until we actually see
	// data or a benign end; a trailer error before that surfaces as a normal
	// HTTP error instead.
	initFrames := tr.InitialFrames()

	assistantSent := ""
	sawTurnEnd := false
	var trailerErr *executor.TrailerStatus
	for ev := range events {
		if ev.Trailer {
			if ev.Status != nil && !ev.Status.OK() {
				trailerErr = ev.Status
			}
			continue
		}
		if ev.Server == nil {
			continue
		}
		if !headersWritten && len(initFrames) > 0 {
			writeSSE(initFrames)
			initFrames = nil
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			delta := diffSuffix(assistantSent, blob.AssistantText)
			if delta != "" {
				assistantSent = blob.AssistantText
				writeSSE(tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta}))
			}
			continue
		}
		trEv := translateEvent(ev.Server, clientToolNames)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			writeSSE(tr.Encode(trEv))
		case translator.EventToolCallStarted, translator.EventToolCallDelta, translator.EventToolCallCompleted:
			writeSSE(tr.Encode(trEv))
		case translator.EventTurnEnded:
			sawTurnEnd = true
			decision.applyToUsage(trEv.Usage, false)
			writeSSE(tr.Encode(trEv))
		}
	}
	if !headersWritten && trailerErr != nil {
		writeUpstreamOpenAIError(w, trailerErr)
		return
	}
	// Late fallback: never emitted init frames because the whole stream was
	// heartbeats — commit and emit them now so codex sees a valid stream.
	if len(initFrames) > 0 {
		writeSSE(initFrames)
	}
	if !sawTurnEnd {
		writeSSE(tr.Encode(&translator.Event{Kind: translator.EventTurnEnded}))
	}
	writeSSE(tr.FinalCompletedFrame())
}

// ---------- Non-streaming path ----------

func nonStreamResponses(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision, clientToolNames []string) {
	acc := translator.ResponsesNonStreamingAccumulator{Model: model}
	assistantSent := ""
	var trailerErr *executor.TrailerStatus
	for ev := range events {
		if ev.Trailer {
			if ev.Status != nil && !ev.Status.OK() {
				trailerErr = ev.Status
			}
			continue
		}
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			delta := diffSuffix(assistantSent, blob.AssistantText)
			if delta != "" {
				assistantSent = blob.AssistantText
				acc.Consume(&translator.Event{Kind: translator.EventTextDelta, Text: delta})
			}
			continue
		}
		trEv := translateEvent(ev.Server, clientToolNames)
		if trEv == nil {
			continue
		}
		acc.Consume(trEv)
	}
	if trailerErr != nil && !acc.HasOutput() {
		writeUpstreamOpenAIError(w, trailerErr)
		return
	}
	var realCacheRead int64
	if acc.Usage != nil {
		realCacheRead = acc.Usage.CacheReadTokens
	}
	w.Header().Set("x-cursor-cache-source", decision.headerAfter(realCacheRead))
	decision.applyToUsage(acc.Usage, false)
	w.Header().Set("content-type", "application/json")
	w.Write(acc.Response("resp_" + auth.GenerateSessionID()))
}
