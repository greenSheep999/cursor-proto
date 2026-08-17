package translator

import (
	"encoding/json"
	"strings"
	"testing"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

func TestOpenAIStreamShape(t *testing.T) {
	w := NewOpenAIStreamWriter("composer-2.5")

	// First text delta should emit two frames: role delta, then content delta.
	first := string(w.Encode(&Event{Kind: EventTextDelta, Text: "Hello"}))
	if strings.Count(first, "data: ") != 2 {
		t.Errorf("first delta should emit 2 frames, got:\n%s", first)
	}
	if !strings.Contains(first, `"role":"assistant"`) {
		t.Errorf("missing role delta:\n%s", first)
	}
	if !strings.Contains(first, `"content":"Hello"`) {
		t.Errorf("missing content delta:\n%s", first)
	}

	// Second delta = one frame with content only.
	second := string(w.Encode(&Event{Kind: EventTextDelta, Text: " World"}))
	if strings.Count(second, "data: ") != 1 {
		t.Errorf("second delta should emit 1 frame, got:\n%s", second)
	}
	if strings.Contains(second, `"role"`) {
		t.Errorf("second delta must not repeat role:\n%s", second)
	}

	// Turn ended = final frame with finish_reason=stop.
	end := string(w.Encode(&Event{Kind: EventTurnEnded, Usage: &Usage{InputTokens: 10, OutputTokens: 5}}))
	if !strings.Contains(end, `"finish_reason":"stop"`) {
		t.Errorf("missing finish_reason on turn end:\n%s", end)
	}

	// FinalDone marker
	if string(w.FinalDone()) != "data: [DONE]\n\n" {
		t.Error("FinalDone shape wrong")
	}
}

func TestAnthropicStreamShape(t *testing.T) {
	w := NewAnthropicStreamWriter("claude-opus-4-8")

	first := string(w.Encode(&Event{Kind: EventTextDelta, Text: "Hello"}))
	// Should include message_start + content_block_start + content_block_delta
	for _, kw := range []string{"event: message_start", "event: content_block_start", "event: content_block_delta"} {
		if !strings.Contains(first, kw) {
			t.Errorf("missing %q in first delta:\n%s", kw, first)
		}
	}

	end := string(w.Encode(&Event{Kind: EventTurnEnded, Usage: &Usage{InputTokens: 10, OutputTokens: 5}}))
	for _, kw := range []string{"event: content_block_stop", "event: message_delta", "event: message_stop", `"stop_reason":"end_turn"`} {
		if !strings.Contains(end, kw) {
			t.Errorf("missing %q in end frames:\n%s", kw, end)
		}
	}
}

func TestAnthropicHeartbeatStartsStreamBeforePing(t *testing.T) {
	w := NewAnthropicStreamWriter("claude-opus-5")
	frame := string(w.Encode(&Event{Kind: EventHeartbeat}))
	startAt := strings.Index(frame, "event: message_start")
	pingAt := strings.Index(frame, "event: ping")
	if startAt < 0 || pingAt < 0 || startAt > pingAt {
		t.Fatalf("heartbeat must emit message_start before ping:\n%s", frame)
	}
	if !strings.Contains(frame, `{"type":"ping"}`) {
		t.Fatalf("heartbeat ping payload missing:\n%s", frame)
	}
}

func TestAnthropicStreamInfersRefusalStopReason(t *testing.T) {
	w := NewAnthropicStreamWriter("claude-fable-5")
	w.Encode(&Event{Kind: EventTextDelta, Text: "我不能满足这个请求。"})
	end := string(w.Encode(&Event{Kind: EventTurnEnded}))
	if !strings.Contains(end, `"stop_reason":"refusal"`) {
		t.Fatalf("refusal response kept generic stop reason: %s", end)
	}
}

func TestIsRefusalTextDoesNotMatchOrdinaryNegation(t *testing.T) {
	if IsRefusalText("这并不意味着我不能帮助你完成正常任务。") {
		t.Fatal("ordinary explanatory text must not be classified as refusal")
	}
}

func TestIsRefusalTextRecognizesChineseUnablePhrase(t *testing.T) {
	if !IsRefusalText("我没办法照做，请见谅。") {
		t.Fatal("direct Chinese inability phrase must be classified as refusal")
	}
}

func TestAnthropicStreamMessageIDUsesClaudeShape(t *testing.T) {
	w := NewAnthropicStreamWriter("claude-opus-5")

	if !strings.HasPrefix(w.ID, "msg_01") || len(w.ID) != len("msg_01")+22 {
		t.Fatalf("message id %q must use Anthropic's msg_01 plus 22 character shape", w.ID)
	}
	if strings.Contains(w.ID, "-") {
		t.Fatalf("message id %q must not contain hyphens", w.ID)
	}
}

func TestNewAnthropicMessageIDUsesClaudeShape(t *testing.T) {
	id := NewAnthropicMessageID()
	if !strings.HasPrefix(id, "msg_01") || len(id) != len("msg_01")+22 {
		t.Fatalf("message id %q must use Anthropic's msg_01 plus 22 character shape", id)
	}
}

func TestAnthropicMessageStartIncludesStableUsageShape(t *testing.T) {
	w := NewAnthropicStreamWriter("claude-opus-5")
	frame := string(w.Encode(&Event{Kind: EventTextDelta, Text: "ok"}))
	for _, field := range []string{
		`"input_tokens":0`,
		`"output_tokens":0`,
		`"cache_creation_input_tokens":0`,
		`"cache_read_input_tokens":0`,
	} {
		if !strings.Contains(frame, field) {
			t.Fatalf("message_start missing %s:\n%s", field, frame)
		}
	}
}

func TestNonStreamingAccumulator(t *testing.T) {
	a := NonStreamingAccumulator{Model: "composer-2.5"}
	a.Consume(&Event{Kind: EventTextDelta, Text: "Hello, "})
	a.Consume(&Event{Kind: EventTextDelta, Text: "world."})
	a.Consume(&Event{Kind: EventTurnEnded, Usage: &Usage{InputTokens: 3, OutputTokens: 2}})

	if a.Text != "Hello, world." {
		t.Errorf("text = %q, want %q", a.Text, "Hello, world.")
	}

	body := a.Response("chatcmpl-test")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("json: %v", err)
	}
	choices := got["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "Hello, world." {
		t.Errorf("content field mismatch: %v", msg["content"])
	}
	usage := got["usage"].(map[string]any)
	if usage["total_tokens"] != float64(5) {
		t.Errorf("total_tokens = %v, want 5", usage["total_tokens"])
	}
}

