// completions_handler.go implements `POST /v1/completions` — OpenAI's
// legacy text-completion endpoint.
//
// Older SDKs (aider, opencode, langchain <=0.1, python-openai <1.0) still
// call this endpoint. It accepts a bare `prompt` (string or []string) and
// returns `object:"text_completion"` chunks instead of Chat Completion
// shape.
//
// The pipeline is straightforward: wrap the prompt into a single user turn,
// reuse the chat pipeline through the same \`chatRunner\` interface used by
// `/v1/responses`, then serialise the accumulator's text output back into
// text_completion shape.
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/simcache"
	"github.com/router-for-me/cursor-proto/translator"
)

type legacyCompletionsRequest struct {
	Model  string          `json:"model"`
	Prompt json.RawMessage `json:"prompt"`
	Stream bool            `json:"stream"`
}

func completionsHandler(c chatRunner, cacheStore *simcache.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req legacyCompletionsRequest
		if err := decodeJSONRequest(w, r, &req, false); err != nil {
			http.Error(w, err.Error(), jsonRequestErrorStatus(err))
			return
		}

		prompt, err := flattenLegacyPrompt(req.Prompt)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		if strings.TrimSpace(prompt) == "" {
			http.Error(w, "empty prompt", 400)
			return
		}

		prefix := prefixForSimCache("openai-completions", req.Model, nil, "", nil)
		decision := decideSimCache(cacheStore, prefix)

		events, err := c.RunChat(r.Context(), &executor.ChatRequest{
			Model:             req.Model,
			UserMessage:       prompt,
			ConversationID:    r.Header.Get("x-conversation-id"),
			PureMode:          true,
			AutoStopOnTurnEnd: true,
		})
		if err != nil {
			http.Error(w, err.Error(), 502)
			return
		}

		if req.Stream {
			w.Header().Set("x-cursor-cache-source", decision.headerBeforeStream())
			streamLegacyCompletions(w, req.Model, events, decision)
			return
		}
		nonStreamLegacyCompletions(w, req.Model, events, decision)
	}
}

// flattenLegacyPrompt accepts either a single string or an array of strings
// (which OpenAI historically supported for batch prompts) and concatenates
// them into one user turn separated by blank lines. Batch prompts collapsed
// to one turn is a lossy but sane fallback — Cursor's chat pipeline has no
// batching, and clients that actually need per-prompt scoring are extinct.
func flattenLegacyPrompt(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		return s, nil
	}
	var arr []string
	if err := json.Unmarshal(raw, &arr); err != nil {
		return "", err
	}
	return strings.Join(arr, "\n\n"), nil
}

// ---------- Streaming path ----------

func streamLegacyCompletions(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision) {
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

	id := "cmpl-" + uuid.NewString()
	created := time.Now().Unix()
	assistantSent := ""
	sawFinish := false
	sawAnyOutput := false
	var lastUsage *translator.Usage
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
				commit()
				writeLegacyChunk(w, flusher, id, created, model, delta, "")
				sawAnyOutput = true
			}
			continue
		}
		trEv := translator.FromServerMessage(ev.Server)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			if trEv.Text != "" {
				commit()
				writeLegacyChunk(w, flusher, id, created, model, trEv.Text, "")
				sawAnyOutput = true
			}
		case translator.EventTurnEnded:
			sawFinish = true
			lastUsage = trEv.Usage
			decision.applyToUsage(trEv.Usage, false)
			if sawAnyOutput {
				commit()
				writeLegacyChunk(w, flusher, id, created, model, "", "stop")
			}
		}
	}
	if !headersWritten && trailerErr != nil {
		writeUpstreamOpenAIError(w, trailerErr)
		return
	}
	if !headersWritten && isEmptyUpstreamResponse(sawAnyOutput, lastUsage) {
		writeEmptyUpstreamOpenAIError(w)
		return
	}
	if !sawFinish {
		commit()
		writeLegacyChunk(w, flusher, id, created, model, "", "stop")
	}
	commit()
	_, _ = w.Write([]byte("data: [DONE]\n\n"))
	if flusher != nil {
		flusher.Flush()
	}
}

func writeLegacyChunk(w http.ResponseWriter, flusher http.Flusher, id string, created int64, model, text, finish string) {
	choice := map[string]any{
		"index": 0,
		"text":  text,
	}
	if finish != "" {
		choice["finish_reason"] = finish
	} else {
		choice["finish_reason"] = nil
	}
	obj := map[string]any{
		"id":      id,
		"object":  "text_completion",
		"created": created,
		"model":   model,
		"choices": []any{choice},
	}
	b, _ := json.Marshal(obj)
	fmt.Fprintf(w, "data: %s\n\n", string(b))
	if flusher != nil {
		flusher.Flush()
	}
}

// ---------- Non-streaming path ----------

func nonStreamLegacyCompletions(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision) {
	acc := translator.NonStreamingAccumulator{Model: model}
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
			acc.Text = blob.AssistantText
			continue
		}
		trEv := translator.FromServerMessage(ev.Server)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			acc.Text += trEv.Text
		case translator.EventTurnEnded:
			acc.Usage = trEv.Usage
			acc.FinishStop = true
		}
	}
	if trailerErr != nil && acc.Text == "" {
		writeUpstreamOpenAIError(w, trailerErr)
		return
	}
	if isEmptyUpstreamResponse(acc.Text != "", acc.Usage) {
		writeEmptyUpstreamOpenAIError(w)
		return
	}
	var realCacheRead int64
	if acc.Usage != nil {
		realCacheRead = acc.Usage.CacheReadTokens
	}
	w.Header().Set("x-cursor-cache-source", decision.headerAfter(realCacheRead))
	decision.applyToUsage(acc.Usage, false)

	usage := legacyBuildUsage(acc.Usage)
	obj := map[string]any{
		"id":      "cmpl-" + auth.GenerateSessionID(),
		"object":  "text_completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{
			map[string]any{
				"text":          acc.Text,
				"index":         0,
				"logprobs":      nil,
				"finish_reason": "stop",
			},
		},
		"usage": usage,
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(obj)
}

// legacyBuildUsage reuses translator.Usage but drops the details sub-objects
// that OpenAI's legacy /v1/completions response shape doesn't carry.
func legacyBuildUsage(u *translator.Usage) map[string]any {
	if u == nil {
		return map[string]any{
			"prompt_tokens":     0,
			"completion_tokens": 0,
			"total_tokens":      0,
		}
	}
	return map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.InputTokens + u.OutputTokens,
	}
}
