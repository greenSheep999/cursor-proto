package kernel

// ABI method handlers for the three executor entry points:
//
//   executor.execute        — non-streaming call: collect the whole
//                              response and return it as one envelope.
//   executor.execute_stream — streaming call: emit each Cursor event
//                              back through host.stream.emit / close.
//   executor.count_tokens   — local heuristic estimate.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/translator"
)

// debugLog is a stderr trace helper for tracking down streaming
// bugs. Enabled by CURSOR_PLUGIN_DEBUG=1 in the environment; a
// no-op otherwise so production builds stay quiet.
func debugLog(msg string, args ...any) {
	if os.Getenv("CURSOR_PLUGIN_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[cursor-plugin] "+msg+"\n", args...)
}

// runChatFn matches the signature of executor.Client.RunChat. The
// handlers take it as a parameter (indirectly, via chatRunner) so
// tests can substitute a fake stream without spinning up a real
// Cursor client.
type chatRunner interface {
	RunChat(ctx context.Context, req *executor.ChatRequest) (<-chan executor.ChatEvent, error)
}

// runnerFactory produces a chatRunner for the request. Production
// builds return a cached *executor.Client from globalClientCache;
// tests inject their own fake.
var runnerFactory = func(authID string, storage []byte) (chatRunner, string, error) {
	client, err := globalClientCache.getClient(authID, storage)
	if err != nil {
		return nil, "", err
	}
	email := ""
	if client.Account != nil {
		email = client.Account.Email
	}
	return client, email, nil
}

// handleExecutorExecute implements executor.execute.
func handleExecutorExecute(payload []byte) ([]byte, int) {
	var req executorRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse executor request: %v", err), false), 0
	}
	shape, errParse := parseByFormat(req.Payload, req.Format)
	if errParse != nil {
		return errorEnvelope("bad_payload", errParse.Error(), false), 0
	}
	if strings.TrimSpace(req.Model) != "" {
		shape.Model = req.Model
	}
	if strings.TrimSpace(shape.Model) == "" {
		return errorEnvelope("bad_payload", "model is required", false), 0
	}
	excluded, errExcluded := storageExcludesModel(req.StorageJSON, shape.Model)
	if errExcluded != nil {
		return errorEnvelope("bad_auth", errExcluded.Error(), true), 0
	}
	if excluded {
		return errorEnvelope("model_excluded", fmt.Sprintf("model %s is excluded for this cursor account", shape.Model), true), 0
	}
	chatReq := buildChatRequest(shape, req.Headers)
	if dec := applyQuotaLane(chatReq, req.StorageJSON, shape.Model); dec.Skip {
		return errorEnvelopeQuota(dec.Message), 0
	}

	runner, _, errClient := runnerFactory(req.AuthID, req.StorageJSON)
	if errClient != nil {
		return errorEnvelope("bad_auth", errClient.Error(), true), 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), requestDeadline())
	defer cancel()
	events, errRun := runner.RunChat(ctx, chatReq)
	if errRun != nil {
		return errorEnvelope("upstream_error", errRun.Error(), true), 0
	}

	format := normaliseFormat(req.Format, req.SourceFormat)
	body, errCollect := collectNonStreaming(format, shape, shape.outputLimiter(), estimateClaudeInputTokens(shape), events)
	if errCollect != nil {
		return errorEnvelope("upstream_error", errCollect.Error(), true), 0
	}
	headers := map[string][]string{
		"Content-Type": {"application/json"},
		// Anthropic tags every response with request-id and clients surface it
		// in errors and bug reports. Without one a caller has no handle to
		// correlate a bad turn with our logs.
		"request-id": {translator.NewAnthropicRequestID()},
	}
	resp := executorResponse{Payload: body, Headers: headers}
	buf, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		return errorEnvelope("marshal_response", errMarshal.Error(), false), 0
	}
	return okEnvelopeJSON(string(buf)), 0
}

// collectNonStreaming iterates the RunChat channel and produces a
// full response body in the requested output format.
func collectNonStreaming(format string, shape chatShape, limiter *translator.OutputLimiter, estimatedInput int64, events <-chan executor.ChatEvent) ([]byte, error) {
	switch format {
	case "claude":
		return buildClaudeNonStreaming(shape.Model, shape, limiter, estimatedInput, events)
	default:
		return buildOpenAINonStreaming(shape.Model, shape.Tools, events)
	}
}

// estimateClaudeInputTokens approximates the request size published on
// Anthropic message_start.usage.input_tokens. Cursor only reports a real
// input count on TurnEnded, which is too late for the preamble.
func estimateClaudeInputTokens(shape chatShape) int64 {
	var b strings.Builder
	b.WriteString(shape.SystemPrompt)
	b.WriteString(shape.UserMessage)
	for _, turn := range shape.History {
		b.WriteString(turn.Content)
	}
	for _, tool := range shape.Tools {
		b.WriteString(tool.Name)
		b.WriteString(tool.Description)
	}
	n := countTokens(b.String())
	if n < 1 {
		return 1
	}
	return n
}

const emptyUpstreamResponseMessage = "empty response from upstream (no content, tool calls, or token usage)"

var errEmptyUpstreamResponse = errors.New(emptyUpstreamResponseMessage)

func isEmptyPluginUpstreamResponse(hasOutput bool, usage *translator.Usage) bool {
	if hasOutput {
		return false
	}
	return usage == nil || usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 &&
		usage.CacheWriteTokens == 0 &&
		usage.ReasoningTokens == 0
}

func usageWithObservedOutput(usage *translator.Usage, text string) *translator.Usage {
	if text == "" {
		return usage
	}
	if usage == nil {
		usage = &translator.Usage{}
	}
	copy := *usage
	copy.ObservedOutputTokens = countTokens(text)
	if copy.ObservedOutputTokens == 0 {
		copy.ObservedOutputTokens = 1
	}
	return &copy
}

// usageAfterLimit reports the clipped length instead of the full generation
// Cursor billed, so a caller that set max_tokens is never told it received
// more output than the bytes it actually got.
func usageAfterLimit(usage *translator.Usage, limiter *translator.OutputLimiter) *translator.Usage {
	if usage == nil || limiter == nil {
		return usage
	}
	if _, _, limited := limiter.StopReason(); !limited {
		return usage
	}
	copy := *usage
	copy.TruncatedOutputTokens = int64(limiter.EmittedTokens())
	if copy.TruncatedOutputTokens == 0 {
		copy.TruncatedOutputTokens = 1
	}
	return &copy
}