func TestFromServerMessageNil(t *testing.T) {
	if FromServerMessage(nil) != nil {
		t.Error("nil input should map to nil event")
	}
}

func TestFromServerMessageNativeWebSearch(t *testing.T) {
	toolCall := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_WebSearchToolCall{
			WebSearchToolCall: &cursorpb.AgentV1_WebSearchToolCall{
				Args: &cursorpb.AgentV1_WebSearchArgs{
					SearchTerm: "Cursor official homepage",
					ToolCallId: "srvtoolu_1",
				},
			},
		},
	}
	started := FromServerMessage(&cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &cursorpb.AgentV1_ToolCallStartedUpdate{
						CallId:   "srvtoolu_1",
						ToolCall: toolCall,
					},
				},
			},
		},
	})
	if started == nil || started.Kind != EventServerToolStarted || started.ToolName != "web_search" {
		t.Fatalf("unexpected native web search start: %#v", started)
	}
	if started.ToolArgsDelta != `{"query":"Cursor official homepage"}` {
		t.Fatalf("unexpected native web search args: %s", started.ToolArgsDelta)
	}

	toolCall.GetWebSearchToolCall().Result = &cursorpb.AgentV1_WebSearchResult{
		Result: &cursorpb.AgentV1_WebSearchResult_Success{
			Success: &cursorpb.AgentV1_WebSearchSuccess{
				References: []*cursorpb.AgentV1_WebSearchReference{{
					Title: "Cursor",
					Url:   "https://cursor.com",
					Chunk: "Cursor homepage",
				}},
			},
		},
	}
	completed := FromServerMessage(&cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallCompleted{
					ToolCallCompleted: &cursorpb.AgentV1_ToolCallCompletedUpdate{
						CallId:   "srvtoolu_1",
						ToolCall: toolCall,
					},
				},
			},
		},
	})
	if completed == nil || completed.Kind != EventWebSearchResult || len(completed.WebResults) != 1 {
		t.Fatalf("unexpected native web search completion: %#v", completed)
	}
	if completed.WebResults[0].URL != "https://cursor.com" {
		t.Fatalf("unexpected native web search reference: %#v", completed.WebResults[0])
	}
}

