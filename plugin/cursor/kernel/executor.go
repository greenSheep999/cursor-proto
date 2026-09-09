package kernel

// Executor-side logic for the Cursor CPA plugin.
//
// The plugin advertises `openai` and `claude` as both input and output
// formats. That means CPA hands us `ExecutorRequest.Payload` already
// translated into one of those formats. The Cursor backend, however,
// speaks only its own protobuf/RunSSE flow (see executor/chat.go), so
// the plugin's job is:
//
//   1. Parse Payload into (system prompt, prior turns, current user
//      turn, tools, streaming flag).
//   2. Rebuild an `*auth.Account` from ExecutorRequest.StorageJSON.
//   3. Get-or-create an `*executor.Client` keyed by AuthID so the
//      session identifiers (checksum, client key, session id) stay
//      stable across calls.
//   4. Drive `Client.RunChat` and produce either a full response body
//      (non-streaming) or a series of SSE frames (streaming).
//
// The output shape mirrors what cmd/cursor-proxy already emits for the
// same protocol: OpenAI Chat Completion for `format=openai`, Anthropic
// Messages for `format=claude`.

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/transport"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
	"github.com/router-for-me/cursor-proto/translator"
)

// clientCache is a per-plugin-process cache of *executor.Client keyed
// by AuthID. Multiple concurrent RunChat calls against the same auth
// reuse the same client (and its stable session identifiers).
type clientCache struct {
	mu      sync.Mutex
	clients map[string]*executor.Client
}

// globalClientCache is intentionally package-level. The plugin is
// loaded once per host process, so the cache lifetime is tied to the
// plugin's lifetime.
var globalClientCache = &clientCache{clients: make(map[string]*executor.Client)}

const (
	chromiumSidecarURLEnv   = "CURSOR_CHROMIUM_SIDECAR_URL"
	chromiumSidecarTokenEnv = "CURSOR_CHROMIUM_SIDECAR_TOKEN"
)

// getClient returns a client for the given auth id, reusing the cache
// when possible. When StorageJSON changes for the same AuthID (a
// refreshed access token, for example) the caller passes in the new
// storage and we rebuild the client so the fresh tokens propagate.
func (c *clientCache) getClient(authID string, storage []byte) (*executor.Client, error) {
	if strings.TrimSpace(authID) == "" {
		return c.buildClient(storage)
	}
	c.mu.Lock()
	if existing, ok := c.clients[authID]; ok {
		if existing.Account != nil && existing.Account.AccessToken == accessTokenFromStorage(storage) {
			c.mu.Unlock()
			return existing, nil
		}
	}
	c.mu.Unlock()

	built, err := c.buildClient(storage)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.clients[authID] = built
	c.mu.Unlock()
	return built, nil
}

// buildClient parses the auth storage into an *auth.Account and wraps
// it in an executor.Client. FillSessionDefaults is invoked implicitly
// by executor.NewClient so per-process session identifiers are set.
func (c *clientCache) buildClient(storage []byte) (*executor.Client, error) {
	if len(storage) == 0 {
		return nil, errors.New("empty storage")
	}
	file, err := cpaformat.Unmarshal(storage)
	if err != nil {
		return nil, fmt.Errorf("parse storage: %w", err)
	}
	if errValidate := file.Validate(); errValidate != nil {
		return nil, fmt.Errorf("validate storage: %w", errValidate)
	}
	acc, err := file.ToAccount()
	if err != nil {
		return nil, fmt.Errorf("build account: %w", err)
	}
	// FillSessionDefaults regenerates non-persistent fields (client key,
	// session id, checksum). If any device identifiers are missing,
	// derive them so requests still look device-consistent.
	if strings.TrimSpace(acc.MachineID) == "" {
		if mid, errMid := auth.GetMachineID(); errMid == nil {
			acc.MachineID = mid
		}
	}
	if strings.TrimSpace(acc.MacMachineID) == "" {
		if mid, errMid := auth.GetMacMachineID(); errMid == nil {
			acc.MacMachineID = mid
		}
	}
	acc.FillSessionDefaults(time.Now())
	return newPluginExecutorClient(acc)
}

// newPluginExecutorClient is the one construction seam for live plugin
// traffic. Model discovery and chat execution both call it, so enabling the
// Chromium sidecar cannot produce a split-brain catalog/chat configuration.
func newPluginExecutorClient(acc *auth.Account) (*executor.Client, error) {
	options := []executor.Option{executor.WithHTTPVersion(transport.Http1_1)}
	if rawURL := strings.TrimSpace(os.Getenv(chromiumSidecarURLEnv)); rawURL != "" {
		option, err := executor.ChromiumSidecarOption(rawURL, os.Getenv(chromiumSidecarTokenEnv))
		if err != nil {
			return nil, fmt.Errorf("configure Chromium sidecar: %w", err)
		}
		options = append(options, option)
	}
	c := executor.NewClient(acc, options...)
	if strings.TrimSpace(os.Getenv(chromiumSidecarURLEnv)) == "" {
		c.API3 = c.API2
	}
	return c, nil
}

// accessTokenFromStorage decodes just the access_token field from a
// storage blob so we can detect refresh churn without allocating a
// full AuthFile. Errors return "" so we fall through to a rebuild.
func accessTokenFromStorage(storage []byte) string {
	if len(storage) == 0 {
		return ""
	}
	var probe struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(storage, &probe); err != nil {
		return ""
	}
	return probe.AccessToken
}