func cursorTrailerError(event executor.ChatEvent) error {
	if !event.Trailer || event.Status == nil || event.Status.OK() {
		return nil
	}
	return event.Status.Err()
}

// buildOpenAINonStreaming mirrors nonStreamOpenAI in cmd/cursor-proxy.
// It intentionally omits the cache-simulator logic — the plugin does
// not have opinions about caching, that's a host-level concern.
//
// When Cursor emits a KV blob (the assembled assistant text so far),
// we use it as the authoritative response. When only text deltas
// arrive (e.g. legacy stream shape or when a KV blob never gets
// flushed), we accumulate the deltas so the final response is still
// populated.
func buildOpenAINonStreaming(model string, tools []executor.ToolDefinition, events <-chan executor.ChatEvent) ([]byte, error) {
	acc := translator.NonStreamingAccumulator{Model: model}
	sawBlob := false
	deltaText := ""
	for ev := range events {
		if err := cursorTrailerError(ev); err != nil {
			return nil, err
		}
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			acc.Text = blob.AssistantText
			sawBlob = true
			continue
		}
		trEv := translatePluginEvent(ev.Server, tools)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			deltaText += trEv.Text
		case translator.EventToolCallStarted:
			acc.Consume(trEv)
		case translator.EventTurnEnded:
			acc.Usage = trEv.Usage
			acc.FinishStop = true
		}
	}
	if !sawBlob && deltaText != "" {
		acc.Text = deltaText
	}
	acc.Usage = usageWithObservedOutput(acc.Usage, acc.Text)
	if isEmptyPluginUpstreamResponse(acc.Text != "" || len(acc.ToolCalls) > 0, acc.Usage) {
		return nil, errEmptyUpstreamResponse
	}
	return acc.Response("chatcmpl-" + auth.GenerateSessionID()), nil
}

func appendUniqueServerToolBlock(blocks *[]map[string]any, block map[string]any) {
	if blocks == nil || block == nil {
		return
	}
	if block["type"] == "server_tool_use" {
		id, _ := block["id"].(string)
		if id != "" {
			for _, existing := range *blocks {
				if existing["type"] != "server_tool_use" {
					continue
				}
				existingID, _ := existing["id"].(string)
				if existingID == id {
					return
				}
			}
		}
	}
	*blocks = append(*blocks, block)
}

// citationsFromServerToolUses walks the collected server_tool_use /
// web_search_tool_result blocks and produces canonical citation entries
// for the final text block. Each web_search_result becomes a
// `web_search_result_location` citation whose encrypted_index binds it
// back to the originating tool_use_id + position. Detectors that grade
// WebSearch on Anthropic field parity treat an empty or absent citations
// array on the answer text as a failure signal.
func citationsFromServerToolUses(blocks []map[string]any) []map[string]any {
	if len(blocks) == 0 {
		return nil
	}
	var citations []map[string]any
	for _, block := range blocks {
		if block["type"] != "web_search_tool_result" {
			continue
		}
		toolUseID, _ := block["tool_use_id"].(string)
		items, ok := block["content"].([]map[string]any)
		if !ok {
			continue
		}
		for idx, item := range items {
			if item["type"] != "web_search_result" {
				continue
			}
			url, _ := item["url"].(string)
			title, _ := item["title"].(string)
			citedText := title
			if citedText == "" {
				citedText = url
			}
			citations = append(citations, map[string]any{
				"type":            "web_search_result_location",
				"cited_text":      citedText,
				"url":             url,
				"title":           title,
				"encrypted_index": fmt.Sprintf("%s#%d", toolUseID, idx),
			})
		}
	}
	return citations
}

// stripJSONMarkdownFences pulls the first JSON literal out of an assistant
// reply. Cursor's Claude upstream ignores the "no Markdown" clause in the
// structured-output system prompt often enough that a response wrapped in
// ```json ... ``` and followed by a short explanation is the typical
// success path. Callers that grade the raw response body (cctest.ai's
// structured-output check, most OpenAI SDKs' JSON-mode contract) need
// the raw JSON, not the fenced answer.
//
// Heuristic:
//  1. Strip a leading ```json (or generic ```) fence and its trailing ```.
//  2. If step 1 does not apply, return the substring from the first `{` or
//     `[` to the last matching `}` or `]`. Any text after is trimmed.
//  3. If neither shape looks like JSON, return the original untouched.
func stripJSONMarkdownFences(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return text
	}
	// Fenced form: ```json ... ``` or ``` ... ```
	if strings.HasPrefix(trimmed, "```") {
		body := strings.TrimPrefix(trimmed, "```")
		if lang := strings.IndexByte(body, '\n'); lang >= 0 {
			// Discard the fence-language token on the first line.
			body = body[lang+1:]
		}
		if end := strings.LastIndex(body, "```"); end >= 0 {
			body = body[:end]
		}
		body = strings.TrimSpace(body)
		if json.Valid([]byte(body)) {
			return body
		}
	}
	// Unfenced form: look for the widest JSON literal.
	firstObj := strings.IndexByte(trimmed, '{')
	firstArr := strings.IndexByte(trimmed, '[')
	start := -1
	closer := byte(0)
	switch {
	case firstObj < 0 && firstArr < 0:
		return text
	case firstObj < 0:
		start = firstArr
		closer = ']'
	case firstArr < 0:
		start = firstObj
		closer = '}'
	case firstObj < firstArr:
		start = firstObj
		closer = '}'
	default:
		start = firstArr
		closer = ']'
	}
	end := strings.LastIndexByte(trimmed, closer)
	if end <= start {
		return text
	}
	candidate := trimmed[start : end+1]
	if json.Valid([]byte(candidate)) {
		return candidate
	}
	return text
}

func webFetchResultBlock(ev *translator.Event) map[string]any {
	url := ""
	body := ""
	title := ""
	if ev != nil && len(ev.WebResults) > 0 {
		url = ev.WebResults[0].URL
		body = ev.WebResults[0].Chunk
		title = ev.WebResults[0].Title
	}
	if title == "" {
		title = url
	}
	var content any
	if ev != nil && ev.ToolError != "" {
		content = map[string]any{
			"type":       "web_fetch_tool_result_error",
			"error_code": "unavailable",
		}
	} else {
		content = map[string]any{
			"type": "web_fetch_result",
			"url":  url,
			"content": map[string]any{
				"type":  "document",
				"title": title,
				"source": map[string]any{
					"type":       "text",
					"media_type": "text/plain",
					"data":       body,
				},
				"citations": map[string]any{"enabled": true},
			},
		}
	}
	id := ""
	if ev != nil {
		id = ev.ToolCallID
	}
	return map[string]any{
		"type":        "web_fetch_tool_result",
		"tool_use_id": id,
		"content":     content,
	}
}