// TestOpenAINonStreamingCacheUsage verifies that cache_read_tokens and
// reasoning_tokens surface in the OpenAI non-streaming response under the
// prompt_tokens_details / completion_tokens_details keys, and that
// total_tokens = prompt + completion.
func TestOpenAINonStreamingCacheUsage(t *testing.T) {
	a := NonStreamingAccumulator{Model: "composer-2.5"}
	a.Consume(&Event{Kind: EventTextDelta, Text: "hello"})
	a.Consume(&Event{Kind: EventTurnEnded, Usage: &Usage{
		InputTokens:     100,
		OutputTokens:    50,
		CacheReadTokens: 60,
		ReasoningTokens: 5,
	}})

	body := a.Response("chatcmpl-test")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	usage, ok := got["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage: %s", body)
	}
	if v, _ := usage["total_tokens"].(float64); v != 150 {
		t.Errorf("total_tokens = %v, want 150", usage["total_tokens"])
	}
	if v, _ := usage["prompt_tokens"].(float64); v != 100 {
		t.Errorf("prompt_tokens = %v, want 100", usage["prompt_tokens"])
	}
	if v, _ := usage["completion_tokens"].(float64); v != 50 {
		t.Errorf("completion_tokens = %v, want 50", usage["completion_tokens"])
	}
	pdet, ok := usage["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("missing prompt_tokens_details: %s", body)
	}
	if v, _ := pdet["cached_tokens"].(float64); v != 60 {
		t.Errorf("prompt_tokens_details.cached_tokens = %v, want 60", pdet["cached_tokens"])
	}
	cdet, ok := usage["completion_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("missing completion_tokens_details: %s", body)
	}
	if v, _ := cdet["reasoning_tokens"].(float64); v != 5 {
		t.Errorf("completion_tokens_details.reasoning_tokens = %v, want 5", cdet["reasoning_tokens"])
	}
}

// TestOpenAIStreamingIncludeUsageFinalFrame verifies that when
// IncludeUsage=true, FinalUsageFrame emits a chunk with empty choices and a
// usage block carrying the cache/reasoning breakdown from the last turn_ended.
func TestOpenAIStreamingIncludeUsageFinalFrame(t *testing.T) {
	w := NewOpenAIStreamWriter("composer-2.5")
	w.IncludeUsage = true
	_ = w.Encode(&Event{Kind: EventTextDelta, Text: "hi"})
	_ = w.Encode(&Event{Kind: EventTurnEnded, Usage: &Usage{
		InputTokens:     100,
		OutputTokens:    50,
		CacheReadTokens: 60,
		ReasoningTokens: 5,
	}})
	frame := string(w.FinalUsageFrame())
	if frame == "" {
		t.Fatal("FinalUsageFrame returned empty")
	}
	if !strings.HasPrefix(frame, "data: ") {
		t.Errorf("frame not SSE: %s", frame)
	}
	// Strip "data: " prefix and trailing \n\n; parse JSON.
	trimmed := strings.TrimPrefix(frame, "data: ")
	trimmed = strings.TrimSuffix(trimmed, "\n\n")
	var payload map[string]any
	if err := json.Unmarshal([]byte(trimmed), &payload); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, trimmed)
	}
	choices, _ := payload["choices"].([]any)
	if len(choices) != 0 {
		t.Errorf("final usage frame choices must be empty, got %v", choices)
	}
	usage, ok := payload["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage in final frame: %s", trimmed)
	}
	if v, _ := usage["total_tokens"].(float64); v != 150 {
		t.Errorf("total_tokens = %v, want 150", usage["total_tokens"])
	}
	pdet, _ := usage["prompt_tokens_details"].(map[string]any)
	if v, _ := pdet["cached_tokens"].(float64); v != 60 {
		t.Errorf("cached_tokens = %v, want 60", pdet["cached_tokens"])
	}
	cdet, _ := usage["completion_tokens_details"].(map[string]any)
	if v, _ := cdet["reasoning_tokens"].(float64); v != 5 {
		t.Errorf("reasoning_tokens = %v, want 5", cdet["reasoning_tokens"])
	}
}

