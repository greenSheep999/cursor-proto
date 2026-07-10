// cursor-proxy exposes OpenAI- and Anthropic-compatible HTTP endpoints backed
// by Cursor.
//
// Usage:
//
//	cursor-proxy -addr 127.0.0.1:8317
//
// Endpoints:
//
//	GET  /v1/models
//	GET  /v1/models/{id}         (single-model detail)
//	POST /v1/chat/completions    (OpenAI Chat Completion)
//	POST /v1/messages            (Anthropic Messages)
//	POST /v1/responses           (OpenAI Responses — codex CLI)
//	POST /v1/completions         (OpenAI legacy text completion)
//
// The proxy reads Cursor auth from Cursor IDE's SQLite storage (macOS default).
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/simcache"
	"github.com/router-for-me/cursor-proto/translator"
)

// ---------- OpenAI schemas ----------

type openaiChatRequest struct {
	Model         string               `json:"model"`
	Messages      []openaiMessage      `json:"messages"`
	Stream        bool                 `json:"stream"`
	Tools         []openaiTool         `json:"tools"`
	StreamOptions *openaiStreamOptions `json:"stream_options"`
}

// openaiStreamOptions mirrors OpenAI's `stream_options` object. Today only
// `include_usage` is supported; when true, we emit a final usage-only
// chunk (choices: []) before `data: [DONE]`.
type openaiStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type openaiMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// openaiTool matches the OpenAI Chat Completion `tools[]` shape. Only
// `type: "function"` is supported today; other types (e.g. code_interpreter)
// are ignored with a debug log.
type openaiTool struct {
	Type     string              `json:"type"`
	Function *openaiToolFunction `json:"function"`
}

type openaiToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

// ---------- Anthropic schemas ----------

