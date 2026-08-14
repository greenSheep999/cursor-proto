// gemini_handler.go implements Gemini's native `/v1beta` surface so the
// Gemini CLI (`@google/gemini-cli`) and `google-generativeai` Python SDK
// can talk to cursor-proxy without a shim.
//
// Endpoints:
//
//	GET  /v1beta/models
//	POST /v1beta/models/{model}:generateContent
//	POST /v1beta/models/{model}:streamGenerateContent
//
// Model routing: the ServeMux pattern `/v1beta/models/{tail}` captures the
// entire `<model>:<method>` string; the handler splits on the trailing
// colon to pick between non-streaming and streaming behaviour. `models/`
// name-prefix (as it appears in `GET /v1beta/models`) is stripped both on
// input (accept either `models/foo` or `foo`) and echoed with the prefix
// on output.
//
// Tool routing: Gemini's `tools:[{functionDeclarations:[...]}]` is
// remapped onto executor.ToolDefinition. If a function declaration lacks
// a name we skip that entry and continue — Gemini SDKs occasionally send
// empty stubs, and the coordinator asked for best-effort dropping over
// hard failure so text-only requests still succeed.
package main

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/simcache"
	"github.com/router-for-me/cursor-proto/translator"
)

// ---------- Request schema ----------

type geminiGenerateContentRequest struct {
	Contents          []geminiContent   `json:"contents"`
	SystemInstruction *geminiContent    `json:"systemInstruction"`
	Tools             []geminiToolGroup `json:"tools"`
	GenerationConfig  map[string]any    `json:"generationConfig"`
}

type geminiContent struct {
	Role  string       `json:"role"`
	Parts []geminiPart `json:"parts"`
}

// geminiPart is a subset of Gemini's Part union. We only forward text
// today; media/inlineData parts are dropped with a debug log.
type geminiPart struct {
	Text         string          `json:"text"`
	FunctionCall json.RawMessage `json:"functionCall"`
	FunctionResp json.RawMessage `json:"functionResponse"`
}

type geminiToolGroup struct {
	FunctionDeclarations []geminiFunctionDecl `json:"functionDeclarations"`
}

type geminiFunctionDecl struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ---------- Router ----------

// geminiRouter dispatches /v1beta/models/{tail} into either
// generateContent or streamGenerateContent based on the method suffix.
func geminiRouter(c chatRunner, cacheStore *simcache.Store) http.HandlerFunc {
	nonStream := geminiGenerateContentHandler(c, cacheStore, false)
	stream := geminiGenerateContentHandler(c, cacheStore, true)
	return func(w http.ResponseWriter, r *http.Request) {
		tail := r.PathValue("tail")
		if tail == "" {
			http.Error(w, "missing model path", http.StatusBadRequest)
			return
		}
		modelPart, method, ok := strings.Cut(tail, ":")
		if !ok {
			http.Error(w, "expected models/{model}:{method}", http.StatusBadRequest)
			return
		}
		model := translator.StripGeminiModelPrefix(modelPart)
		// Stash the model on the URL query so the shared handler can pick
		// it up without another path split.
		q := r.URL.Query()
		q.Set("_model", model)
		r.URL.RawQuery = q.Encode()
		switch method {
		case "generateContent":
			nonStream(w, r)
		case "streamGenerateContent":
			stream(w, r)
		default:
			http.Error(w, "unknown method: "+method, http.StatusNotFound)
		}
	}
}

// ---------- Handler ----------