func storageExcludesModel(storage []byte, model string) (bool, error) {
	file, err := cpaformat.Unmarshal(storage)
	if err != nil {
		return false, fmt.Errorf("parse storage: %w", err)
	}
	model = strings.ToLower(strings.TrimSpace(model))
	for _, pattern := range file.ExcludedModels {
		if wildcardModelMatch(strings.ToLower(strings.TrimSpace(pattern)), model) {
			return true, nil
		}
	}
	return false, nil
}

func wildcardModelMatch(pattern, model string) bool {
	if pattern == "" {
		return false
	}
	if pattern == "*" {
		return true
	}
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == model
	}
	if parts[0] != "" {
		if !strings.HasPrefix(model, parts[0]) {
			return false
		}
		model = model[len(parts[0]):]
	}
	if parts[len(parts)-1] != "" {
		suffix := parts[len(parts)-1]
		if !strings.HasSuffix(model, suffix) {
			return false
		}
		model = model[:len(model)-len(suffix)]
	}
	for _, part := range parts[1 : len(parts)-1] {
		if part == "" {
			continue
		}
		index := strings.Index(model, part)
		if index < 0 {
			return false
		}
		model = model[index+len(part):]
	}
	return true
}

// executorRequest mirrors the ABI shape of pluginapi.ExecutorRequest.
// See docs/phase-8b-abi.md for the full field-by-field breakdown.
// Only the fields the executor actually consumes are declared;
// unknown fields are ignored.
type executorRequest struct {
	AuthID          string              `json:"AuthID"`
	AuthProvider    string              `json:"AuthProvider"`
	Model           string              `json:"Model"`
	Format          string              `json:"Format"`
	Stream          bool                `json:"Stream"`
	Alt             string              `json:"Alt"`
	Headers         map[string][]string `json:"Headers"`
	OriginalRequest []byte              `json:"OriginalRequest"`
	SourceFormat    string              `json:"SourceFormat"`
	Payload         []byte              `json:"Payload"`
	StorageJSON     []byte              `json:"StorageJSON"`
	AuthMetadata    map[string]any      `json:"AuthMetadata"`
	AuthAttributes  map[string]string   `json:"AuthAttributes"`
	StreamID        string              `json:"stream_id,omitempty"`
	HostCallbackID  string              `json:"host_callback_id,omitempty"`
	// Metadata may carry per-request extras (e.g. requested_model,
	// interceptor-set headers). Not consumed today.
	Metadata json.RawMessage `json:"Metadata,omitempty"`
}

// executorResponse mirrors pluginapi.ExecutorResponse. Payload is a
// base64-encoded []byte on the wire (Go's encoding/json default for
// []byte).
type executorResponse struct {
	Payload  []byte              `json:"Payload,omitempty"`
	Headers  map[string][]string `json:"Headers,omitempty"`
	Metadata map[string]any      `json:"Metadata,omitempty"`
}

// executorStreamResponse mirrors rpcExecutorStreamResponse. We always
// return an empty Chunks slice and drive chunks asynchronously via
// host.stream.emit — that gives CPA the true streaming shape it
// expects (chunks arrive as soon as Cursor produces them, not
// buffered).
type executorStreamResponse struct {
	Headers map[string][]string `json:"headers,omitempty"`
	// Chunks intentionally omitted so the host uses the async
	// stream_id path. If the host receives an empty/absent chunks
	// slice, it reads from the bridge instead.
}

// chatShape is the intermediate representation extracted from an
// OpenAI or Claude payload before it becomes an executor.ChatRequest.
// Cursor's protocol handles system prompts, history, and tools
// independently of the source format.
type chatShape struct {
	Model        string
	SystemPrompt string
	History      []executor.HistoryTurn
	UserMessage  string
	Tools        []executor.ToolDefinition
	Stream       bool
	IncludeUsage bool // OpenAI stream_options.include_usage
	Effort       string
	Thinking     bool
	JSONSchema   json.RawMessage
	Attachments  []executor.Attachment
	WebSearch    bool
	WebFetch     bool
	// ForceTool is the Anthropic tool_choice name. "*" means any tool.
	// Cursor has no native tool_choice, so buildChatRequest turns this
	// into an explicit instruction on the user turn.
	ForceTool string

	// Cursor's protocol has no field for either control, so they are enforced
	// locally by translator.OutputLimiter on the way back out.
	MaxTokens     int
	StopSequences []string
}

// outputLimiter builds the generation-control enforcer for this request.
func (s chatShape) outputLimiter() *translator.OutputLimiter {
	return &translator.OutputLimiter{
		MaxTokens:     s.MaxTokens,
		StopSequences: s.StopSequences,
	}
}