// TestOpenAIStreamingIncludeUsageOptOut verifies that FinalUsageFrame stays
// silent when the client didn't opt in via stream_options.include_usage.
func TestOpenAIStreamingIncludeUsageOptOut(t *testing.T) {
	w := NewOpenAIStreamWriter("composer-2.5")
	_ = w.Encode(&Event{Kind: EventTextDelta, Text: "hi"})
	_ = w.Encode(&Event{Kind: EventTurnEnded, Usage: &Usage{InputTokens: 10, OutputTokens: 5, CacheReadTokens: 3}})
	if got := w.FinalUsageFrame(); len(got) != 0 {
		t.Errorf("FinalUsageFrame with IncludeUsage=false should be empty; got %q", got)
	}
}

// TestAnthropicNonStreamingCacheUsage covers BuildAnthropicUsage: input_tokens
// is total input minus cache_read (never <0); cache_read_input_tokens and
// cache_creation_input_tokens both appear in the object.
func TestAnthropicNonStreamingCacheUsage(t *testing.T) {
	u := &Usage{InputTokens: 100, OutputTokens: 50, CacheReadTokens: 60, CacheWriteTokens: 0}
	got := BuildAnthropicUsage(u)
	if v, _ := got["input_tokens"].(int64); v != 40 {
		t.Errorf("input_tokens = %v, want 40 (100-60)", got["input_tokens"])
	}
	if v, _ := got["output_tokens"].(int64); v != 50 {
		t.Errorf("output_tokens = %v, want 50", got["output_tokens"])
	}
	if v, _ := got["cache_read_input_tokens"].(int64); v != 60 {
		t.Errorf("cache_read_input_tokens = %v, want 60", got["cache_read_input_tokens"])
	}
	if v, _ := got["cache_creation_input_tokens"].(int64); v != 0 {
		t.Errorf("cache_creation_input_tokens = %v, want 0", got["cache_creation_input_tokens"])
	}

	// Boundary: cache read greater than total input clamps to 0.
	got = BuildAnthropicUsage(&Usage{InputTokens: 10, OutputTokens: 3, CacheReadTokens: 999})
	if v, _ := got["input_tokens"].(int64); v != 0 {
		t.Errorf("clamp: input_tokens = %v, want 0", got["input_tokens"])
	}
}

func TestAnthropicUsageExcludesCacheCreationFromInput(t *testing.T) {
	got := BuildAnthropicUsage(&Usage{
		InputTokens:      100,
		OutputTokens:     20,
		CacheReadTokens:  30,
		CacheWriteTokens: 50,
	})
	if input, _ := got["input_tokens"].(int64); input != 20 {
		t.Fatalf("input_tokens = %d, want 20 after cache read/write subtraction", input)
	}
	if read, _ := got["cache_read_input_tokens"].(int64); read != 30 {
		t.Fatalf("cache_read_input_tokens = %d, want 30", read)
	}
	if write, _ := got["cache_creation_input_tokens"].(int64); write != 50 {
		t.Fatalf("cache_creation_input_tokens = %d, want 50", write)
	}
}

// TestAnthropicStreamingMessageDeltaCache verifies the message_delta event
// on the streaming Anthropic path carries cache_read_input_tokens and
// cache_creation_input_tokens alongside input/output.
func TestAnthropicStreamingMessageDeltaCache(t *testing.T) {
	w := NewAnthropicStreamWriter("claude-opus-4")
	_ = w.Encode(&Event{Kind: EventTextDelta, Text: "hi"})
	end := string(w.Encode(&Event{Kind: EventTurnEnded, Usage: &Usage{
		InputTokens:      100,
		OutputTokens:     50,
		CacheReadTokens:  60,
		CacheWriteTokens: 0,
	}}))
	if !strings.Contains(end, `"cache_read_input_tokens":60`) {
		t.Errorf("missing cache_read_input_tokens=60:\n%s", end)
	}
	if !strings.Contains(end, `"cache_creation_input_tokens":0`) {
		t.Errorf("missing cache_creation_input_tokens=0:\n%s", end)
	}
	// input_tokens should be 40 (100 total minus 60 cached).
	if !strings.Contains(end, `"input_tokens":40`) {
		t.Errorf("missing input_tokens=40 after cache subtract:\n%s", end)
	}
	if !strings.Contains(end, `"output_tokens":50`) {
		t.Errorf("missing output_tokens=50:\n%s", end)
	}
}