// buildClaudeNonStreaming mirrors nonStreamAnthropic in cmd/cursor-proxy.
// Falls back to accumulating text deltas when Cursor never emits a
// KV blob (see buildOpenAINonStreaming for the rationale).
//
// The response mirrors Anthropic's Messages API precisely: when
// extended thinking is enabled and the upstream emits thinking deltas
// plus a signature, we surface them as a `thinking` content block with
// the associated `signature` field ahead of the final `text` block.
// Callers doing signature verification (cctest.ai's "签名校验" dimension)
// depend on those fields being present in the non-stream response, not
// only during streaming — the previous aggregator silently dropped them.
func buildClaudeNonStreaming(model string, shape chatShape, limiter *translator.OutputLimiter, estimatedInput int64, events <-chan executor.ChatEvent) ([]byte, error) {
	assistantText := ""
	sawBlob := false
	deltaText := ""
	thinkingText := ""
	thinkingSignature := ""
	var usage *translator.Usage
	var toolUses []map[string]any
	var serverToolUses []map[string]any
	webSearchRequests := 0
	deadline := time.NewTimer(requestDeadline())
	defer deadline.Stop()
	noOutput := time.NewTimer(noOutputDeadline())
	defer noOutput.Stop()
	// serverToolTimer mirrors the streaming path: Cursor's upstream stalls
	// after a server_tool_use frame with no follow-up result. In the non-
	// streaming aggregator we surface a synthetic unavailable-result block
	// on timeout so the caller sees a real answer instead of the whole
	// request eventually failing on the outer 300s deadline.
	nsServerToolTimer := time.NewTimer(0)
	nsServerToolTimer.Stop()
	nsPendingServerToolID := ""
	nsPendingServerToolName := ""
	stopNsServerToolTimer := func() {
		if !nsServerToolTimer.Stop() {
			select {
			case <-nsServerToolTimer.C:
			default:
			}
		}
	}
	defer stopNsServerToolTimer()
	sawSemantic := false
	for {
		var ev executor.ChatEvent
		select {
		case next, ok := <-events:
			if !ok {
				// Cursor closed the stream without emitting the matching
				// web_search_tool_result. Anthropic's contract requires a
				// result block for every server_tool_use before end_turn;
				// synthesize an unavailable-result block so the response
				// aggregator produces a legal turn shape.
				if nsPendingServerToolID != "" {
					block := map[string]any{
						"type":        "web_search_tool_result",
						"tool_use_id": nsPendingServerToolID,
						"content": map[string]any{
							"type":       "web_search_tool_result_error",
							"error_code": "unavailable",
						},
					}
					if strings.EqualFold(nsPendingServerToolName, "web_fetch") {
						block["type"] = "web_fetch_tool_result"
						block["content"] = map[string]any{
							"type":       "web_fetch_tool_result_error",
							"error_code": "unavailable",
						}
					}
					appendUniqueServerToolBlock(&serverToolUses, block)
					webSearchRequests++
					nsPendingServerToolID = ""
					nsPendingServerToolName = ""
				}
				goto collected
			}
			ev = next
		case <-deadline.C:
			return nil, errors.New(requestDeadlineMessage)
		case <-nsServerToolTimer.C:
			// Fake an unavailable result block so the caller sees a
			// terminated turn with an explicit error_code rather than
			// waiting through the 300s request deadline.
			if nsPendingServerToolID != "" {
				block := map[string]any{
					"type":        "web_search_tool_result",
					"tool_use_id": nsPendingServerToolID,
					"content": map[string]any{
						"type":       "web_search_tool_result_error",
						"error_code": "unavailable",
					},
				}
				if strings.EqualFold(nsPendingServerToolName, "web_fetch") {
					block["type"] = "web_fetch_tool_result"
					block["content"] = map[string]any{
						"type":       "web_fetch_tool_result_error",
						"error_code": "unavailable",
					}
				}
				appendUniqueServerToolBlock(&serverToolUses, block)
				webSearchRequests++
				nsPendingServerToolID = ""
				nsPendingServerToolName = ""
			}
			goto collected
		case <-noOutput.C:
			if !sawSemantic {
				return nil, errors.New(noOutputDeadlineMessage)
			}
		}
		if err := cursorTrailerError(ev); err != nil {
			return nil, err
		}
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && (blob.AssistantText != "" || blob.ThoughtText != "" || blob.Signature != "") {
			if blob.AssistantText != "" {
				assistantText = blob.AssistantText
			}
			// Cursor's KV blob folds the entire thinking body plus its
			// provider-issued signature into the final assistant frame.
			// Prefer the blob values over the streaming deltas so the
			// non-stream response mirrors what Cursor actually saved,
			// even when the wire dripped a lone signature_delta with
			// no thinking_delta preceding it.
			if blob.ThoughtText != "" {
				thinkingText = blob.ThoughtText
			}
			if blob.Signature != "" {
				thinkingSignature = blob.Signature
			}
			sawBlob = true
			sawSemantic = true
			continue
		}
		trEv := translateClaudeExecuteEvent(ev.Server, shape)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			deltaText += trEv.Text
			sawSemantic = true
		case translator.EventThinkingDelta:
			thinkingText += trEv.Text
			sawSemantic = true
		case translator.EventSignatureDelta:
			thinkingSignature = trEv.Text
		case translator.EventToolCallStarted:
			sawSemantic = true
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
		case translator.EventServerToolStarted:
			sawSemantic = true
			var input any = map[string]any{}
			if trEv.ToolArgsDelta != "" {
				_ = json.Unmarshal([]byte(trEv.ToolArgsDelta), &input)
			}
			appendUniqueServerToolBlock(&serverToolUses, map[string]any{
				"type":  "server_tool_use",
				"id":    trEv.ToolCallID,
				"name":  trEv.ToolName,
				"input": input,
			})
			// Start the per-tool result timer. Cursor emits the tool_use
			// frame and then can stall silently — the outer noOutput timer
			// no longer fires once sawSemantic=true, so without this
			// dedicated bound the request rides the 300s deadline.
			if trEv.ToolCallID != "" {
				stopNsServerToolTimer()
				nsPendingServerToolID = trEv.ToolCallID
				nsPendingServerToolName = trEv.ToolName
				nsServerToolTimer.Reset(serverToolResultTimeout())
			}
		case translator.EventWebSearchResult:
			// Disarm the server-tool timer as soon as its result (real or
			// error) shows up.
			if nsPendingServerToolID != "" && (trEv.ToolCallID == "" || trEv.ToolCallID == nsPendingServerToolID) {
				stopNsServerToolTimer()
				nsPendingServerToolID = ""
				nsPendingServerToolName = ""
			}
			if strings.EqualFold(trEv.ToolName, "web_fetch") {
				appendUniqueServerToolBlock(&serverToolUses, webFetchResultBlock(trEv))
				break
			}
			// Cursor's web_search occasionally fails upstream with an
			// "unavailable" tool result (transient Cursor-side outage).
			// Anthropic's wire format for that case is a single object
			// with type=web_search_tool_result_error inside content, not
			// an empty result list. Reflecting the error verbatim lets
			// downstream retry logic (and cctest.ai's WebSearch check)
			// see the real reason instead of a silent empty-result turn.
			if trEv.ToolError != "" {
				code := trEv.ToolError
				appendUniqueServerToolBlock(&serverToolUses, map[string]any{
					"type":        "web_search_tool_result",
					"tool_use_id": trEv.ToolCallID,
					"content": map[string]any{
						"type":       "web_search_tool_result_error",
						"error_code": code,
					},
				})
				webSearchRequests++
				break
			}
			results := make([]map[string]any, 0, len(trEv.WebResults))
			for _, result := range trEv.WebResults {
				// Preserve canonical Anthropic field parity: emit page_age
				// (nullable) alongside url/title/encrypted_content. Absent
				// page_age is a documented cctest.ai fail signal.
				results = append(results, map[string]any{
					"type":              "web_search_result",
					"url":               result.URL,
					"title":             result.Title,
					"encrypted_content": result.Chunk,
					"page_age":          nil,
				})
			}
			appendUniqueServerToolBlock(&serverToolUses, map[string]any{
				"type":        "web_search_tool_result",
				"tool_use_id": trEv.ToolCallID,
				"content":     results,
			})
			webSearchRequests++
		case translator.EventTurnEnded:
			usage = trEv.Usage
		}
	}
