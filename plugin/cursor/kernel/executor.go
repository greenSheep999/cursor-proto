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
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
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
	c2 := executor.NewClient(acc)
	c2.API3 = c2.API2
	return c2, nil
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
		Tools []struct {
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
		Stream   bool `json:"stream"`
		Thinking *struct {
			Type string `json:"type"`
		} `json:"thinking"`
		OutputConfig *struct {
			Effort string          `json:"effort"`
			Format json.RawMessage `json:"format"`
		} `json:"output_config"`
		Tools []struct {
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
	shape := chatShape{
		Model:        req.Model,
		SystemPrompt: systemPrompt,
		UserMessage:  flattenClaudeContent(req.Messages[lastUserIdx].Content),
		Stream:       req.Stream,
	}
	if req.Thinking != nil {
		shape.Thinking = strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "adaptive") ||
			strings.EqualFold(strings.TrimSpace(req.Thinking.Type), "enabled")
	}
	if req.OutputConfig != nil {
		shape.Effort = strings.ToLower(strings.TrimSpace(req.OutputConfig.Effort))
		shape.JSONSchema = extractJSONSchema(req.OutputConfig.Format)
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
		if strings.TrimSpace(t.Name) == "" {
			continue
		}
		shape.Tools = append(shape.Tools, executor.ToolDefinition{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.InputSchema,
		})
	}
	return shape, nil
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
func buildChatRequest(shape chatShape, headers map[string][]string) *executor.ChatRequest {
	systemPrompt := shape.SystemPrompt
	if len(shape.JSONSchema) > 0 {
		if systemPrompt != "" {
			systemPrompt += "\n\n"
		}
		systemPrompt += "Return only valid JSON matching this JSON Schema. Do not use Markdown fences or add explanatory text:\n" + string(shape.JSONSchema)
	}
	req := &executor.ChatRequest{
		Model:              resolveCursorModelVariant(shape.Model, shape.Effort, shape.Thinking),
		UserMessage:        shape.UserMessage,
		SystemPrompt:       systemPrompt,
		History:            shape.History,
		Mode:               executor.APIConversationMode(len(shape.Tools) > 0),
		PureMode:           true,
		AutoStopOnTurnEnd:  true,
		AutoStopOnToolCall: true,
		Tools:              shape.Tools,
	}
	if headers != nil {
		if convID := firstHeader(headers, "X-Conversation-Id"); convID != "" {
			req.ConversationID = convID
		}
	}
	return req
}

func extractJSONSchema(format json.RawMessage) json.RawMessage {
	if len(format) == 0 {
		return nil
	}
	var parsed struct {
		Type   string          `json:"type"`
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(format, &parsed); err != nil || !strings.EqualFold(parsed.Type, "json_schema") || len(parsed.Schema) == 0 {
		return nil
	}
	buf := bytes.Buffer{}
	if err := json.Compact(&buf, parsed.Schema); err == nil {
		return json.RawMessage(append([]byte(nil), buf.Bytes()...))
	}
	return append(json.RawMessage(nil), parsed.Schema...)
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