// parseOpenAIPayload converts an OpenAI Chat Completion request body
// into a chatShape. The OpenAI schema handled here is the subset
// documented in cmd/cursor-proxy/main.go: system messages fold into a
// single systemPrompt, non-system messages become history, the last
// user message becomes UserMessage, and function-tools populate Tools.
func parseOpenAIPayload(body []byte) (chatShape, error) {
	var req struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Stream        bool `json:"stream"`
		StreamOptions *struct {
			IncludeUsage bool `json:"include_usage"`
		} `json:"stream_options"`
		ToolChoice json.RawMessage `json:"tool_choice"`
		Tools      []struct {
			Type     string `json:"type"`
			Function *struct {
				Name        string         `json:"name"`
				Description string         `json:"description"`
				Parameters  map[string]any `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return chatShape{}, fmt.Errorf("parse openai payload: %w", err)
	}
	var systemPrompt string
	turns := make([]struct {
		role string
		text string
	}, 0, len(req.Messages))
	for _, m := range req.Messages {
		text := flattenOpenAIContent(m.Content)
		if m.Role == "system" {
			if systemPrompt != "" {
				systemPrompt += "\n"
			}
			systemPrompt += text
			continue
		}
		turns = append(turns, struct {
			role string
			text string
		}{role: m.Role, text: text})
	}
	lastUserIdx := -1
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return chatShape{}, errors.New("openai payload has no user message")
	}
	shape := chatShape{
		Model:        req.Model,
		SystemPrompt: systemPrompt,
		UserMessage:  turns[lastUserIdx].text,
		Stream:       req.Stream,
		IncludeUsage: req.StreamOptions != nil && req.StreamOptions.IncludeUsage,
	}
	for _, t := range turns[:lastUserIdx] {
		if t.role != "user" && t.role != "assistant" {
			continue
		}
		shape.History = append(shape.History, executor.HistoryTurn{Role: t.role, Content: t.text})
	}
	for _, t := range req.Tools {
		if t.Type != "" && t.Type != "function" {
			continue
		}
		if t.Function == nil || strings.TrimSpace(t.Function.Name) == "" {
			continue
		}
		shape.Tools = append(shape.Tools, executor.ToolDefinition{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: t.Function.Parameters,
		})
	}
	shape.ForceTool = parseToolChoice(req.ToolChoice)
	return shape, nil
}

// flattenOpenAIContent accepts a plain string or an array of content
// blocks and returns a single flat string. Cursor's protocol is text-
// only so any non-text blocks (images, etc.) are dropped.
func flattenOpenAIContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out strings.Builder
	for _, block := range blocks {
		if block.Type != "" && block.Type != "text" {
			continue
		}
		if block.Text == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(block.Text)
	}
	return out.String()
}

// parseClaudePayload converts an Anthropic Messages request body into
// a chatShape. System prompt supports both string and array-of-block
// forms; content supports string and array-of-block forms.
func parseClaudePayload(body []byte) (chatShape, error) {
	var req struct {
		Model    string          `json:"model"`
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Stream        bool     `json:"stream"`
		MaxTokens     int      `json:"max_tokens"`
		StopSequences []string `json:"stop_sequences"`
		Thinking      *struct {
			Type string `json:"type"`
		} `json:"thinking"`
		OutputConfig *struct {
			Effort string          `json:"effort"`
			Format json.RawMessage `json:"format"`
		} `json:"output_config"`
		// ResponseFormat mirrors OpenAI / Anthropic beta
		// (structured-outputs-2025-11-13) placement of the JSON-schema
		// gate at the top level of the request. Some clients (including
		// cctest.ai's structured-output probe) put the schema here
		// instead of under output_config.format; both shapes must reach
		// buildChatRequest so the generated system-prompt splice
		// actually constrains the model.
		ResponseFormat json.RawMessage `json:"response_format"`
		ToolChoice     json.RawMessage `json:"tool_choice"`
		Tools          []struct {
			Type        string         `json:"type"`
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return chatShape{}, fmt.Errorf("parse claude payload: %w", err)
	}
	systemPrompt := flattenClaudeSystem(req.System)
	lastUserIdx := -1
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "user" {
			lastUserIdx = i
			break
		}
	}
	if lastUserIdx < 0 {
		return chatShape{}, errors.New("claude payload has no user message")
	}
	lastUserFlat := flattenClaudeContent(req.Messages[lastUserIdx].Content)
	shape := chatShape{
		Model:         req.Model,
		SystemPrompt:  systemPrompt,
		UserMessage:   continueFromToolResults(lastUserFlat),
		Stream:        req.Stream,
		Attachments:   extractClaudeAttachments(req.Messages[lastUserIdx].Content),
		MaxTokens:     req.MaxTokens,
		StopSequences: req.StopSequences,
	}
	if req.Thinking != nil {
		shape.Thinking = strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "adaptive") ||
			strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "enabled")
	}
	if req.OutputConfig != nil {
		shape.Effort = strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort))
		shape.JSONSchema = extractJSONSchema(req.OutputConfig.Format)
	}
	// Fall back to the top-level response_format when output_config.format
	// was absent — some clients only send the top-level shape. When both
	// are present the more specific output_config value wins because
	// that is Cursor / Anthropic's preferred location.
	if len(shape.JSONSchema) == 0 && len(req.ResponseFormat) > 0 {
		shape.JSONSchema = extractJSONSchema(req.ResponseFormat)
	}
	for _, m := range req.Messages[:lastUserIdx] {
		if m.Role != "user" && m.Role != "assistant" {
			continue
		}
		shape.History = append(shape.History, executor.HistoryTurn{
			Role:    m.Role,
			Content: flattenClaudeContent(m.Content),
		})
	}
	for _, t := range req.Tools {
		typ := strings.ToLower(strings.TrimSpace(t.Type))
		switch {
		case strings.HasPrefix(typ, "web_search_"):
			shape.WebSearch = true
			continue
		case strings.HasPrefix(typ, "web_fetch_"):
			shape.WebFetch = true
			continue
		}
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		shape.Tools = append(shape.Tools, executor.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	shape.ForceTool = parseToolChoice(req.ToolChoice)
	pairEmptyToolUseIDs(&shape, lastUserFlat)
	return shape, nil
}

// pairEmptyToolUseIDs copies tool_result.tool_use_id onto a preceding
// assistant tool_use that arrived without id. TokenSheep remarshals
// Anthropic history with `id,omitempty`, so the current user turn still
// has the id while ConversationHistory does not — Vertex then 400s.
func pairEmptyToolUseIDs(shape *chatShape, currentUserFlat string) {
	if shape == nil {
		return
	}
	var ids []string
	for _, fragment := range executor.ParseContentFragments(currentUserFlat) {
		if fragment.Kind == executor.ContentToolResult && strings.TrimSpace(fragment.ToolID) != "" {
			ids = append(ids, fragment.ToolID)
		}
	}
	if len(ids) == 0 {
		return
	}
	next := 0
	for i := range shape.History {
		if shape.History[i].Role != "assistant" {
			continue
		}
		shape.History[i].Content, next = rewriteEmptyToolUseIDs(shape.History[i].Content, ids, next)
		if next >= len(ids) {
			return
		}
	}
}

func rewriteEmptyToolUseIDs(content string, ids []string, next int) (string, int) {
	if next >= len(ids) || content == "" {
		return content, next
	}
	lines := strings.Split(content, "\n")
	for i, line := range lines {
		var block map[string]any
		if json.Unmarshal([]byte(line), &block) != nil {
			continue
		}
		if block["type"] != "tool_use" {
			continue
		}
		id, _ := block["id"].(string)
		if strings.TrimSpace(id) != "" || next >= len(ids) {
			continue
		}
		block["id"] = ids[next]
		next++
		encoded, err := json.Marshal(block)
		if err == nil {
			lines[i] = string(encoded)
		}
	}
	return strings.Join(lines, "\n"), next
}

// flattenClaudeSystem handles the string / array-of-block system field.
func flattenClaudeSystem(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out strings.Builder
	for _, block := range blocks {
		text, _ := block["text"].(string)
		if text == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(text)
	}
	return out.String()
}

// continueFromToolResults rewrites a user turn that is only tool_result
// blocks into an explicit continuation prompt. Anthropic's second tool
// turn has no user text; leaving the raw JSON as UserMessage makes some
// models re-call the same tool instead of answering.
func continueFromToolResults(content string) string {
	fragments := executor.ParseContentFragments(content)
	if len(fragments) == 0 {
		return content
	}
	for _, fragment := range fragments {
		if fragment.Kind != executor.ContentToolResult {
			return content
		}
	}
	var out strings.Builder
	out.WriteString("Continue from these tool results. Do not call the same tool again unless the result is insufficient.\n")
	for _, fragment := range fragments {
		out.WriteString("\n")
		out.WriteString(fragment.Transcript())
		out.WriteByte('\n')
	}
	return out.String()
}

// flattenClaudeContent handles the string / array-of-block content
// field for one Anthropic message. Tool round-trip blocks are retained
// as compact JSON so the in-band history carries tool calls and results.
func flattenClaudeContent(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return ""
	}
	var out strings.Builder
	for _, block := range blocks {
		bType, _ := block["type"].(string)
		var content string
		switch bType {
		case "", "text":
			content, _ = block["text"].(string)
		case "tool_use", "tool_result":
			encoded, err := json.Marshal(block)
			if err == nil {
				content = string(encoded)
			}
		default:
			continue
		}
		if content == "" {
			continue
		}
		if out.Len() > 0 {
			out.WriteByte('\n')
		}
		out.WriteString(content)
	}
	return out.String()
}

// buildChatRequest lifts a parsed chatShape into an executor.ChatRequest
// with the Cursor-specific knobs the plugin always sets: PureMode on
// (we present as an API caller, not an IDE) and AutoStopOnTurnEnd on
// (close the SSE stream as soon as a turn ends so we do not park).
//
// Model variant selection (thinking / effort tier) is expressed as
// ModelParameters keyed by "thinking" and "effort" — the executor then
// looks the live catalog up per-account and picks the matching variant.
// This keeps two families of legacy slug shapes working transparently:
// modern models (opus-5, sonnet-5, fable-5, opus-4-8, opus-4-7) use
// "<base>-thinking-<tier>" slugs, older models (sonnet-4-5, sonnet-4-6,
// opus-4-5, opus-4-6, haiku-4-5, sonnet-4) use "<version>-<family>-
// [<tier>-]thinking" slugs and often ship only 1-2 variants at all.
// Synthesizing the slug on the client side was hard-coded to the newer
// shape and rejected ~half the Claude line with ERROR_BAD_MODEL_NAME.
func buildChatRequest(shape chatShape, headers map[string][]string) *executor.ChatRequest {
	systemPrompt := shape.SystemPrompt
	if len(shape.JSONSchema) > 0 {
		if systemPrompt != "" {
			systemPrompt += "\n\n"
		}
		// The empty-object schema (`{}`) is the sentinel value returned
		// by extractJSONSchema for bare json_object mode (OpenAI's
		// response_format:{type:"json_object"}). Emit the JSON-only guard
		// without a schema literal — anything more specific would be
		// invented by us and could mislead the model.
		if string(bytes.TrimSpace(shape.JSONSchema)) == "{}" {
			systemPrompt += "Return only valid JSON. Do not use Markdown fences or add explanatory text."
		} else {
			systemPrompt += "Return only valid JSON matching this JSON Schema. Do not use Markdown fences or add explanatory text:\n" + string(shape.JSONSchema)
		}
	}
	effort := shape.Effort
	if shape.Thinking && normalizeCursorEffort(effort) == "" {
		// API thinking probes expect extended reasoning; medium/base often
		// answers without a thinking block on Cursor's Vertex/Bedrock path.
		effort = "high"
	}
	parameters := chatModelParameters(shape.Thinking, effort)
	model := resolveCursorServerToolVariant(shape.Model, shape.WebSearch || shape.WebFetch)
	userMessage := shape.UserMessage
	if instr := forcedToolInstruction(shape.ForceTool); instr != "" {
		if userMessage != "" {
			userMessage += "\n\n"
		}
		userMessage += instr
	}
	req := &executor.ChatRequest{
		Model:              model,
		ModelParameters:    parameters,
		UserMessage:        userMessage,
		SystemPrompt:       systemPrompt,
		History:            shape.History,
		Mode:               executor.APIConversationMode(len(shape.Tools) > 0 || shape.WebSearch || shape.WebFetch),
		PureMode:           !(shape.WebSearch || shape.WebFetch),
		AutoStopOnTurnEnd:  true,
		AutoStopOnToolCall: true,
		Tools:              shape.Tools,
		Attachments:        shape.Attachments,
		WebSearch:          shape.WebSearch,
		WebFetch:           shape.WebFetch,
	}
	if headers != nil {
		if convID := firstHeader(headers, "X-Conversation-Id"); convID != "" {
			req.ConversationID = convID
		}
	}
	return req
}

// chatModelParameters converts the shape's thinking/effort knobs into the
// parameter map the live catalog's variants are keyed by. Callers that
// omit both leave the map nil so the executor picks the model's default
// variant. When only thinking is set we still emit "thinking":"true"
// (with no effort) — that lets older families like sonnet-4-5 that
// ship exactly {thinking, non-thinking} pairs resolve cleanly.
func chatModelParameters(thinking bool, effort string) map[string]string {
	parameters := map[string]string{}
	if thinking {
		parameters["thinking"] = "true"
	}
	if tier := normalizeCursorEffort(effort); tier != "" {
		parameters["effort"] = tier
	}
	if len(parameters) == 0 {
		return nil
	}
	return parameters
}

type cursorServerToolProfile struct {
	Tier string
}

var cursorServerToolProfiles = map[string]cursorServerToolProfile{
	"claude-opus-4-8": {Tier: "low"},
}

func resolveCursorServerToolVariant(model string, serverTools bool) string {
	if !serverTools {
		return model
	}
	fast := strings.HasSuffix(model, "-fast")
	core := strings.TrimSuffix(model, "-fast")
	tiers := []string{"low", "medium", "high", "xhigh", "max"}
	base := core
	thinking := false
	for _, tier := range tiers {
		if strings.HasSuffix(core, "-thinking-"+tier) {
			base = strings.TrimSuffix(core, "-thinking-"+tier)
			thinking = true
			break
		}
		if strings.HasSuffix(core, "-"+tier) {
			base = strings.TrimSuffix(core, "-"+tier)
			break
		}
	}
	profile, ok := cursorServerToolProfiles[base]
	if !ok || profile.Tier == "" {
		return model
	}
	resolved := base + "-" + profile.Tier
	if thinking {
		resolved = base + "-thinking-" + profile.Tier
	}
	if fast {
		resolved += "-fast"
	}
	return resolved
}

func extractClaudeAttachments(raw json.RawMessage) []executor.Attachment {
	if len(raw) == 0 {
		return nil
	}
	var blocks []struct {
		Type   string `json:"type"`
		Source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
		} `json:"source"`
		Title string `json:"title"`
	}
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	attachments := make([]executor.Attachment, 0, len(blocks))
	for index, block := range blocks {
		if block.Type != "image" && block.Type != "document" {
			continue
		}
		if block.Source.Type != "base64" || block.Source.Data == "" {
			continue
		}
		data, err := base64.StdEncoding.DecodeString(block.Source.Data)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(block.Source.Data)
		}
		if err != nil || len(data) == 0 {
			continue
		}
		filename := strings.TrimSpace(block.Title)
		if filename == "" {
			filename = fmt.Sprintf("%s-%d%s", block.Type, index+1, attachmentExtension(block.Source.MediaType, block.Type))
		}
		attachments = append(attachments, executor.Attachment{
			Kind:     block.Type,
			Filename: filename,
			MimeType: block.Source.MediaType,
			Data:     data,
		})
	}
	return attachments
}

func attachmentExtension(mimeType, kind string) string {
	switch strings.ToLower(strings.TrimSpace(mimeType)) {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	}
	if kind == "image" {
		return ".img"
	}
	return ".bin"
}

func parseToolChoice(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		switch strings.ToLower(strings.TrimSpace(asString)) {
		case "required", "any":
			return "*"
		default:
			return ""
		}
	}
	var obj struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		Function *struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	switch strings.ToLower(strings.TrimSpace(obj.Type)) {
	case "tool":
		return strings.TrimSpace(obj.Name)
	case "function":
		if obj.Function != nil {
			return strings.TrimSpace(obj.Function.Name)
		}
		return ""
	case "any", "required":
		return "*"
	default:
		return ""
	}
}

// forcedToolInstruction generates the prompt fragment that emulates
// Anthropic's native tool_choice enforcement. Cursor's upstream does not
// forward tool_choice, so we splice the constraint into the user message
// and rely on the model to comply. The wording is deliberately absolute —
// polite variants ("please call...") get ignored on short/ambiguous
// prompts. Even when arguments are missing the model must call the tool
// with its best guess; asking for clarification instead is what cctest.ai's
// tool_use probe fails us on.
func forcedToolInstruction(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if name == "*" {
		return "You MUST call one of the provided tools on this turn. Never reply with only text; if arguments are ambiguous, pick a reasonable default and call the tool anyway."
	}
	return "You MUST call the `" + name + "` tool on this turn. Do not reply with only text and do not ask for clarification; if any argument is missing, pick a reasonable default value and call the tool anyway."
}

// extractJSONSchema normalises the three JSON-schema request shapes we see
// in the wild into a single compact schema blob:
//
//  1. Anthropic output_config.format:
//     `{"type":"json_schema","schema":{...}}`
//  2. OpenAI response_format / Anthropic structured-outputs beta:
//     `{"type":"json_schema","json_schema":{"name":"...","strict":true,
//     "schema":{...}}}`
//  3. Bare JSON-object gating (OpenAI json_object mode) — no schema, we
//     just note that a JSON constraint was requested by returning an
//     empty non-nil `{}` blob so the caller can still inject the guard
//     system prompt.
//
// Returning nil means "no structured-output constraint at all".
func extractJSONSchema(format json.RawMessage) json.RawMessage {
	if len(format) == 0 {
		return nil
	}
	var parsed struct {
		Type       string          `json:"type"`
		Schema     json.RawMessage `json:"schema"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(format, &parsed); err != nil {
		return nil
	}
	kind := strings.ToLower(strings.TrimSpace(parsed.Type))
	// Direct schema at the top of the format object (Anthropic
	// output_config.format shape).
	if len(parsed.Schema) > 0 && (kind == "json_schema" || kind == "") {
		return compactSchema(parsed.Schema)
	}
	// Nested schema — OpenAI response_format style and Anthropic's
	// structured-outputs-2025-11-13 beta both put the actual schema one
	// level deeper under a `json_schema` object with `name/strict/schema`.
	if len(parsed.JSONSchema) > 0 {
		var nested struct {
			Schema json.RawMessage `json:"schema"`
		}
		if err := json.Unmarshal(parsed.JSONSchema, &nested); err == nil && len(nested.Schema) > 0 {
			return compactSchema(nested.Schema)
		}
		// Some callers put the schema directly under json_schema without
		// a wrapper — accept that too.
		return compactSchema(parsed.JSONSchema)
	}
	// Bare json_object mode — no schema shipped, but callers still expect
	// a JSON-only reply. Return an empty schema so buildChatRequest emits
	// the guarding system prompt but does not embed a schema string.
	if kind == "json_object" {
		return json.RawMessage(`{}`)
	}
	return nil
}

func compactSchema(schema json.RawMessage) json.RawMessage {
	buf := bytes.Buffer{}
	if err := json.Compact(&buf, schema); err == nil {
		return json.RawMessage(append([]byte(nil), buf.Bytes()...))
	}
	return append(json.RawMessage(nil), schema...)
}

func resolveCursorModelVariant(model, effort string, thinking bool) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return model
	}
	fast := strings.HasSuffix(model, "-fast")
	core := strings.TrimSuffix(model, "-fast")
	tiers := []string{"low", "medium", "high", "xhigh", "max"}
	base, currentTier := core, ""
	for _, tier := range tiers {
		if strings.HasSuffix(core, "-thinking-"+tier) {
			base = strings.TrimSuffix(core, "-thinking-"+tier)
			currentTier = tier
			break
		}
		if strings.HasSuffix(core, "-"+tier) {
			base = strings.TrimSuffix(core, "-"+tier)
			currentTier = tier
			break
		}
	}
	requestedTier := normalizeCursorEffort(effort)
	if requestedTier == "" {
		requestedTier = currentTier
	}
	if requestedTier == "" {
		requestedTier = "medium"
	}
	if !thinking && normalizeCursorEffort(effort) == "" {
		return model
	}
	resolved := base + "-" + requestedTier
	if thinking {
		resolved = base + "-thinking-" + requestedTier
	}
	if fast {
		resolved += "-fast"
	}
	return resolved
}

func normalizeCursorEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return ""
	}
}

// firstHeader mirrors http.Header.Get without depending on net/http
// so the plugin stays light on imports.
func firstHeader(h map[string][]string, name string) string {
	if len(h) == 0 {
		return ""
	}
	lowered := strings.ToLower(name)
	for k, v := range h {
		if strings.ToLower(k) == lowered && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

func translatePluginEvent(server *cursorpb.AgentV1_AgentServerMessage, tools []executor.ToolDefinition) *translator.Event {
	event := translator.FromServerMessage(server)
	if event == nil {
		return nil
	}
	names := declaredToolNames(tools)
	event = rewriteNativeWebSearchForClient(event, names, false, false)
	if event == nil {
		return nil
	}
	if len(names) == 0 {
		// See translatePluginEventForDialect: a tool call the caller never
		// declared cannot be executed or answered, so it must not be surfaced.
		if isClientToolEvent(event.Kind) {
			return nil
		}
		return event
	}
	if isClientToolEvent(event.Kind) {
		translator.ApplyClientToolAlias(event, names)
	}
	return event
}

// translateClaudePluginEvent preserves the exact tool spelling declared by
// the Anthropic client. When a request omits a native tool from tools[] (for
// example Claude Code's deferred/lazy tool loading), fall back to Claude
// Code's canonical case-sensitive names instead of Cursor's lowercase names.
// Without this fallback Claude Code receives `glob` and rejects it because its
// dispatch table contains `Glob`.
func translatePluginEventForDialect(server *cursorpb.AgentV1_AgentServerMessage, tools []executor.ToolDefinition, dialect translator.ToolNameDialect) *translator.Event {
	return translateClaudeEvent(server, tools, dialect, false, false)
}

func translateClaudeEvent(server *cursorpb.AgentV1_AgentServerMessage, tools []executor.ToolDefinition, dialect translator.ToolNameDialect, serverSearch, serverFetch bool) *translator.Event {
	event := translator.FromServerMessage(server)
	if event == nil {
		return nil
	}
	names := declaredToolNames(tools)
	event = rewriteNativeWebSearchForClient(event, names, serverSearch, serverFetch)
	if event == nil {
		return nil
	}
	canonicalizeAnthropicToolEvent(event)
	if len(names) == 0 && isClientToolEvent(event.Kind) {
		// Cursor runs its own native tools while working a turn. Surfacing one
		// as a client `tool_use` when the caller declared no tools hands back a
		// call it cannot execute and has no id to answer, which a strict client
		// rejects. Server tools (web search) are a different contract and pass
		// through untouched.
		return nil
	}
	if isClientToolEvent(event.Kind) {
		translator.ApplyClientToolContract(event, names, dialect)
	}
	return event
}

func declaredToolNames(tools []executor.ToolDefinition) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := strings.TrimSpace(tool.Name); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// rewriteNativeWebSearchForClient converts Cursor's native search into a
// client tool_use when the caller declared WebSearch. Claude Code executes
// that tool itself; leaving it as server_tool_use keeps the HTTP stream open
// while Cursor searches (or waits for an approval we already sent).
func rewriteNativeWebSearchForClient(event *translator.Event, names []string, serverSearch, serverFetch bool) *translator.Event {
	if event == nil {
		return nil
	}
	searchName := declaredClientToolName(names, "websearch", "web_search")
	fetchName := declaredClientToolName(names, "webfetch", "web_fetch")
	switch event.Kind {
	case translator.EventServerToolPermission:
		switch strings.ToLower(event.ToolName) {
		case "web_search":
			if searchName == "" {
				// Anthropic server-tool request: announce server_tool_use as
				// soon as Cursor asks permission so the stream is not silent
				// for the whole search. The later ToolCallStarted with the
				// same id is dropped by the Anthropic writer.
				event.Kind = translator.EventServerToolStarted
				event.ToolName = "web_search"
				return event
			}
			event.Kind = translator.EventToolCallStarted
			event.ToolName = searchName
			return event
		case "web_fetch":
			if fetchName != "" {
				event.Kind = translator.EventToolCallStarted
				event.ToolName = fetchName
				return event
			}
			if serverFetch {
				event.Kind = translator.EventServerToolStarted
				event.ToolName = "web_fetch"
				return event
			}
			// Search-only Anthropic requests must not grow a matching
			// web_fetch server_tool_use. Cursor often asks to fetch the
			// first hit; announcing that without a result fails strict
			// stream checkers.
			return nil
		default:
			return nil
		}
	case translator.EventServerToolStarted:
		if isFetchToolName(event.ToolName) {
			return rewriteFetchEvent(event, fetchName, serverFetch)
		}
		if searchName != "" && strings.EqualFold(event.ToolName, "web_search") {
			event.Kind = translator.EventToolCallStarted
			event.ToolName = searchName
		}
		return event
	case translator.EventToolCallStarted, translator.EventToolCallDelta, translator.EventToolCallCompleted:
		if isFetchToolName(event.ToolName) {
			return rewriteFetchEvent(event, fetchName, serverFetch)
		}
		return event
	case translator.EventWebSearchResult:
		if isFetchToolName(event.ToolName) {
			if fetchName != "" || !serverFetch {
				return nil
			}
			return event
		}
		if searchName != "" {
			return nil
		}
		return event
	default:
		if isUndeclaredFetchEvent(event, fetchName) && !serverFetch {
			return nil
		}
		return event
	}
}

func rewriteFetchEvent(event *translator.Event, fetchName string, serverFetch bool) *translator.Event {
	if fetchName != "" {
		if event.Kind == translator.EventServerToolStarted || event.Kind == translator.EventServerToolPermission {
			event.Kind = translator.EventToolCallStarted
		}
		event.ToolName = fetchName
		return event
	}
	if !serverFetch {
		return nil
	}
	switch event.Kind {
	case translator.EventToolCallStarted, translator.EventServerToolStarted, translator.EventServerToolPermission:
		event.Kind = translator.EventServerToolStarted
		event.ToolName = "web_fetch"
		return event
	case translator.EventToolCallCompleted:
		event.Kind = translator.EventWebSearchResult
		event.ToolName = "web_fetch"
		return event
	case translator.EventToolCallDelta:
		return nil
	default:
		event.ToolName = "web_fetch"
		return event
	}
}

func isFetchToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "web_fetch", "webfetch", "fetch", "web_fetch_tool_call", "webfetchtoolcall", "fetchtoolcall":
		return true
	default:
		return false
	}
}

func isUndeclaredFetchEvent(event *translator.Event, fetchName string) bool {
	return event != nil && fetchName == "" && isFetchToolName(event.ToolName)
}

func canonicalizeAnthropicToolEvent(event *translator.Event) {
	if event == nil || event.ToolCallID == "" {
		return
	}
	server := event.Kind == translator.EventServerToolStarted ||
		event.Kind == translator.EventWebSearchResult ||
		event.Kind == translator.EventServerToolPermission
	event.ToolCallID = translator.CanonicalAnthropicToolID(event.ToolCallID, server)
}

func declaredClientToolName(names []string, aliases ...string) string {
	want := make(map[string]bool, len(aliases))
	for _, alias := range aliases {
		want[strings.ToLower(alias)] = true
	}
	for _, name := range names {
		trimmed := strings.TrimSpace(name)
		if want[strings.ToLower(trimmed)] {
			return trimmed
		}
	}
	return ""
}

func isClientToolEvent(kind translator.EventKind) bool {
	switch kind {
	case translator.EventToolCallStarted, translator.EventToolCallDelta, translator.EventToolCallCompleted:
		return true
	default:
		return false
	}
}

func translateAnthropicPluginEvent(server *cursorpb.AgentV1_AgentServerMessage, tools []executor.ToolDefinition) *translator.Event {
	return translateClaudeEvent(server, tools, translator.ToolNameDialectClaudeCode, false, false)
}

func translateClaudeExecuteEvent(server *cursorpb.AgentV1_AgentServerMessage, shape chatShape) *translator.Event {
	return translateClaudeEvent(server, shape.Tools, translator.ToolNameDialectClaudeCode, shape.WebSearch, shape.WebFetch)
}

// normaliseFormat maps a wire format string to one we handle. Empty
// and unknown values default to the SourceFormat when possible,
// otherwise "openai".
func normaliseFormat(format, sourceFormat string) string {
	f := strings.ToLower(strings.TrimSpace(format))
	switch f {
	case "openai", "openai-response":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	}
	sf := strings.ToLower(strings.TrimSpace(sourceFormat))
	switch sf {
	case "openai", "openai-response":
		return "openai"
	case "claude", "anthropic":
		return "claude"
	}
	return "openai"
}

// parseByFormat picks the right parser and returns the chatShape.
func parseByFormat(payload []byte, format string) (chatShape, error) {
	switch normaliseFormat(format, "") {
	case "claude":
		return parseClaudePayload(payload)
	default:
		return parseOpenAIPayload(payload)
	}
}

// gatherEvents pumps executor.ChatEvent frames into a
// translator-friendly stream. The Cursor executor emits KV blob
// events carrying the fully-assembled assistant text and interaction
// updates for structured events; this helper mirrors the diff-suffix
// logic in cmd/cursor-proxy so callers see one clean text delta per
// new chunk.
type collectedTurn struct {
	AssistantText string
	Usage         *translator.Usage
	ToolCalls     []map[string]any
	SawTurnEnd    bool
	SawToolCall   bool
}

// diffSuffix returns the trailing portion of full that comes after
// sent. When full does not start with sent, the entire text is
// returned (defensive against the server replaying tokens).
func diffSuffix(sent, full string) string {
	if sent == "" {
		return full
	}
	if full == sent || strings.HasPrefix(sent, full) {
		return ""
	}
	if strings.HasPrefix(full, sent) {
		return full[len(sent):]
	}
	maxOverlap := len(sent)
	if len(full) < maxOverlap {
		maxOverlap = len(full)
	}
	for overlap := maxOverlap; overlap > 0; overlap-- {
		if strings.HasSuffix(sent, full[:overlap]) {
			return full[overlap:]
		}
	}
	return full
}

type assistantStreamState struct {
	emitted     string
	sawSnapshot bool
}

func (s *assistantStreamState) consumeDelta(delta string) string {
	if delta == "" || s.sawSnapshot {
		return ""
	}
	s.emitted += delta
	return delta
}

func (s *assistantStreamState) consumeSnapshot(full string) string {
	if full == "" {
		return ""
	}
	s.sawSnapshot = true
	delta := diffSuffix(s.emitted, full)
	if delta != "" {
		s.emitted += delta
	}
	return delta
}

// countTokens uses the char heuristic (ASCII bytes / 4 + CJK bytes /
// 1.5) — the same one cpa-context-guard uses. Non-ASCII, non-CJK
// characters are treated as ASCII (they compress worse but we do
// not want to over-count Latin-1 accented text).
func countTokens(text string) int64 {
	var ascii, cjk float64
	for _, r := range text {
		if isCJK(r) {
			cjk++
		} else {
			ascii++
		}
	}
	return int64(ascii/4 + cjk/1.5)
}

// isCJK reports whether a rune sits in the common CJK Unified
// Ideograph ranges. Kept intentionally narrow — full-width kana and
// hangul are billed the same as CJK by most tokenizers.
func isCJK(r rune) bool {
	switch {
	case r >= 0x3400 && r <= 0x4DBF: // CJK Extension A
		return true
	case r >= 0x4E00 && r <= 0x9FFF: // CJK Unified
		return true
	case r >= 0xF900 && r <= 0xFAFF: // Compatibility
		return true
	case r >= 0x20000 && r <= 0x2A6DF: // Extension B
		return true
	case r >= 0x3040 && r <= 0x30FF: // Hiragana + Katakana
		return true
	case r >= 0xAC00 && r <= 0xD7AF: // Hangul syllables
		return true
	}
	return false
}