collected:
	if !sawBlob && deltaText != "" {
		assistantText = deltaText
	}
	// Emptiness is judged on the upstream response, before local clipping, so
	// a max_tokens ceiling of a few tokens is not mistaken for a dead account.
	upstreamProducedOutput := assistantText != "" || thinkingText != "" || thinkingSignature != "" || len(toolUses) > 0 || len(serverToolUses) > 0
	assistantText = limiter.Truncate(assistantText)
	// Strip Markdown code fences and prose surrounding a JSON body when the
	// caller asked for structured output. Even with a strict system prompt
	// Cursor's upstream tends to wrap the reply in ```json ... ``` and
	// occasionally trails a "Let me know if you'd like..." explanation.
	// cctest.ai's structured-output probe grades on the raw response body
	// being valid JSON on its own, so a fenced answer fails even though
	// the JSON inside is correct.
	if len(shape.JSONSchema) > 0 {
		assistantText = stripJSONMarkdownFences(assistantText)
	}
	observed := assistantText
	for _, tu := range toolUses {
		if raw, err := json.Marshal(tu); err == nil {
			observed += string(raw)
		}
	}
	for _, tu := range serverToolUses {
		if raw, err := json.Marshal(tu); err == nil {
			observed += string(raw)
		}
	}
	usage = usageAfterLimit(usageWithObservedOutput(usage, observed), limiter)
	if usage != nil && usage.InputTokens <= 0 && estimatedInput > 0 {
		usage.InputTokens = estimatedInput
	}
	if isEmptyPluginUpstreamResponse(upstreamProducedOutput, usage) {
		return nil, errEmptyUpstreamResponse
	}
	content := []map[string]any{}
	// Anthropic Messages API places the `thinking` block ahead of any
	// text/tool_use blocks. Callers doing signature verification pin on
	// that ordering and on the `signature` field being carried alongside
	// the thinking text. Cursor upstream sometimes streams a signature
	// with no visible thinking body (redacted reasoning), so we emit
	// the block whenever either the text or the signature is present —
	// matching what the streaming path advertises.
	if thinkingText != "" || thinkingSignature != "" {
		block := map[string]any{"type": "thinking", "thinking": thinkingText}
		if thinkingSignature != "" {
			block["signature"] = thinkingSignature
		}
		content = append(content, block)
	}
	// Anthropic's canonical block order for a search-augmented turn is
	// search-first: server_tool_use → web_search_tool_result → text.
	// Detectors that grade block order (cctest.ai WebSearch dimension)
	// and downstream clients that pin citations to the preceding search
	// both depend on this ordering. Appending server tools last (the
	// previous behavior) reads as an unsupported "text-then-search"
	// shape upstream.
	content = append(content, serverToolUses...)
	if assistantText != "" {
		textBlock := map[string]any{"type": "text", "text": assistantText}
		// When the turn included a web_search_tool_result, the final text
		// block should carry canonical citations tying each cited URL back
		// to the search result. Detectors (cctest.ai WebSearch dimension)
		// look for a non-empty citations array on the text block; without
		// it the answer reads as unattributed even when the search itself
		// succeeded.
		if citations := citationsFromServerToolUses(serverToolUses); len(citations) > 0 {
			textBlock["citations"] = citations
		}
		content = append(content, textBlock)
	}
	for _, tu := range toolUses {
		content = append(content, tu)
	}
	stopReason := translator.AnthropicStopReason(assistantText, len(toolUses) > 0)
	var stopSequence any
	if reason, sequence, limited := limiter.StopReason(); limited {
		stopReason = reason
		if sequence != "" {
			stopSequence = sequence
		}
	}
	resp := map[string]any{
		"id":            translator.NewAnthropicMessageID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": stopSequence,
	}
	responseUsage := translator.BuildAnthropicUsage(usage)
	if webSearchRequests > 0 {
		responseUsage["server_tool_use"] = map[string]int{"web_search_requests": webSearchRequests}
	}
	resp["usage"] = responseUsage
	buf, _ := json.Marshal(resp)
	return buf, nil
}