type anthropicMessagesRequest struct {
	Model    string             `json:"model"`
	System   any                `json:"system"`
	Messages []anthropicMessage `json:"messages"`
	Stream   bool               `json:"stream"`
	Tools    []anthropicTool    `json:"tools"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// ---------- main ----------

func main() {
	addr := flag.String("addr", "127.0.0.1:8317", "listen address")
	apiKeysFlag := flag.String("api-keys", "", "comma-separated API keys required in Authorization: Bearer header; falls back to $"+apiKeysEnv+" when unset")
	tokenFile := flag.String("token-file", "", "path to account JSON (overrides IDE SQLite lookup); "+
		"env CURSOR_PROXY_ACCOUNT_FILE is used when this flag is empty")
	simulateCache := flag.Bool("simulate-cache", true, "enable local prompt-cache simulator; env CURSOR_PROXY_SIMULATE_CACHE=false disables it")
	cacheTTL := flag.String("cache-ttl", "10m", "simulator entry TTL (duration string)")
	cacheSize := flag.Int("cache-size", 1000, "simulator max entries")
	flag.Parse()

	// Env override for the on/off toggle. Any value that parses as boolean is
	// respected; an unparseable value falls back to the flag default so a
	// typo doesn't silently disable the simulator.
	if v := os.Getenv("CURSOR_PROXY_SIMULATE_CACHE"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			*simulateCache = b
		}
	}

	tokenPath := *tokenFile
	if tokenPath == "" {
		tokenPath = os.Getenv("CURSOR_PROXY_ACCOUNT_FILE")
	}

	var acc *auth.Account
	if tokenPath != "" {
		a, err := auth.LoadAccount(tokenPath)
		if err != nil {
			log.Fatalf("load account from %s: %v", tokenPath, err)
		}
		acc = a
	} else {
		acc = loadAccountFromIDE()
	}

	c := executor.NewClient(acc)
	c.API3 = c.API2 // chat also lives on api2

	apiKeys := LoadAPIKeys(*apiKeysFlag)

	var cacheStore *simcache.Store
	if *simulateCache {
		ttl := parseCacheTTL(*cacheTTL)
		cacheStore = simcache.New(ttl, *cacheSize)
		log.Printf("[proxy] sim-cache enabled: ttl=%s size=%d", ttl, *cacheSize)
	} else {
		log.Printf("[proxy] sim-cache disabled (real Cursor cache_read numbers pass through)")
	}

	log.Printf("[proxy] cursor account loaded: email=%s", acc.Email)
	log.Printf("[proxy] listening on http://%s", *addr)
	if len(apiKeys) > 0 {
		log.Printf("[proxy] api-key auth enabled: %d key(s) configured", len(apiKeys))
	} else {
		log.Printf("[proxy] api-key auth disabled (set -api-keys or $%s to enable)", apiKeysEnv)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/models", modelsHandler(c))
	// GET /v1/models/{id} — single-model detail. Registered with an
	// explicit GET pattern so ServeMux distinguishes it from the list.
	mux.HandleFunc("GET /v1/models/{id}", modelDetailHandler(c))
	mux.HandleFunc("/v1/usage", usageHandler(c))
	mux.HandleFunc("/v1/usage/prometheus", usagePrometheusHandler(c))
	mux.HandleFunc("/v1/chat/completions", openaiChatHandler(c, cacheStore))
	mux.HandleFunc("/v1/messages", anthropicMessagesHandler(c, cacheStore))
	mux.HandleFunc("/v1/responses", responsesHandler(c, cacheStore))
	mux.HandleFunc("/v1/completions", completionsHandler(c, cacheStore))

	handler := RequireAPIKeys(apiKeys, mux)
	log.Fatal(http.ListenAndServe(*addr, handler))
}

// ---------- /v1/models ----------

func modelsHandler(c *executor.Client) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := c.ListModels()
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		list := []map[string]any{}
		for _, m := range resp.Models {
			list = append(list, map[string]any{
				"id":       m.GetName(),
				"object":   "model",
				"owned_by": "cursor",
			})
		}
		w.Header().Set("content-type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": list})
	}
}

// ---------- /v1/chat/completions (OpenAI) ----------

func openaiChatHandler(c *executor.Client, cacheStore *simcache.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req openaiChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}

		systemPrompt := ""
		convTurns := make([]openaiMessage, 0, len(req.Messages))
		for _, m := range req.Messages {
			if m.Role == "system" {
				if systemPrompt != "" {
					systemPrompt += "\n"
				}
				systemPrompt += m.Content
				continue
			}
			convTurns = append(convTurns, m)
		}

		lastUserIdx := -1
		for i := len(convTurns) - 1; i >= 0; i-- {
			if convTurns[i].Role == "user" {
				lastUserIdx = i
				break
			}
		}
		if lastUserIdx < 0 {
			http.Error(w, "no user message", 400)
			return
		}
		userText := convTurns[lastUserIdx].Content
		history := make([]executor.HistoryTurn, 0, lastUserIdx)
		for _, m := range convTurns[:lastUserIdx] {
			if m.Role != "user" && m.Role != "assistant" {
				continue
			}
			history = append(history, executor.HistoryTurn{Role: m.Role, Content: m.Content})
		}

		// Ask the simulator whether it has seen this stable prefix before.
		// The result is consulted after RunChat to rewrite `cached_tokens`.
		prefix := prefixFromOpenAI(strings.TrimSpace(systemPrompt), history)
		decision := decideSimCache(cacheStore, prefix)

		tools := convertOpenAITools(req.Tools)
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

		if req.Stream {
			// Streaming responses commit headers before we can inspect the
			// real cache_read from Cursor, so the header is set from the
			// simulator's pre-stream view (real / simulated). See docs.
			w.Header().Set("x-cursor-cache-source", decision.headerBeforeStream())
			includeUsage := req.StreamOptions != nil && req.StreamOptions.IncludeUsage
			streamOpenAI(w, req.Model, events, includeUsage, decision)
			return
		}
		nonStreamOpenAI(w, req.Model, events, decision)
	}
}

// convertOpenAITools flattens the OpenAI tools[] wrapper into
// executor.ToolDefinition. Non-function entries and entries missing a name
// are skipped.
func convertOpenAITools(in []openaiTool) []executor.ToolDefinition {
	if len(in) == 0 {
		return nil
	}
	out := make([]executor.ToolDefinition, 0, len(in))
	for _, t := range in {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Function == nil || t.Function.Name == "" {
			continue
		}
		out = append(out, executor.ToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	return out
}

// convertAnthropicTools converts Anthropic-style `tools[]` into
// executor.ToolDefinition.
func convertAnthropicTools(in []anthropicTool) []executor.ToolDefinition {
	if len(in) == 0 {
		return nil
	}
	out := make([]executor.ToolDefinition, 0, len(in))
	for _, t := range in {
		if t.Name == "" {
			continue
		}
		out = append(out, executor.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return out
}

func streamOpenAI(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, includeUsage bool, decision simCacheDecision) {
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	flusher, _ := w.(http.Flusher)

	tr := translator.NewOpenAIStreamWriter(model)
	tr.IncludeUsage = includeUsage
	assistantSent := ""
	sawTurnEnd := false
	for ev := range events {
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			delta := diffSuffix(assistantSent, blob.AssistantText)
			if delta != "" {
				assistantSent = blob.AssistantText
				payload := tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})
				w.Write(payload)
				if flusher != nil {
					flusher.Flush()
				}
			}
			continue
		}
		trEv := translator.FromServerMessage(ev.Server)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventToolCallStarted, translator.EventToolCallDelta:
			if payload := tr.Encode(trEv); len(payload) > 0 {
				w.Write(payload)
				if flusher != nil {
					flusher.Flush()
				}
			}
		case translator.EventTurnEnded:
			sawTurnEnd = true
			// Rewrite usage before handing the event to the writer. Anthropic
			// cache-creation marking is skipped here (OpenAI schema has no
			// equivalent field).
			decision.applyToUsage(trEv.Usage, false)
			if payload := tr.Encode(trEv); len(payload) > 0 {
				w.Write(payload)
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
	}
	// If a tool call arrived but the server never sent turn_ended (typical
	// when Cursor is waiting on a BidiAppend tool result), synthesize a
	// finish_reason=tool_calls terminator so OpenAI clients see a valid stop.
	if !sawTurnEnd && tr.SawToolCall {
		if payload := tr.Encode(&translator.Event{Kind: translator.EventTurnEnded}); len(payload) > 0 {
			w.Write(payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}
	if payload := tr.FinalUsageFrame(); len(payload) > 0 {
		w.Write(payload)
		if flusher != nil {
			flusher.Flush()
		}
	}
	w.Write(tr.FinalDone())
	if flusher != nil {
		flusher.Flush()
	}
}

func nonStreamOpenAI(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision) {
	acc := translator.NonStreamingAccumulator{Model: model}
	for ev := range events {
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
		case translator.EventToolCallStarted:
			acc.Consume(trEv)
		case translator.EventTurnEnded:
			acc.Usage = trEv.Usage
			acc.FinishStop = true
		}
	}
	// Non-streaming: we can see Cursor's real cache_read before writing, so
	// set the accurate three-state header (real / simulated / mixed).
	var realCacheRead int64
	if acc.Usage != nil {
		realCacheRead = acc.Usage.CacheReadTokens
	}
	w.Header().Set("x-cursor-cache-source", decision.headerAfter(realCacheRead))
	decision.applyToUsage(acc.Usage, false)
	w.Header().Set("content-type", "application/json")
	w.Write(acc.Response("chatcmpl-" + auth.GenerateSessionID()))
}

func diffSuffix(sent, full string) string {
	if sent == "" {
		return full
	}
	if len(full) > len(sent) && full[:len(sent)] == sent {
		return full[len(sent):]
	}
	return full
}

// ---------- /v1/messages (Anthropic) ----------

func anthropicMessagesHandler(c *executor.Client, cacheStore *simcache.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req anthropicMessagesRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		systemPrompt := flattenAnthropicSystem(req.System)
		lastUserIdx := -1
		for i := len(req.Messages) - 1; i >= 0; i-- {
			if req.Messages[i].Role == "user" {
				lastUserIdx = i
				break
			}
		}
		if lastUserIdx < 0 {
			http.Error(w, "no user message", 400)
			return
		}
		userText := flattenAnthropicContent(req.Messages[lastUserIdx].Content)
		history := make([]executor.HistoryTurn, 0, lastUserIdx)
		for _, m := range req.Messages[:lastUserIdx] {
			if m.Role != "user" && m.Role != "assistant" {
				continue
			}
			history = append(history, executor.HistoryTurn{
				Role:    m.Role,
				Content: flattenAnthropicContent(m.Content),
			})
		}

		prefix := prefixFromOpenAI(strings.TrimSpace(systemPrompt), history)
		decision := decideSimCache(cacheStore, prefix)

		tools := convertAnthropicTools(req.Tools)
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

		if req.Stream {
			w.Header().Set("x-cursor-cache-source", decision.headerBeforeStream())
			streamAnthropic(w, req.Model, events, decision)
			return
		}
		nonStreamAnthropic(w, req.Model, events, decision)
	}
}

func streamAnthropic(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision) {
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no")
	flusher, _ := w.(http.Flusher)

	tr := translator.NewAnthropicStreamWriter(model)
	assistantSent := ""
	var lastUsage *translator.Usage
	for ev := range events {
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			delta := diffSuffix(assistantSent, blob.AssistantText)
			if delta != "" {
				assistantSent = blob.AssistantText
				w.Write(tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta}))
				if flusher != nil {
					flusher.Flush()
				}
			}
			continue
		}
		trEv := translator.FromServerMessage(ev.Server)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventToolCallStarted, translator.EventToolCallDelta, translator.EventToolCallCompleted:
			if payload := tr.Encode(trEv); len(payload) > 0 {
				w.Write(payload)
				if flusher != nil {
					flusher.Flush()
				}
			}
		case translator.EventTurnEnded:
			lastUsage = trEv.Usage
		}
	}
	// Anthropic streaming: on a miss, advertise cache_creation_input_tokens
	// so the Anthropic-style prompt-cache lifecycle is visible. On a hit,
	// rewrite cache_read_input_tokens to max(real, simulated).
	decision.applyToUsage(lastUsage, true)
	end := &translator.Event{Kind: translator.EventTurnEnded, Usage: lastUsage}
	w.Write(tr.Encode(end))
	if flusher != nil {
		flusher.Flush()
	}
}

func nonStreamAnthropic(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision) {
	assistantText := ""
	var usage *translator.Usage
	var toolUses []map[string]any
	for ev := range events {
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			assistantText = blob.AssistantText
			continue
		}
		trEv := translator.FromServerMessage(ev.Server)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventToolCallStarted:
			var input any = map[string]any{}
			if trEv.ToolArgsDelta != "" {
				var parsed any
				if err := json.Unmarshal([]byte(trEv.ToolArgsDelta), &parsed); err == nil {
					input = parsed
				}
			}
			toolUses = append(toolUses, map[string]any{
				"type":  "tool_use",
				"id":    trEv.ToolCallID,
				"name":  trEv.ToolName,
				"input": input,
			})
		case translator.EventTurnEnded:
			usage = trEv.Usage
		}
	}
	content := []map[string]any{}
	if assistantText != "" {
		content = append(content, map[string]any{"type": "text", "text": assistantText})
	}
	for _, tu := range toolUses {
		content = append(content, tu)
	}
	stopReason := "end_turn"
	if len(toolUses) > 0 {
		stopReason = "tool_use"
	}
	var realCacheRead int64
	if usage != nil {
		realCacheRead = usage.CacheReadTokens
	}
	w.Header().Set("x-cursor-cache-source", decision.headerAfter(realCacheRead))
	decision.applyToUsage(usage, true)
	resp := map[string]any{
		"id":            "msg_" + auth.GenerateSessionID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
	}
	if usage != nil {
		resp["usage"] = translator.BuildAnthropicUsage(usage)
	}
	w.Header().Set("content-type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func flattenAnthropicSystem(s any) string {
	switch v := s.(type) {
	case string:
		return v
	case []any:
		out := ""
		for _, block := range v {
			b, _ := block.(map[string]any)
			if b == nil {
				continue
			}
			if t, _ := b["text"].(string); t != "" {
				if out != "" {
					out += "\n"
				}
				out += t
			}
		}
		return out
	}
	return ""
}

func flattenAnthropicContent(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		out := ""
		for _, block := range v {
			b, _ := block.(map[string]any)
			if b == nil {
				continue
			}
			if t, _ := b["text"].(string); t != "" {
				if out != "" {
					out += "\n"
				}
				out += t
			}
		}
		return out
	}
	return ""
}

// ---------- auth loading ----------

func loadAccountFromIDE() *auth.Account {
	dbPath := os.Getenv("HOME") + "/Library/Application Support/Cursor/User/globalStorage/state.vscdb"
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var access, email string
	if err := db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken'`).Scan(&access); err != nil {
		log.Fatalf("no accessToken: %v", err)
	}
	_ = db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/cachedEmail'`).Scan(&email)

	machineID, _ := auth.GetMachineID()
	macID, _ := auth.GetMacMachineID()
	return &auth.Account{
		Email:        email,
		AccessToken:  access,
		MachineID:    machineID,
		MacMachineID: macID,
	}
}