func geminiGenerateContentHandler(c chatRunner, cacheStore *simcache.Store, streaming bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req geminiGenerateContentRequest
		if err := decodeJSONRequest(w, r, &req, false); err != nil {
			http.Error(w, err.Error(), jsonRequestErrorStatus(err))
			return
		}
		tools := convertGeminiTools(req.Tools)
		clientToolNames := toolDefNames(tools)
		recordToolsFromRequest(clientToolNames)
		model := r.URL.Query().Get("_model")
		if model == "" {
			http.Error(w, "missing model", http.StatusBadRequest)
			return
		}

		systemPrompt, history, userText, err := parseGeminiInput(&req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if userText == "" {
			http.Error(w, "no user content", http.StatusBadRequest)
			return
		}

		prefix := prefixForSimCache("gemini-generate-content", model, tools, strings.TrimSpace(systemPrompt), history)
		decision := decideSimCache(cacheStore, prefix)

		events, err := c.RunChat(r.Context(), &executor.ChatRequest{
			Model:              model,
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
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}

		if streaming {
			w.Header().Set("x-cursor-cache-source", decision.headerBeforeStream())
			streamGemini(w, model, events, decision, clientToolNames)
			return
		}
		nonStreamGemini(w, model, events, decision, clientToolNames)
	}
}

// parseGeminiInput folds Gemini's `systemInstruction` + `contents[]` into
// the (system, history, userText) triple.
//
// The last `user`-role content becomes userText; everything before it
// becomes history. `functionCall` and `functionResponse` parts on
// historical turns are best-effort inlined as text so the model retains
// tool context.
func parseGeminiInput(req *geminiGenerateContentRequest) (systemPrompt string, history []executor.HistoryTurn, userText string, err error) {
	if req.SystemInstruction != nil {
		systemPrompt = flattenGeminiParts(req.SystemInstruction.Parts)
	}
	if len(req.Contents) == 0 {
		return systemPrompt, nil, "", nil
	}

	lastUserIdx := -1
	for i := len(req.Contents) - 1; i >= 0; i-- {
		if req.Contents[i].Role == "user" || req.Contents[i].Role == "" {
			lastUserIdx = i
			break
		}
	}

	built := make([]executor.HistoryTurn, 0, len(req.Contents))
	for i, c := range req.Contents {
		text := flattenGeminiParts(c.Parts)
		role := c.Role
		if role == "" {
			role = "user"
		}
		if role == "model" {
			role = "assistant"
		}
		if i == lastUserIdx && role == "user" {
			userText = text
			continue
		}
		if text == "" {
			continue
		}
		if role != "user" && role != "assistant" {
			continue
		}
		built = append(built, executor.HistoryTurn{Role: role, Content: text})
	}
	return systemPrompt, built, userText, nil
}

// flattenGeminiParts turns a Parts array into plain text. Non-text parts
// (functionCall / functionResponse in transcripts, inline media) are
// stringified into a bracketed tag so the model still sees the context
// without needing native tool history.
func flattenGeminiParts(parts []geminiPart) string {
	var out strings.Builder
	for _, p := range parts {
		if p.Text != "" {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString(p.Text)
			continue
		}
		if len(p.FunctionCall) > 0 {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString("[functionCall ")
			out.Write(p.FunctionCall)
			out.WriteString("]")
			continue
		}
		if len(p.FunctionResp) > 0 {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString("[functionResponse ")
			out.Write(p.FunctionResp)
			out.WriteString("]")
		}
	}
	return out.String()
}

// convertGeminiTools projects Gemini's tool schema onto executor.ToolDefinition.
// Function declarations without a name are skipped so callers with empty
// stubs (Gemini SDK does this occasionally) still get their text response.
func convertGeminiTools(in []geminiToolGroup) []executor.ToolDefinition {
	if len(in) == 0 {
		return nil
	}
	var out []executor.ToolDefinition
	for _, grp := range in {
		for _, fd := range grp.FunctionDeclarations {
			if fd.Name == "" {
				continue
			}
			out = append(out, executor.ToolDefinition{
				Name:        fd.Name,
				Description: fd.Description,
				InputSchema: fd.Parameters,
			})
		}
	}
	return out
}

// ---------- Streaming path ----------

func streamGemini(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision, clientToolNames []string) {
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

	tr := translator.NewGeminiStreamWriter(model)
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
		case translator.EventToolCallStarted, translator.EventToolCallDelta:
			// Gemini writer buffers tool calls until turn_ended.
			tr.Encode(trEv)
		case translator.EventTurnEnded:
			sawTurnEnd = true
			decision.applyToUsage(trEv.Usage, false)
			writeSSE(tr.Encode(trEv))
		}
	}
	if !headersWritten && trailerErr != nil {
		writeUpstreamGeminiError(w, trailerErr)
		return
	}
	if !sawTurnEnd {
		writeSSE(tr.Encode(&translator.Event{Kind: translator.EventTurnEnded}))
	}
}

// ---------- Non-streaming path ----------

func nonStreamGemini(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision, clientToolNames []string) {
	acc := translator.GeminiNonStreamingAccumulator{Model: model}
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
		writeUpstreamGeminiError(w, trailerErr)
		return
	}
	var realCacheRead int64
	if acc.Usage != nil {
		realCacheRead = acc.Usage.CacheReadTokens
	}
	w.Header().Set("x-cursor-cache-source", decision.headerAfter(realCacheRead))
	decision.applyToUsage(acc.Usage, false)
	w.Header().Set("content-type", "application/json")
	_, _ = w.Write(acc.Response())
}

// ---------- /v1beta/models ----------

func geminiModelsListHandler(c modelLister) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := listModels(c)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ids := make([]string, 0, len(list))
		for _, m := range list {
			if id, ok := m["id"].(string); ok {
				ids = append(ids, id)
			}
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(translator.BuildGeminiModelsList(ids))
	}
}