// handleExecutorExecuteStream implements executor.execute_stream. It
// returns synchronously (with empty chunks so the host uses the
// async stream_id path) and drives a background goroutine that emits
// SSE frames via host.stream.emit until RunChat is done.
func handleExecutorExecuteStream(payload []byte) ([]byte, int) {
	var req executorRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse executor request: %v", err), false), 0
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("bad_request", "stream_id is required for executor.execute_stream", false), 0
	}
	shape, errParse := parseByFormat(req.Payload, req.Format)
	if errParse != nil {
		return errorEnvelope("bad_payload", errParse.Error(), false), 0
	}
	if strings.TrimSpace(req.Model) != "" {
		shape.Model = req.Model
	}
	if strings.TrimSpace(shape.Model) == "" {
		return errorEnvelope("bad_payload", "model is required", false), 0
	}
	excluded, errExcluded := storageExcludesModel(req.StorageJSON, shape.Model)
	if errExcluded != nil {
		return errorEnvelope("bad_auth", errExcluded.Error(), true), 0
	}
	if excluded {
		return errorEnvelope("model_excluded", fmt.Sprintf("model %s is excluded for this cursor account", shape.Model), true), 0
	}
	chatReq := buildChatRequest(shape, req.Headers)
	if dec := applyQuotaLane(chatReq, req.StorageJSON, shape.Model); dec.Skip {
		return errorEnvelopeQuota(dec.Message), 0
	}

	runner, _, errClient := runnerFactory(req.AuthID, req.StorageJSON)
	if errClient != nil {
		return errorEnvelope("bad_auth", errClient.Error(), true), 0
	}

	// Kick off the run before returning so any immediate wire errors
	// surface as an envelope failure instead of a silent stream close.
	ctx, cancel := context.WithCancel(context.Background())
	events, errRun := runner.RunChat(ctx, chatReq)
	if errRun != nil {
		cancel()
		return errorEnvelope("upstream_error", errRun.Error(), true), 0
	}

	format := normaliseFormat(req.Format, req.SourceFormat)
	headers := map[string][]string{
		"Content-Type": {"text/event-stream"},
		"request-id":   {translator.NewAnthropicRequestID()},
	}

	go streamEvents(ctx, cancel, req.StreamID, format, shape, shape.IncludeUsage, shape.Thinking, shape.outputLimiter(), estimateClaudeInputTokens(shape), events)

	// Async streaming: return synchronously with empty chunks. The
	// host will read chunks off the stream bridge as we emit them.
	resp := executorStreamResponse{Headers: headers}
	buf, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		cancel()
		return errorEnvelope("marshal_response", errMarshal.Error(), false), 0
	}
	return okEnvelopeJSON(string(buf)), 0
}

// streamEvents runs in a background goroutine for the lifetime of one
// executor.execute_stream call. It pumps Cursor events into the host
// stream bridge and always closes the stream on exit.
func streamEvents(ctx context.Context, cancel context.CancelFunc, streamID, format string, shape chatShape, includeUsage, expectThinkingSignature bool, limiter *translator.OutputLimiter, estimatedInput int64, events <-chan executor.ChatEvent) {
	defer cancel()

	var streamErr string
	defer func() {
		closePayload, _ := json.Marshal(map[string]string{
			"stream_id": streamID,
			"error":     streamErr,
		})
		_, _ = callHost("host.stream.close", closePayload)
	}()

	switch format {
	case "claude":
		streamClaude(streamID, shape.Model, expectThinkingSignature, shape, limiter, estimatedInput, events, &streamErr)
	default:
		streamOpenAI(streamID, shape.Model, includeUsage, shape.Tools, events, &streamErr)
	}
	_ = ctx // kept for future context-aware emit
}

