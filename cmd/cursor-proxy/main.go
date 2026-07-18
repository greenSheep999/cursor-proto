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
//	POST /v1/messages/count_tokens (heuristic estimator)
//	POST /v1/responses           (OpenAI Responses — codex CLI)
//	POST /v1/completions         (OpenAI legacy text completion)
//	GET  /v1beta/models          (Gemini SDK)
//	POST /v1beta/models/{model}:generateContent
//	POST /v1beta/models/{model}:streamGenerateContent
//
// The proxy reads Cursor auth from Cursor IDE's SQLite storage (macOS default).
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"
	"strconv"
	"strings"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/simcache"
	"github.com/router-for-me/cursor-proto/executor/transport"
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
	// OutputConfig.Effort maps onto Cursor's tier suffix (-low / -medium /
	// -high). Anthropic clients that don't set it fall back to whatever
	// tier is already baked into the model name.
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	// Thinking.BudgetTokens (Extended Thinking) — presence means the
	// client wants reasoning; we forward this to Cursor via the model
	// tier suffix, using "high" when a budget is set.
	Thinking *anthropicThinking `json:"thinking,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens"`
}

type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// anthropicTool covers both "custom" tools (user-defined, with input_schema)
// and Anthropic's server-side tools (web_search_20250305, computer_20241022,
// bash_20250124, text_editor_20250124, code_execution_20250825, …), which
// are identified by their `type` field rather than by carrying an
// input_schema. Cursor's upstream doesn't run any of these server tools —
// they execute inside Anthropic's own infrastructure — so we surface a
// clear 400 when we see one instead of silently dropping it (which used
// to make clients hang waiting for a tool_use block that would never come).
type anthropicTool struct {
	Type        string         `json:"type"`
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"input_schema"`
}

// isAnthropicServerTool returns true when a tool entry describes one of
// Anthropic's server-side tools (identified by a versioned `type` string
// like "web_search_20250305"). Custom tools either have Type=="" or
// Type=="custom" per the Messages API spec.
func isAnthropicServerTool(t anthropicTool) bool {
	if t.Type == "" || t.Type == "custom" {
		return false
	}
	// Any typed tool that isn't a user-defined "custom" one is server-side
	// as of the current Anthropic API (2025-2026). Being permissive here
	// keeps us forward-compatible if Anthropic adds new server tools.
	return true
}

// ---------- main ----------

// ProtoVersion is the release tag this binary was built from (e.g.
// "cursor3.11/v0.2.1"). Set at link time via
//
//	-ldflags="-X main.ProtoVersion=${GITHUB_REF_NAME}"
//
// See .github/workflows/release.yml. Local `go build` / `go run` keeps
// the "dev" fallback so /v1/proxy-info surfaces the lack of a pinned
// release.
var ProtoVersion = "dev"

func main() {
	addr := flag.String("addr", "127.0.0.1:8317", "listen address")
	apiKeysFlag := flag.String("api-keys", "", "comma-separated API keys required in Authorization: Bearer header; falls back to $"+apiKeysEnv+" when unset")
	tokenFile := flag.String("token-file", "", "path to account JSON (overrides IDE SQLite lookup); "+
		"env CURSOR_PROXY_ACCOUNT_FILE is used when this flag is empty")
	upstreamProxy := flag.String("upstream-proxy", "", "outbound proxy for calls to Cursor's backend "+
		"(http://[user:pass@]host:port, https://…, or socks5://…). Falls back to $HTTPS_PROXY / $HTTP_PROXY. "+
		"Required when your account is region-gated (Cursor returns ERROR_UNSUPPORTED_REGION on the "+
		"claude-*, gpt-*, gemini-* families without a non-CN egress).")
	simulateCache := flag.Bool("simulate-cache", true, "enable local prompt-cache simulator; env CURSOR_PROXY_SIMULATE_CACHE=false disables it")
	cacheTTL := flag.String("cache-ttl", "10m", "simulator entry TTL (duration string)")
	cacheSize := flag.Int("cache-size", 1000, "simulator max entries")
	httpVersion := flag.String("http-version", "auto",
		"upstream HTTP protocol version: auto | http1.1 | http1.0. "+
			"'auto' negotiates h2 via ALPN and works on healthy networks. "+
			"Downgrade only when a corporate proxy, TLS-inspecting VPN, "+
			"or older SSL appliance in front of you mangles h2 SSE "+
			"streams (symptom: 'unexpected EOF' partway through a "+
			"generation) AND that middlebox translates h2 back to h1 "+
			"for you. Direct connections to Cursor's backend REQUIRE "+
			"h2 — forcing http1.1 without a middlebox in between will "+
			"produce a 502 with a malformed HTTP response. Falls back "+
			"to $CURSOR_PROXY_HTTP_VERSION when unset.")
	cursorAPIKey := flag.String("cursor-api-key", "",
		"Cursor dashboard-issued API key (crsr_...) used by the "+
			"agent-mode Node runner. Falls back to $CURSOR_API_KEY. "+
			"When unset, /v1/agents/* endpoints return 503 and agent "+
			"mode is disabled — wire mode continues to work.")
	nodeRunnerPath := flag.String("node-runner", "",
		"Path to the compiled node-runner/dist/index.js. When set "+
			"together with -cursor-api-key (or its env), spawns a Node "+
			"child process on startup that exposes /v1/agents/*. Falls "+
			"back to $CURSOR_PROXY_NODE_RUNNER. Leave empty to disable "+
			"agent mode entirely (wire mode still works). See "+
			"docs/sdk-integration.md.")
	nodeBinary := flag.String("node-binary", "",
		"Path to the `node` executable used to run the SDK runner. "+
			"Defaults to `node` on PATH. Only consulted when -node-runner is set.")
	showVersion := flag.Bool("version", false, "print JSON with Cursor line, impersonated version, commit, release hash, and proto tag, then exit")
	flag.Parse()

	if *showVersion {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(CurrentProxyInfo())
		return
	}

	// Resolve upstream proxy: -upstream-proxy > $CURSOR_PROXY_UPSTREAM > $HTTPS_PROXY > $HTTP_PROXY.
	// Setting it here (before executor.NewClient) is what actually plumbs the
	// proxy through to the Connect stream — Go's http.DefaultTransport reads
	// HTTPS_PROXY on init, but we set it programmatically to make the flag
	// deterministic across shells and containers.
	if strings.TrimSpace(*upstreamProxy) == "" {
		for _, k := range []string{"CURSOR_PROXY_UPSTREAM", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			if v := strings.TrimSpace(os.Getenv(k)); v != "" {
				*upstreamProxy = v
				break
			}
		}
	}
	if p := strings.TrimSpace(*upstreamProxy); p != "" {
		// Setting the env is enough: net/http.DefaultTransport reads
		// HTTPS_PROXY via httpproxy.FromEnvironment on every Do(), so this
		// affects both the RunSSE stream and BidiAppend.
		_ = os.Setenv("HTTPS_PROXY", p)
		_ = os.Setenv("HTTP_PROXY", p)
		log.Printf("[proxy] upstream proxy: %s", p)
	}

	// Resolve HTTP version: -http-version > $CURSOR_PROXY_HTTP_VERSION.
	// The flag's own default is "auto", which is treated as "look at
	// the env before committing" so operators can override via env
	// without having to also pass the flag. Any parse error fails
	// loudly at startup — silently falling back to auto would hide
	// a typo that leaves the operator's middlebox workaround inactive.
	if strings.TrimSpace(strings.ToLower(*httpVersion)) == "auto" {
		if v := strings.TrimSpace(os.Getenv("CURSOR_PROXY_HTTP_VERSION")); v != "" {
			*httpVersion = v
		}
	}
	httpVer, err := transport.Parse(*httpVersion)
	if err != nil {
		log.Fatalf("bad -http-version: %v", err)
	}
	SetCurrentHTTPVersion(httpVer)

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
	var ideReloader func() *auth.Account
	if tokenPath != "" {
		a, err := auth.LoadAccount(tokenPath)
		if err != nil {
			log.Fatalf("load account from %s: %v", tokenPath, err)
		}
		acc = a
	} else {
		// IDE-backed account: install an mtime reloader so account switches
		// in the running Cursor IDE take effect on the next upstream call
		// (no proxy restart). See makeIDEAccountReloader for the contract.
		dbPath := ideDBPath()
		acc = loadAccountFromIDE()
		var startMTime time.Time
		if info, err := os.Stat(dbPath); err == nil {
			startMTime = info.ModTime()
		}
		ideReloader = makeIDEAccountReloader(dbPath, startMTime)
	}

	c := executor.NewClient(acc, executor.WithHTTPVersion(httpVer))
	c.API3 = c.API2 // chat also lives on api2
	if ideReloader != nil {
		c.AccountReloader = ideReloader
	}

	apiKeys := LoadAPIKeys(*apiKeysFlag)

	var cacheStore *simcache.Store
	if *simulateCache {
		ttl := parseCacheTTL(*cacheTTL)
		cacheStore = simcache.New(ttl, *cacheSize)
		log.Printf("[proxy] sim-cache enabled: ttl=%s size=%d", ttl, *cacheSize)
	} else {
		log.Printf("[proxy] sim-cache disabled (real Cursor cache_read numbers pass through)")
	}
	// Let /v1/capabilities report the actual runtime state of the
	// simcache toggle rather than the compile-time default.
	setSimCacheEnabled(*simulateCache)

	log.Printf("[proxy] cursor account loaded: email=%s", acc.Email)
	SetWireAccountEmail(acc.Email)
	log.Printf("[proxy] upstream HTTP: %s", httpVer)

	// Agent mode: optional. Spawn the Node SDK runner if both a
	// runner path and an API key are configured. Either missing =>
	// wire-only build; /v1/agents/* returns 503 with a clear body.
	// Wire mode is unaffected either way.
	if sup := maybeStartAgentSupervisor(*nodeRunnerPath, *nodeBinary, *cursorAPIKey); sup != nil {
		setAgentSupervisor(sup)
	}

	log.Printf("[proxy] listening on http://%s", *addr)
	if len(apiKeys) > 0 {
		log.Printf("[proxy] api-key auth enabled: %d key(s) configured", len(apiKeys))
	} else {
		log.Printf("[proxy] api-key auth disabled (set -api-keys or $%s to enable)", apiKeysEnv)
	}

	mux := http.NewServeMux()
	// /v1/proxy-info reports the Cursor line / impersonated version /
	// release hash / proto tag. Registered BEFORE RequireAPIKeys wraps
	// the mux so the endpoint is always reachable — cursor2api's sidecar
	// supervisor probes this before wiring any API key.
	mux.HandleFunc("GET /v1/proxy-info", proxyInfoHandler)
	// /v1/capabilities + /v1/introspect/* — read-only observation
	// endpoints. Same rationale as /v1/proxy-info: no secrets, and
	// cursor2api probes them before wiring the API key.
	mux.HandleFunc("GET /v1/capabilities", capabilitiesHandler)
	mux.HandleFunc("GET /v1/introspect/recent-tools", recentToolsHandler)
	mux.HandleFunc("GET /v1/introspect/recent-mcp-servers", recentMCPServersHandler)

	// /v1/agents/* — agent mode (see docs/sdk-integration.md).
	// These DO go through the -api-keys gate (unlike proxy-info
	// and introspect). When agent mode is disabled (no supervisor
	// configured), each handler returns 503 with a clear body.
	mux.HandleFunc("/v1/agents", agentsRootHandler)
	mux.HandleFunc("/v1/agents/{id}", agentsItemHandler)
	mux.HandleFunc("POST /v1/agents/{id}/runs", agentsSendHandler)
	mux.HandleFunc("POST /v1/agents/{id}/runs/stream", agentsSendStreamHandler)
	mux.HandleFunc("GET /v1/agents/{id}/runs/{run_id}/stream", agentsStreamHandler)
	mux.HandleFunc("POST /v1/agents/{id}/runs/{run_id}/cancel", agentsCancelHandler)

	mux.HandleFunc("/v1/models", modelsHandler(c))
	// GET /v1/models/{id} — single-model detail. Registered with an
	// explicit GET pattern so ServeMux distinguishes it from the list.
	mux.HandleFunc("GET /v1/models/{id}", modelDetailHandler(c))
	mux.HandleFunc("/v1/usage", usageHandler(c))
	mux.HandleFunc("/v1/usage/prometheus", usagePrometheusHandler(c))
	// GET /v1/usage/events — paginated per-request event log.
	// Backs cursor2api's /usage table. See
	// docs/upstream-issues/usage-events-list.md in cursor2api for
	// the downstream spec.
	mux.HandleFunc("GET /v1/usage/events", usageEventsHandler(c))
	mux.HandleFunc("/v1/chat/completions", openaiChatHandler(c, cacheStore))
	mux.HandleFunc("/v1/messages", anthropicMessagesHandler(c, cacheStore))
	mux.HandleFunc("/v1/messages/count_tokens", countTokensHandler)
	mux.HandleFunc("/v1/responses", responsesHandler(c, cacheStore))
	mux.HandleFunc("/v1/completions", completionsHandler(c, cacheStore))

	// Gemini native surface (used by @google/gemini-cli, google-generativeai).
	// Route splits on `{model}:{method}` internally.
	mux.HandleFunc("GET /v1beta/models", geminiModelsListHandler(c))
	mux.HandleFunc("POST /v1beta/models/{tail}", geminiRouter(c, cacheStore))

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
		// Record declared tools in the ring buffer BEFORE any
		// validation — we want the observation to include requests
		// that end in 400, since "tried to use tool X but got
		// rejected" is exactly the signal downstream wants for
		// diagnostics.
		clientToolNames := extractOpenAIToolNames(req.Tools)
		recordToolsFromRequest(clientToolNames)

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
			streamOpenAI(w, req.Model, events, includeUsage, decision, clientToolNames)
			return
		}
		nonStreamOpenAI(w, req.Model, events, decision, clientToolNames)
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
// executor.ToolDefinition. Returns the converted list plus, if any
// server-side tools were rejected, a description of the first offender so
// the handler can return a helpful 400 instead of dropping tools silently.
func convertAnthropicTools(in []anthropicTool) (out []executor.ToolDefinition, unsupportedType string) {
	if len(in) == 0 {
		return nil, ""
	}
	out = make([]executor.ToolDefinition, 0, len(in))
	for _, t := range in {
		if isAnthropicServerTool(t) {
			if unsupportedType == "" {
				unsupportedType = t.Type
			}
			continue
		}
		if t.Name == "" {
			continue
		}
		out = append(out, executor.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return out, unsupportedType
}

func streamOpenAI(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, includeUsage bool, decision simCacheDecision, clientToolNames []string) {
	// Defer committing SSE headers until we've either seen a data frame or an
	// error trailer, so a fast-fail model-gate error can be surfaced as a
	// proper HTTP status code rather than a 200 with empty content.
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

	tr := translator.NewOpenAIStreamWriter(model)
	tr.IncludeUsage = includeUsage
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
			writeSSE(tr.Encode(trEv))
		case translator.EventTurnEnded:
			sawTurnEnd = true
			decision.applyToUsage(trEv.Usage, false)
			writeSSE(tr.Encode(trEv))
		}
	}
	// If we've written nothing yet and the trailer reported an error, respond
	// with a proper HTTP error instead of an empty SSE stream. Composer /
	// grok / kimi flows always emit data frames first, so this branch is
	// scoped to hard-fail cases (region gates, auth, etc.).
	if !headersWritten && trailerErr != nil {
		writeUpstreamOpenAIError(w, trailerErr)
		return
	}
	// If a tool call arrived but the server never sent turn_ended (typical
	// when Cursor is waiting on a BidiAppend tool result), synthesize a
	// finish_reason=tool_calls terminator so OpenAI clients see a valid stop.
	if !sawTurnEnd && tr.SawToolCall {
		writeSSE(tr.Encode(&translator.Event{Kind: translator.EventTurnEnded}))
	}
	writeSSE(tr.FinalUsageFrame())
	commit()
	w.Write(tr.FinalDone())
	if flusher != nil {
		flusher.Flush()
	}
}

func nonStreamOpenAI(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision, clientToolNames []string) {
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
		trEv := translateEvent(ev.Server, clientToolNames)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			acc.Consume(trEv)
		case translator.EventToolCallStarted:
			acc.Consume(trEv)
		case translator.EventTurnEnded:
			acc.Usage = trEv.Usage
			acc.FinishStop = true
		}
	}
	// Surface hard upstream errors (region gate, auth, etc.) as proper HTTP
	// errors instead of an empty 200 with `content:""`.
	if trailerErr != nil && acc.Text == "" && len(acc.ToolCalls) == 0 {
		writeUpstreamOpenAIError(w, trailerErr)
		return
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
		// Feed the introspection ring. See openaiChatHandler for
		// why we record before validation.
		clientToolNames := extractAnthropicToolNames(req.Tools)
		recordToolsFromRequest(clientToolNames)

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

		// Reject server-side Anthropic tools (web_search_20250305 etc.)
		// with a 400 instead of silently dropping them — clients rely on
		// tool_use blocks that Cursor's upstream will never emit for
		// these, and the mismatch used to hang the stream.
		tools, unsupportedTool := convertAnthropicTools(req.Tools)
		if unsupportedTool != "" {
			writeAnthropicError(w, http.StatusBadRequest,
				"invalid_request_error",
				fmt.Sprintf("Anthropic server-side tool %q is not supported when routed through Cursor. "+
					"Cursor runs its own agent tools; only client-side custom tools (type=\"custom\" or omitted) are proxied.",
					unsupportedTool))
			return
		}

		// Resolve the target Cursor tier. Priority:
		//   1. Explicit tier suffix already on req.Model (e.g. "claude-sonnet-5-high") wins.
		//   2. output_config.effort — Anthropic's official knob.
		//   3. thinking.budget_tokens (presence maps to "high").
		//   4. Whatever the model alias table gives us (see canonicalizeAnthropicModel).
		effort := ""
		if req.OutputConfig != nil {
			effort = strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort))
		}
		if effort == "" && req.Thinking != nil && req.Thinking.BudgetTokens > 0 {
			effort = "high"
		}
		modelForCursor := canonicalizeAnthropicModel(req.Model, effort)

		events, err := c.RunChat(r.Context(), &executor.ChatRequest{
			Model:              modelForCursor,
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
			streamAnthropic(w, req.Model, events, decision, clientToolNames)
			return
		}
		nonStreamAnthropic(w, req.Model, events, decision, clientToolNames)
	}
}

// writeAnthropicError sends an Anthropic-shaped error body. The Messages API
// returns {"type":"error","error":{"type":"<type>","message":"..."}}.
func writeAnthropicError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

// canonicalizeAnthropicModel maps Anthropic's official bare model names
// (e.g. "claude-sonnet-4-5-20250929", "claude-opus-4-5") to the Cursor
// tier-suffixed names the upstream expects. When `effort` is set it wins
// over any tier already in the alias — this lets output_config.effort
// override the client's model string in a single normalized place.
//
// Bare names without an alias (or names already carrying a tier suffix)
// pass through unchanged, so power users who type the Cursor name
// directly still work.
func canonicalizeAnthropicModel(model, effort string) string {
	m := strings.TrimSpace(model)
	if m == "" {
		return m
	}
	// Alias table: Anthropic bare name → Cursor tier-less base.
	// Kept intentionally small; extend as new Claude generations ship.
	aliases := map[string]string{
		// Sonnet 4.5 family
		"claude-sonnet-4-5":          "claude-4.5-sonnet",
		"claude-sonnet-4-5-20250929": "claude-4.5-sonnet",
		// Opus 4.5
		"claude-opus-4-5":          "claude-4.5-opus",
		"claude-opus-4-5-20250929": "claude-4.5-opus",
		// Sonnet 5 (Cursor-style tier root)
		"claude-sonnet-5": "claude-sonnet-5",
		// Haiku 4.5
		"claude-haiku-4-5":          "claude-4.5-haiku",
		"claude-haiku-4-5-20251001": "claude-4.5-haiku",
	}
	base := m
	wasAliased := false
	if canonical, ok := aliases[m]; ok {
		base = canonical
		wasAliased = true
	}
	// Only tier-suffix models that came in as Anthropic bare names (i.e.
	// the ones our alias table maps). Cursor-native names like `default`,
	// `composer-2.5`, `cursor-grok-4.5-medium`, or a Cursor tier root
	// like `claude-4.5-sonnet` that the caller typed directly are passed
	// through unchanged — the caller either knows what they want or the
	// name already carries its own tier. Appending `-medium` to
	// `default` would produce `default-medium`, which the Cursor backend
	// rejects as ERROR_BAD_MODEL_NAME.
	if !wasAliased {
		return base
	}
	if effort == "low" || effort == "medium" || effort == "high" {
		if !strings.HasSuffix(base, "-low") &&
			!strings.HasSuffix(base, "-medium") &&
			!strings.HasSuffix(base, "-high") &&
			!strings.HasSuffix(base, "-xhigh") &&
			!strings.HasSuffix(base, "-fast") {
			return base + "-" + effort
		}
	}
	return base
}

func streamAnthropic(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision, clientToolNames []string) {
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

	tr := translator.NewAnthropicStreamWriter(model)
	assistantSent := ""
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
			lastUsage = trEv.Usage
		}
	}
	if !headersWritten && trailerErr != nil {
		writeUpstreamAnthropicError(w, trailerErr)
		return
	}
	// Anthropic streaming: on a miss, advertise cache_creation_input_tokens
	// so the Anthropic-style prompt-cache lifecycle is visible. On a hit,
	// rewrite cache_read_input_tokens to max(real, simulated).
	decision.applyToUsage(lastUsage, true)
	end := &translator.Event{Kind: translator.EventTurnEnded, Usage: lastUsage}
	// A trailer error that arrived AFTER we'd already committed SSE
	// headers can't be turned into an HTTP status anymore, so surface it
	// on the message_delta as stop_reason="error". This keeps clients
	// from treating a partial stream as a successful end_turn.
	if trailerErr != nil {
		end.StopReason = "error"
	}
	writeSSE(tr.Encode(end))
}

func nonStreamAnthropic(w http.ResponseWriter, model string, events <-chan executor.ChatEvent, decision simCacheDecision, clientToolNames []string) {
	assistantText := ""
	var usage *translator.Usage
	var toolUses []map[string]any
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
			assistantText = blob.AssistantText
			continue
		}
		trEv := translateEvent(ev.Server, clientToolNames)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			assistantText += trEv.Text
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
	if trailerErr != nil && assistantText == "" && len(toolUses) == 0 {
		writeUpstreamAnthropicError(w, trailerErr)
		return
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

// flattenAnthropicContent projects an Anthropic content block list into a
// single text string that Cursor's chat protocol can consume as one turn.
//
// Anthropic content blocks come in several shapes; we preserve them all so
// the model sees a coherent conversation on turn 2+:
//
//   - text            → the text itself
//   - tool_use        → "[tool_use name=X input={...}]" so the model sees
//                       it already made this call and doesn't repeat it
//   - tool_result     → "[tool_result for tool_use_id=Y: <content>]" so
//                       the model sees the result of its previous call
//
// This is a fallback for the fact that Cursor's HistoryTurn is text-only;
// a proper fix (native tool_use / tool_result in the Cursor wire) would
// require expanding executor to carry structured tool history.
func flattenAnthropicContent(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		parts := make([]string, 0, len(v))
		for _, block := range v {
			b, _ := block.(map[string]any)
			if b == nil {
				continue
			}
			bt, _ := b["type"].(string)
			switch bt {
			case "text", "":
				if t, _ := b["text"].(string); t != "" {
					parts = append(parts, t)
				}
			case "tool_use":
				name, _ := b["name"].(string)
				id, _ := b["id"].(string)
				inputBytes, _ := json.Marshal(b["input"])
				parts = append(parts, fmt.Sprintf("[tool_use id=%s name=%s input=%s]",
					id, name, string(inputBytes)))
			case "tool_result":
				id, _ := b["tool_use_id"].(string)
				// tool_result.content is either a plain string or a list
				// of blocks (usually text). Flatten either shape.
				var contentStr string
				switch rc := b["content"].(type) {
				case string:
					contentStr = rc
				case []any:
					var sb strings.Builder
					for _, rb := range rc {
						rbm, _ := rb.(map[string]any)
						if t, _ := rbm["text"].(string); t != "" {
							if sb.Len() > 0 {
								sb.WriteString("\n")
							}
							sb.WriteString(t)
						}
					}
					contentStr = sb.String()
				}
				isErr, _ := b["is_error"].(bool)
				tag := "tool_result"
				if isErr {
					tag = "tool_result_error"
				}
				parts = append(parts, fmt.Sprintf("[%s tool_use_id=%s: %s]", tag, id, contentStr))
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

// ---------- auth loading ----------

// ideDBPath is the on-disk location of the Cursor IDE's SQLite state store
// on macOS. The IDE writes cursorAuth/accessToken + cachedEmail here every
// time the user signs in or switches accounts.
func ideDBPath() string {
	return os.Getenv("HOME") + "/Library/Application Support/Cursor/User/globalStorage/state.vscdb"
}

func loadAccountFromIDE() *auth.Account {
	acc, err := readAccountFromIDE(ideDBPath())
	if err != nil {
		log.Fatalf("%v", err)
	}
	return acc
}

// readAccountFromIDE snapshots the IDE's `state.vscdb` into cursor-proxy's
// user cache directory and reads the account token from the snapshot. We
// never open the IDE's live file — that used to show up in Sparkle's
// `lsof` scan on every point-release update and blocked Cursor from
// swapping the app bundle. See docs/upstream-issues/state-vscdb-copy-on-
// read.md for the full failure mode analysis.
func readAccountFromIDE(dbPath string) (*auth.Account, error) {
	snapshot, err := auth.SnapshotIDEDB(dbPath)
	if err != nil {
		return nil, fmt.Errorf("snapshot IDE db: %w", err)
	}
	db, err := sql.Open("sqlite3", "file:"+snapshot+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	defer db.Close()
	var access, email string
	if err := db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken'`).Scan(&access); err != nil {
		return nil, fmt.Errorf("no accessToken: %w", err)
	}
	_ = db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/cachedEmail'`).Scan(&email)

	machineID, _ := auth.GetMachineID()
	macID, _ := auth.GetMacMachineID()
	return &auth.Account{
		Email:        email,
		AccessToken:  access,
		MachineID:    machineID,
		MacMachineID: macID,
	}, nil
}

// makeIDEAccountReloader returns a closure the executor.Client can call
// before every upstream request to pick up account switches performed in
// the running Cursor IDE. It's a mtime-check (cheap: one stat syscall);
// the sqlite read only happens when the mtime advances.
//
// Zero-alloc happy path: same mtime → nil return → caller keeps its cached
// account. On a change: reads the DB, builds a fresh Account, refreshes
// session defaults, and returns it for atomic swap on the client.
//
// Errors are logged and treated as "keep current" — we don't want a
// transient sqlite lock (IDE is busy writing) to knock the proxy offline.
func makeIDEAccountReloader(dbPath string, initial time.Time) func() *auth.Account {
	var lastMTime = initial
	return func() *auth.Account {
		info, err := os.Stat(dbPath)
		if err != nil {
			return nil
		}
		if !info.ModTime().After(lastMTime) {
			return nil
		}
		acc, err := readAccountFromIDE(dbPath)
		if err != nil {
			log.Printf("[proxy] IDE sqlite changed but reload failed: %v", err)
			return nil
		}
		acc.FillSessionDefaults(time.Now())
		lastMTime = info.ModTime()
		log.Printf("[proxy] IDE account reloaded: email=%s", acc.Email)
		return acc
	}
}