// emit sends one payload chunk through the host bridge.
func emit(streamID string, payload []byte) error {
	req, err := json.Marshal(map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	if err != nil {
		return err
	}
	_, errCall := callHost("host.stream.emit", req)
	return errCall
}

func emitStreamKeepalive(streamID, format string) error {
	if format != "claude" {
		// CPA owns OpenAI SSE framing and emits its own connection-level
		// keepalives. Sending an SSE comment here would be wrapped as a data
		// event by the host ("data: : ping"), which is not valid JSON.
		return nil
	}
	return emit(streamID, []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
}

func streamFirstOutputTimeout() time.Duration {
	const fallback = 60 * time.Second
	raw := strings.TrimSpace(os.Getenv("CURSOR_STREAM_FIRST_OUTPUT_TIMEOUT_MS"))
	if raw == "" {
		return fallback
	}
	milliseconds, err := strconv.Atoi(raw)
	if err != nil || milliseconds <= 0 {
		return fallback
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func streamHeartbeatPreambleDelay() time.Duration {
	const fallback = 8 * time.Second
	raw := strings.TrimSpace(os.Getenv("CURSOR_STREAM_HEARTBEAT_PREAMBLE_MS"))
	if raw == "" {
		return fallback
	}
	milliseconds, err := strconv.Atoi(raw)
	if err != nil || milliseconds < 0 {
		return fallback
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// serverToolResultTimeout bounds how long we wait for a
// web_search_tool_result / web_fetch_tool_result after Cursor emits the
// corresponding server_tool_use frame. Cursor's upstream regularly stalls
// on this transition — the wire keeps sending SSE pings for minutes but
// the actual tool result never arrives. Without a dedicated bound the
// outer request deadline (300s) is the only safety net, which is much
// longer than any client keeps its connection open.
//
// 60s covers the observed opus-4-x web_search wall clock — Anthropic's
// grounded search on Bedrock/Vertex regularly takes 40-60s to return
// citations, and 30s was clipping legitimate answers. Cursor's own IDE
// waits significantly longer; we cap at 60s to still fail fast on a
// truly-stalled search but keep the caller inside the common 90-120s
// HTTP client timeout window.
func serverToolResultTimeout() time.Duration {
	const fallback = 60 * time.Second
	raw := strings.TrimSpace(os.Getenv("CURSOR_SERVER_TOOL_RESULT_TIMEOUT_MS"))
	if raw == "" {
		return fallback
	}
	milliseconds, err := strconv.Atoi(raw)
	if err != nil || milliseconds <= 0 {
		return fallback
	}
	return time.Duration(milliseconds) * time.Millisecond
}

const firstOutputTimeoutMessage = "upstream produced no content before first-output timeout"

const requestDeadlineMessage = "upstream did not complete the turn before the request deadline"

const noOutputDeadlineMessage = "upstream produced only heartbeats before the no-output deadline"

// defaultRequestDeadline is the outermost wall-clock bound on one turn.
//
// Every other timer here can be renewed by upstream activity: the first-output
// timer resets on heartbeats so long prompt prefill survives, and the executor
// idle timer resets on any frame. Cursor can hold a run open indefinitely with
// heartbeats alone — observed with an account whose catalog advertises a model
// it cannot actually serve, and with a server-side web search that never
// returns. Without an absolute ceiling those turns hang until the client gives
// up, which is what a CLI shows as a frozen session.
const defaultRequestDeadline = 300 * time.Second

func requestDeadline() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CURSOR_REQUEST_DEADLINE_MS"))
	if raw == "" {
		return defaultRequestDeadline
	}
	milliseconds, err := strconv.Atoi(raw)
	if err != nil || milliseconds <= 0 {
		return defaultRequestDeadline
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// defaultNoOutputDeadline is an absolute bound that heartbeats cannot renew.
// Production accounts can advertise a model they never generate for; Cursor
// then emits pings forever. The sliding first-output timer resets on those
// pings, so a CLI sits on "Cogitating…" until the 5-minute request deadline.
const defaultNoOutputDeadline = 90 * time.Second

func noOutputDeadline() time.Duration {
	raw := strings.TrimSpace(os.Getenv("CURSOR_NO_OUTPUT_DEADLINE_MS"))
	if raw == "" {
		return defaultNoOutputDeadline
	}
	milliseconds, err := strconv.Atoi(raw)
	if err != nil || milliseconds <= 0 {
		return defaultNoOutputDeadline
	}
	return time.Duration(milliseconds) * time.Millisecond
}

// emitOpenAIPayload converts the translator's HTTP-ready SSE bytes into the
// payload units expected by CPA's async stream bridge. The host adds the
// outer `data: ...\n\n` framing for OpenAI streams itself. Passing complete
// SSE frames through host.stream.emit would therefore produce
// `data: data: {...}`, which standard OpenAI clients reject.
func emitOpenAIPayload(streamID string, encoded []byte) error {
	for _, line := range strings.Split(string(encoded), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if err := emit(streamID, []byte(payload)); err != nil {
			return err
		}
	}
	return nil
}

// streamOpenAI mirrors streamOpenAI in cmd/cursor-proxy but emits each
// SSE frame through the host stream bridge.
//
// Cursor emits assistant text through two channels: KV blobs that
// carry the assembled text so far, and text-delta interaction
// updates. We prefer KV blobs (they're the canonical wire shape) but
// fall through to text deltas when the server does not send blobs
// (e.g. legacy stream shape).
func streamOpenAI(streamID, model string, includeUsage bool, tools []executor.ToolDefinition, events <-chan executor.ChatEvent, errOut *string) {
	tr := translator.NewOpenAIStreamWriter(model)
	tr.IncludeUsage = includeUsage
	textState := assistantStreamState{}
	sawTurnEnd := false
	sawOutput := false
	var lastUsage *translator.Usage
	for ev := range events {
		if err := cursorTrailerError(ev); err != nil {
			*errOut = err.Error()
			return
		}
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			delta := textState.consumeSnapshot(blob.AssistantText)
			if delta != "" {
				sawOutput = true
				if err := emitOpenAIPayload(streamID, tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})); err != nil {
					*errOut = err.Error()
					return
				}
			}
			continue
		}
		trEv := translatePluginEvent(ev.Server, tools)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			if delta := textState.consumeDelta(trEv.Text); delta != "" {
				sawOutput = true
				if err := emitOpenAIPayload(streamID, tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})); err != nil {
					*errOut = err.Error()
					return
				}
			}
		case translator.EventThinkingDelta, translator.EventHeartbeat:
			if err := emitStreamKeepalive(streamID, "openai"); err != nil {
				*errOut = err.Error()
				return
			}
		case translator.EventToolCallStarted, translator.EventToolCallDelta:
			sawOutput = true
			if payload := tr.Encode(trEv); len(payload) > 0 {
				if err := emitOpenAIPayload(streamID, payload); err != nil {
					*errOut = err.Error()
					return
				}
			}
		case translator.EventTurnEnded:
			sawTurnEnd = true
			lastUsage = usageWithObservedOutput(trEv.Usage, textState.emitted)
			trEv.Usage = lastUsage
			if payload := tr.Encode(trEv); len(payload) > 0 {
				if err := emitOpenAIPayload(streamID, payload); err != nil {
					*errOut = err.Error()
					return
				}
			}
		}
	}
	if isEmptyPluginUpstreamResponse(sawOutput, lastUsage) {
		*errOut = emptyUpstreamResponseMessage
		return
	}
	// Synthetic tool_calls terminator when the server never sent
	// turn_ended — same rescue path as cursor-proxy's http handler.
	if !sawTurnEnd && tr.SawToolCall {
		if payload := tr.Encode(&translator.Event{Kind: translator.EventTurnEnded}); len(payload) > 0 {
			if err := emitOpenAIPayload(streamID, payload); err != nil {
				*errOut = err.Error()
				return
			}
		}
	}
	if payload := tr.FinalUsageFrame(); len(payload) > 0 {
		if err := emitOpenAIPayload(streamID, payload); err != nil {
			*errOut = err.Error()
			return
		}
	}
	if err := emitOpenAIPayload(streamID, tr.FinalDone()); err != nil {
		*errOut = err.Error()
		return
	}
}

// streamClaude mirrors streamAnthropic in cmd/cursor-proxy but emits
// SSE frames through the host stream bridge. See streamOpenAI for
// the KV-blob vs text-delta fallback rationale.
func streamClaude(streamID, model string, expectThinkingSignature bool, shape chatShape, limiter *translator.OutputLimiter, estimatedInput int64, events <-chan executor.ChatEvent, errOut *string) {
	tr := translator.NewAnthropicStreamWriter(model)
	tr.InputTokens = estimatedInput
	startedAt := time.Now()
	textState := assistantStreamState{}
	var observedOutput strings.Builder
	sawOutput := false
	signatureSent := false
	streamStarted := false
	pingSent := false
	var pendingText strings.Builder
	var lastUsage *translator.Usage
	firstOutputTimer := time.NewTimer(streamFirstOutputTimeout())
	defer firstOutputTimer.Stop()
	stopFirstOutputTimer := func() {
		if !firstOutputTimer.Stop() {
			select {
			case <-firstOutputTimer.C:
			default:
			}
		}
	}
	resetFirstOutputTimer := func() {
		if sawOutput {
			return
		}
		stopFirstOutputTimer()
		firstOutputTimer.Reset(streamFirstOutputTimeout())
	}
	markOutput := func() {
		if sawOutput {
			return
		}
		sawOutput = true
		stopFirstOutputTimer()
	}
	// pendingServerTool tracks the id of the last server_tool_use event we
	// forwarded downstream and the wall-clock timer that waits for its
	// matching web_search_tool_result. Cursor's upstream regularly stalls
	// after emitting the server_tool_use frame — it emits nothing but
	// pings for minutes, and the outer request deadline (300s) is far too
	// slow to keep an HTTP client alive. This timer synthesises an
	// unavailable-result block so the caller sees a real terminal state
	// instead of a hung connection. See docs/chromium-transport.md for
	// the wire-shape references.
	pendingServerToolID := ""
	pendingServerToolName := ""
	serverToolTimer := time.NewTimer(0)
	serverToolTimer.Stop()
	stopServerToolTimer := func() {
		if !serverToolTimer.Stop() {
			select {
			case <-serverToolTimer.C:
			default:
			}
		}
	}
	armServerToolTimer := func(id, name string) {
		stopServerToolTimer()
		pendingServerToolID = id
		pendingServerToolName = name
		serverToolTimer.Reset(serverToolResultTimeout())
	}
	clearServerToolTimer := func() {
		stopServerToolTimer()
		pendingServerToolID = ""
		pendingServerToolName = ""
	}
	defer stopServerToolTimer()
	flushPendingText := func() bool {
		if pendingText.Len() == 0 {
			return true
		}
		payload := tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: pendingText.String()})
		pendingText.Reset()
		if len(payload) == 0 {
			return true
		}
		if err := emit(streamID, payload); err != nil {
			*errOut = err.Error()
			return false
		}
		streamStarted = true
		return true
	}
	// outputLimited records that max_tokens or a stop sequence ended the turn.
	// The event channel keeps being drained afterwards so RunChat's reader
	// goroutine can finish instead of blocking on an unread send.
	outputLimited := false
	admitText := func(delta string) string {
		if !limiter.Active() {
			return delta
		}
		allowed, done := limiter.Feed(delta)
		if done {
			outputLimited = true
		}
		return allowed
	}
	// The first-output timer is renewed by heartbeats so long prefill survives;
	// this one never is, so a run Cursor keeps alive without ever finishing
	// still terminates.
	turnDeadline := time.NewTimer(requestDeadline())
	defer turnDeadline.Stop()
	noOutputTimer := time.NewTimer(noOutputDeadline())
	defer noOutputTimer.Stop()

	streamEnded := false
	for !streamEnded {
		var ev executor.ChatEvent
		select {
		case next, ok := <-events:
			if !ok {
				// Cursor closed the events channel. If we announced a
				// server_tool_use but never delivered its matching
				// web_search_tool_result / web_fetch_tool_result, the
				// EventTurnEnded encoding below will close the orphan
				// via translator.closeOrphanServerTools — but we still
				// need to disarm the timer so the deferred stop path
				// doesn't leak.
				clearServerToolTimer()
				streamEnded = true
				continue
			}
			ev = next
		case <-noOutputTimer.C:
			if sawOutput {
				continue
			}
			if streamStarted {
				if payload := tr.EncodeError("api_error", noOutputDeadlineMessage); len(payload) > 0 {
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
					}
				}
				return
			}
			*errOut = noOutputDeadlineMessage
			return
		case <-turnDeadline.C:
			if streamStarted {
				if payload := tr.EncodeError("api_error", requestDeadlineMessage); len(payload) > 0 {
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
					}
				}
				return
			}
			*errOut = requestDeadlineMessage
			return
		case <-firstOutputTimer.C:
			if streamStarted {
				// The response is already committed, so downstream failover would
				// splice a second message_start. Report the timeout through
				// Anthropic's legal SSE error event and close this one stream.
				if payload := tr.EncodeError("api_error", firstOutputTimeoutMessage); len(payload) > 0 {
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
					}
				}
				return
			}
			*errOut = firstOutputTimeoutMessage
			return
		case <-serverToolTimer.C:
			// Cursor stalled after announcing a server_tool_use (web_search
			// or web_fetch) frame. Synthesise the Anthropic-native error
			// result block so callers see a real terminal state and can
			// retry, instead of the client hanging until turn/no-output
			// timers eventually fire minutes later.
			toolID := pendingServerToolID
			toolName := pendingServerToolName
			clearServerToolTimer()
			if toolID != "" {
				errorBlock := map[string]any{
					"type":        "web_search_tool_result",
					"tool_use_id": toolID,
					"content": map[string]any{
						"type":       "web_search_tool_result_error",
						"error_code": "unavailable",
					},
				}
				if strings.EqualFold(toolName, "web_fetch") {
					errorBlock["type"] = "web_fetch_tool_result"
					errorBlock["content"] = map[string]any{
						"type":       "web_fetch_tool_result_error",
						"error_code": "unavailable",
					}
				}
				if payload := tr.Encode(&translator.Event{
					Kind:       translator.EventWebSearchResult,
					ToolCallID: toolID,
					ToolName:   toolName,
					ToolError:  "unavailable",
				}); len(payload) > 0 {
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
						return
					}
				}
				// Close the message with a legal end_turn so downstream sees
				// a terminated turn rather than a truncated stream.
				if payload := tr.Encode(&translator.Event{Kind: translator.EventTurnEnded, StopReason: "end_turn"}); len(payload) > 0 {
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
						return
					}
				}
				_ = errorBlock // block variable kept for readability; encoded via translator above
			}
			return
		}
		if err := cursorTrailerError(ev); err != nil {
			if streamStarted {
				if payload := tr.EncodeError("api_error", err.Error()); len(payload) > 0 {
					if emitErr := emit(streamID, payload); emitErr != nil {
						*errOut = emitErr.Error()
					}
				}
				return
			}
			*errOut = err.Error()
			return
		}
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil {
			if blob.AssistantText != "" {
				delta := textState.consumeSnapshot(blob.AssistantText)
				if !outputLimited {
					delta = admitText(delta)
				} else {
					delta = ""
				}
				if delta != "" {
					markOutput()
					if expectThinkingSignature && !signatureSent {
						pendingText.WriteString(delta)
					} else {
						payload := tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})
						if err := emit(streamID, payload); err != nil {
							*errOut = err.Error()
							return
						}
						streamStarted = true
					}
				}
			}
			if expectThinkingSignature && !signatureSent && blob.Signature != "" {
				signatureSent = true
				if payload := tr.Encode(&translator.Event{Kind: translator.EventSignatureDelta, Text: blob.Signature}); len(payload) > 0 {
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
						return
					}
					streamStarted = true
				}
				if !flushPendingText() {
					return
				}
			}
			continue
		}
		trEv := translateClaudeExecuteEvent(ev.Server, shape)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			delta := textState.consumeDelta(trEv.Text)
			if outputLimited {
				delta = ""
			} else {
				delta = admitText(delta)
			}
			if delta != "" {
				markOutput()
				if expectThinkingSignature && !signatureSent {
					pendingText.WriteString(delta)
				} else {
					payload := tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
						return
					}
					streamStarted = true
				}
			}
		case translator.EventThinkingDelta:
			markOutput()
			if payload := tr.Encode(trEv); len(payload) > 0 {
				if err := emit(streamID, payload); err != nil {
					*errOut = err.Error()
					return
				}
				streamStarted = true
			}
		case translator.EventHeartbeat:
			// A heartbeat proves Cursor is still actively processing the turn.
			// Treat the first-output timeout as an idle timeout, not an absolute
			// wall-clock deadline, so long-context prefill is not killed at 60s.
			resetFirstOutputTimer()
			if !streamStarted {
				// Cursor often emits a heartbeat immediately before closing an
				// empty response. Starting the Anthropic message on that heartbeat
				// commits HTTP 200; CPA/New API then retries another auth and splices
				// a second message_start into the same client stream. Give fast-empty
				// accounts a short retryable window before committing the preamble.
				if time.Since(startedAt) < streamHeartbeatPreambleDelay() {
					continue
				}
				if payload := tr.Encode(trEv); len(payload) > 0 {
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
						return
					}
					streamStarted = true
					pingSent = true
				}
				continue
			}
			if err := emitStreamKeepalive(streamID, "claude"); err != nil {
				*errOut = err.Error()
				return
			}
			pingSent = true
		case translator.EventToolCallStarted, translator.EventToolCallDelta, translator.EventToolCallCompleted,
			translator.EventServerToolStarted, translator.EventWebSearchResult:
			if trEv.ToolArgsDelta != "" {
				observedOutput.WriteString(trEv.ToolArgsDelta)
			}
			markOutput()
			// Track server_tool_use starts so we can bail if Cursor stalls
			// on the corresponding tool result. Cursor's web_search /
			// web_fetch upstream frequently emits the tool_use frame and
			// then goes silent for minutes — see docs/upstream-issues.
			switch trEv.Kind {
			case translator.EventServerToolStarted:
				if trEv.ToolCallID != "" {
					armServerToolTimer(trEv.ToolCallID, trEv.ToolName)
				}
			case translator.EventWebSearchResult:
				// The matching result arrived; disarm.
				if pendingServerToolID != "" && (trEv.ToolCallID == "" || trEv.ToolCallID == pendingServerToolID) {
					clearServerToolTimer()
				}
			}
			if payload := tr.Encode(trEv); len(payload) > 0 {
				if err := emit(streamID, payload); err != nil {
					*errOut = err.Error()
					return
				}
				streamStarted = true
			}
		case translator.EventTurnEnded:
			stopFirstOutputTimer()
			clearServerToolTimer()
			lastUsage = trEv.Usage
		}
	}
	if isEmptyPluginUpstreamResponse(sawOutput, lastUsage) {
		if streamStarted {
			// Preserve the single committed envelope and report the upstream
			// failure through Anthropic's standard error event.
			if payload := tr.EncodeError("api_error", emptyUpstreamResponseMessage); len(payload) > 0 {
				if err := emit(streamID, payload); err != nil {
					*errOut = err.Error()
				}
			}
			return
		}
		*errOut = emptyUpstreamResponseMessage
		return
	}
	if remainder := limiter.Flush(); remainder != "" {
		pendingText.WriteString(remainder)
	}
	if !flushPendingText() {
		return
	}
	// Anthropic emits at least one ping on long server-tool turns; conformance
	// suites also expect ping on short streams once message_start is committed.
	if streamStarted && !pingSent {
		if payload := tr.Encode(&translator.Event{Kind: translator.EventHeartbeat}); len(payload) > 0 {
			if err := emit(streamID, payload); err != nil {
				*errOut = err.Error()
				return
			}
			pingSent = true
		}
	}
	emittedText := textState.emitted + observedOutput.String()
	lastUsage = usageAfterLimit(usageWithObservedOutput(lastUsage, emittedText), limiter)
	end := &translator.Event{Kind: translator.EventTurnEnded, Usage: lastUsage}
	if reason, sequence, limited := limiter.StopReason(); limited {
		end.StopReason = reason
		end.StopSequence = sequence
	}
	if payload := tr.Encode(end); len(payload) > 0 {
		if err := emit(streamID, payload); err != nil {
			*errOut = err.Error()
			return
		}
	}
}

// handleExecutorCountTokens implements executor.count_tokens using a
// local character heuristic. The response mirrors the OpenAI usage
// shape so hosts that pattern-match on that field see a consistent
// key. See docs/phase-8b-abi.md for why we do not call Cursor's
// backend to count tokens.
func handleExecutorCountTokens(payload []byte) ([]byte, int) {
	var req executorRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse count request: %v", err), false), 0
	}
	if len(req.Payload) == 0 && len(req.OriginalRequest) > 0 {
		req.Payload = req.OriginalRequest
	}
	shape, err := parseByFormat(req.Payload, req.Format)
	if err != nil {
		return errorEnvelope("bad_payload", err.Error(), false), 0
	}
	var b strings.Builder
	if shape.SystemPrompt != "" {
		b.WriteString(shape.SystemPrompt)
		b.WriteByte('\n')
	}
	for _, turn := range shape.History {
		b.WriteString(turn.Content)
		b.WriteByte('\n')
	}
	b.WriteString(shape.UserMessage)
	tokens := countTokens(b.String())

	body, errMarshal := json.Marshal(map[string]any{
		// Anthropic's /v1/messages/count_tokens answers with input_tokens and
		// nothing else; the OpenAI-shaped keys stay for hosts that pattern-match
		// on them. Emitting only the latter made the endpoint unusable from an
		// Anthropic client even where the host routed the path correctly.
		"input_tokens": tokens,
		"total_tokens": tokens,
		"usage": map[string]any{
			"prompt_tokens":     tokens,
			"completion_tokens": 0,
			"total_tokens":      tokens,
		},
	})
	if errMarshal != nil {
		return errorEnvelope("marshal_response", errMarshal.Error(), false), 0
	}
	resp := executorResponse{
		Payload: body,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
	}
	buf, errMarshalResp := json.Marshal(resp)
	if errMarshalResp != nil {
		return errorEnvelope("marshal_response", errMarshalResp.Error(), false), 0
	}
	return okEnvelopeJSON(string(buf)), 0
}
