package kernel

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/cursor-proto/executor"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// installFakes wires the runnerFactory and hostCallInvoker to the
// caller-supplied fakes and returns a cleanup func. It is intended
// for use inside a subtest as `defer cleanup()`.
func installFakes(t *testing.T, factory func(string, []byte) (chatRunner, string, error), invoker func(string, []byte) ([]byte, error)) func() {
	t.Helper()
	prevRunner := runnerFactory
	prevInvoker := hostCallInvoker
	runnerFactory = factory
	hostCallInvoker = invoker
	return func() {
		runnerFactory = prevRunner
		hostCallInvoker = prevInvoker
	}
}

// fakeRunner emits a scripted list of events on RunChat. Each event
// carries a pre-baked AgentServerMessage so translator.FromServerMessage
// picks up something meaningful.
type fakeRunner struct {
	events []executor.ChatEvent
	err    error
}

func (f *fakeRunner) RunChat(ctx context.Context, req *executor.ChatRequest) (<-chan executor.ChatEvent, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(chan executor.ChatEvent, len(f.events))
	go func() {
		defer close(out)
		for _, ev := range f.events {
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}()
	return out, nil
}

// buildTextDeltaEvent produces a ChatEvent that FromServerMessage
// interprets as an EventTextDelta with the given text.
func buildTextDeltaEvent(text string) executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_TextDelta{
					TextDelta: &cursorpb.AgentV1_TextDeltaUpdate{Text: text},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

// buildTurnEndedEvent produces a ChatEvent that FromServerMessage
// interprets as an EventTurnEnded with the given input/output totals.
func buildTurnEndedEvent(inTok, outTok int64) executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_TurnEnded{
					TurnEnded: &cursorpb.AgentV1_TurnEndedUpdate{
						InputTokens:  &inTok,
						OutputTokens: &outTok,
					},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func buildAssistantBlobEvent(text string) executor.ChatEvent {
	blob, _ := json.Marshal(map[string]any{
		"role": "assistant",
		"content": []map[string]any{{
			"type": "text",
			"text": text,
		}},
	})
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_KvServerMessage{
			KvServerMessage: &cursorpb.AgentV1_KvServerMessage{
				Message: &cursorpb.AgentV1_KvServerMessage_SetBlobArgs{
					SetBlobArgs: &cursorpb.AgentV1_SetBlobArgs{BlobData: blob},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func buildSignedAssistantBlobEvent(thinking, signature, text string) executor.ChatEvent {
	blob, _ := json.Marshal(map[string]any{
		"role": "assistant",
		"content": []map[string]any{
			{"type": "reasoning", "text": thinking, "signature": signature},
			{"type": "text", "text": text},
		},
	})
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_KvServerMessage{
			KvServerMessage: &cursorpb.AgentV1_KvServerMessage{
				Message: &cursorpb.AgentV1_KvServerMessage_SetBlobArgs{
					SetBlobArgs: &cursorpb.AgentV1_SetBlobArgs{BlobData: blob},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func buildHeartbeatEvent() executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_Heartbeat{
					Heartbeat: &cursorpb.AgentV1_HeartbeatUpdate{},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func buildThinkingDeltaEvent(text string) executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ThinkingDelta{
					ThinkingDelta: &cursorpb.AgentV1_ThinkingDeltaUpdate{Text: text},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

// buildFakeExecutorRequest hand-marshals the executorRequest JSON with
// the given payload and format. StorageJSON is a fake but well-formed
// AuthFile so the executor code that touches it does not panic; tests
// swap the runnerFactory so no real Cursor client is built.
func buildFakeExecutorRequest(t *testing.T, format string, payload []byte, stream bool, streamID string) []byte {
	t.Helper()
	req := executorRequest{
		AuthID:       "fake-auth",
		AuthProvider: "cursor",
		Model:        "composer-2.5",
		Format:       format,
		Stream:       stream,
		Payload:      payload,
		StorageJSON:  []byte(`{"type":"cursor","access_token":"AT","email":"fake@example.com"}`),
		StreamID:     streamID,
	}
	buf, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return buf
}

// TestExecute_OpenAI_NonStreaming exercises handleExecutorExecute end
// to end with a scripted RunChat and asserts the OpenAI response body
// carries the assistant text and a stop finish_reason.
func TestExecute_OpenAI_NonStreaming(t *testing.T) {
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("Hello, "),
			buildTextDeltaEvent("world."),
			buildTurnEndedEvent(11, 3),
		},
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) {
			return runner, "unit@example.com", nil
		},
		nil,
	)()

	payload := []byte(`{
        "model": "composer-2.5",
        "messages": [{"role": "user", "content": "hi"}]
    }`)
	raw, rc := dispatch("executor.execute", buildFakeExecutorRequest(t, "openai", payload, false, ""))
	if rc != 0 {
		t.Fatalf("rc = %d, want 0. envelope=%s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var resp executorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !strings.Contains(string(resp.Payload), `"content":"Hello, world."`) {
		t.Errorf("payload missing assistant text: %s", string(resp.Payload))
	}
	if !strings.Contains(string(resp.Payload), `"finish_reason":"stop"`) {
		t.Errorf("payload missing stop reason: %s", string(resp.Payload))
	}
	if !strings.Contains(string(resp.Payload), `"prompt_tokens":11`) {
		t.Errorf("payload missing usage: %s", string(resp.Payload))
	}
}

// TestExecute_Claude_NonStreaming exercises the same path for Claude
// output.
func TestExecute_Claude_NonStreaming(t *testing.T) {
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("Bonjour"),
			buildTurnEndedEvent(7, 2),
		},
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) {
			return runner, "", nil
		},
		nil,
	)()

	payload := []byte(`{
        "model": "claude-4.5-sonnet",
        "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
    }`)
	raw, rc := dispatch("executor.execute", buildFakeExecutorRequest(t, "claude", payload, false, ""))
	if rc != 0 {
		t.Fatalf("rc = %d, want 0. envelope=%s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var resp executorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !strings.Contains(string(resp.Payload), `"text":"Bonjour"`) {
		t.Errorf("payload missing text block: %s", string(resp.Payload))
	}
	if !strings.Contains(string(resp.Payload), `"stop_reason":"end_turn"`) {
		t.Errorf("payload missing end_turn: %s", string(resp.Payload))
	}
}

// TestExecuteStream_OpenAI captures the chunks the plugin emits and
// asserts an SSE-shaped sequence ending in [DONE].
func TestExecuteStream_OpenAI(t *testing.T) {
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("Hi"),
			buildTurnEndedEvent(4, 1),
		},
	}

	var (
		mu       sync.Mutex
		emitted  [][]byte
		closed   bool
		closeErr string
		done     = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				StreamID string `json:"stream_id"`
				Payload  []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			var req struct {
				StreamID string `json:"stream_id"`
				Error    string `json:"error"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("close unmarshal: %v", err)
			}
			mu.Lock()
			closed = true
			closeErr = req.Error
			mu.Unlock()
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{
        "model": "composer-2.5",
        "messages": [{"role": "user", "content": "hi"}],
        "stream": true,
        "stream_options": {"include_usage": true}
    }`)
	raw, rc := dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "openai", payload, true, "s-1"))
	if rc != 0 {
		t.Fatalf("rc = %d, want 0: %s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	// Wait for the goroutine to signal close.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream never closed within 2s")
	}
	mu.Lock()
	defer mu.Unlock()
	if !closed {
		t.Fatalf("stream never closed. emitted=%d", len(emitted))
	}
	if closeErr != "" {
		t.Errorf("close carried error: %q", closeErr)
	}
	if len(emitted) < 2 {
		t.Fatalf("emitted too few chunks: %d", len(emitted))
	}
	last := string(emitted[len(emitted)-1])
	if !strings.Contains(last, "data: [DONE]") {
		t.Errorf("final chunk not [DONE]: %q", last)
	}
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	if !strings.Contains(joined, `"content":"Hi"`) {
		t.Errorf("stream missing content: %s", joined)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Errorf("stream missing finish_reason=stop: %s", joined)
	}
	if !strings.Contains(joined, `"prompt_tokens":4`) {
		t.Errorf("stream missing usage frame: %s", joined)
	}
}

func TestExecuteStream_OpenAI_DeduplicatesFinalAssistantBlob(t *testing.T) {
	runner := &fakeRunner{events: []executor.ChatEvent{
		buildTextDeltaEvent("Hi"),
		buildAssistantBlobEvent("Hi"),
		buildTurnEndedEvent(4, 1),
	}}

	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "openai", payload, true, "dedupe-openai"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if got := strings.Count(joined, `"content":"Hi"`); got != 1 {
		t.Fatalf("assistant text emitted %d times, want once: %s", got, joined)
	}
}

func TestExecuteStream_Claude_ForwardsHeartbeatAndDeduplicatesFinalBlob(t *testing.T) {
	runner := &fakeRunner{events: []executor.ChatEvent{
		buildHeartbeatEvent(),
		buildThinkingDeltaEvent("reasoning"),
		buildTextDeltaEvent("Hi"),
		buildAssistantBlobEvent("Hi"),
		buildTurnEndedEvent(4, 1),
	}}

	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "dedupe-claude"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if !strings.Contains(joined, `event: ping`) || !strings.Contains(joined, `{"type":"ping"}`) {
		t.Fatalf("heartbeat was not forwarded as an Anthropic ping: %s", joined)
	}
	if !strings.Contains(joined, `"type":"thinking_delta"`) || !strings.Contains(joined, `"thinking":"reasoning"`) {
		t.Fatalf("thinking delta was not forwarded as Anthropic thinking: %s", joined)
	}
	if got := strings.Count(joined, `"text":"Hi"`); got != 1 {
		t.Fatalf("assistant text emitted %d times, want once: %s", got, joined)
	}
}

func TestExecuteStream_Claude_OrdersRealSignatureBeforeBufferedText(t *testing.T) {
	runner := &fakeRunner{events: []executor.ChatEvent{
		buildThinkingDeltaEvent("reasoning"),
		buildTextDeltaEvent("answer"),
		buildSignedAssistantBlobEvent("reasoning", "c2lnbmF0dXJl", "answer"),
		buildTurnEndedEvent(4, 2),
	}}

	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-4-8-medium","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "signed-thinking"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	thinkingAt := strings.Index(joined, `"type":"thinking_delta"`)
	signatureAt := strings.Index(joined, `"type":"signature_delta"`)
	textAt := strings.Index(joined, `"type":"text_delta"`)
	if thinkingAt < 0 || signatureAt < 0 || textAt < 0 || !(thinkingAt < signatureAt && signatureAt < textAt) {
		t.Fatalf("invalid thinking/signature/text order: %s", joined)
	}
	if strings.Count(joined, `"text":"answer"`) != 1 {
		t.Fatalf("buffered answer was duplicated: %s", joined)
	}
}

func byteSlicesToStrings(chunks [][]byte) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, string(c))
	}
	return out
}

// TestCountTokens exercises the local heuristic and asserts the
// returned envelope wraps a `total_tokens` field.
func TestCountTokens(t *testing.T) {
	payload := []byte(`{
        "model": "composer-2.5",
        "messages": [
            {"role": "system", "content": "you are helpful"},
            {"role": "user", "content": "hello world"}
        ]
    }`)
	raw, rc := dispatch("executor.count_tokens", buildFakeExecutorRequest(t, "openai", payload, false, ""))
	if rc != 0 {
		t.Fatalf("rc = %d, want 0: %s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var resp executorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	total, ok := out["total_tokens"].(float64)
	if !ok || total <= 0 {
		t.Errorf("total_tokens missing or non-positive: %v", out["total_tokens"])
	}
}

// TestCountTokens_CJK exercises the CJK code path so we do not lose
// coverage of the isCJK branch.
func TestCountTokens_CJK(t *testing.T) {
	if got := countTokens("你好世界"); got == 0 {
		t.Errorf("countTokens(CJK) = 0, want > 0")
	}
	if got := countTokens("hello"); got == 0 {
		t.Errorf("countTokens(ASCII) = 0, want > 0")
	}
}

// TestParseOpenAIPayload_NoUser ensures the parser rejects payloads
// with no user message.
func TestParseOpenAIPayload_NoUser(t *testing.T) {
	_, err := parseOpenAIPayload([]byte(`{"model":"x","messages":[{"role":"system","content":"s"}]}`))
	if err == nil {
		t.Fatal("expected error for missing user message")
	}
}

func TestParseClaudePayload_PreservesToolUseAndToolResultHistory(t *testing.T) {
	payload := []byte(`{
		"model":"claude-opus-5",
		"messages":[
			{"role":"user","content":"Check Shanghai weather."},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{"city":"Shanghai"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"Sunny, 28 C."}]},
			{"role":"user","content":"Summarize the result."}
		]
	}`)

	shape, err := parseClaudePayload(payload)
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if len(shape.History) != 3 {
		t.Fatalf("history length = %d, want 3: %#v", len(shape.History), shape.History)
	}
	if !strings.Contains(shape.History[1].Content, `"type":"tool_use"`) ||
		!strings.Contains(shape.History[1].Content, `"name":"get_weather"`) ||
		!strings.Contains(shape.History[1].Content, `"city":"Shanghai"`) {
		t.Fatalf("assistant tool_use was not preserved: %q", shape.History[1].Content)
	}
	if !strings.Contains(shape.History[2].Content, `"type":"tool_result"`) ||
		!strings.Contains(shape.History[2].Content, `"tool_use_id":"toolu_123"`) ||
		!strings.Contains(shape.History[2].Content, `Sunny, 28 C.`) {
		t.Fatalf("user tool_result was not preserved: %q", shape.History[2].Content)
	}
}

// TestParseClaudePayload_ArrayContent covers the content-block path
// so we do not lose it on refactor.
func TestParseClaudePayload_ArrayContent(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
        "model": "claude",
        "system": [{"type":"text","text":"sys1"},{"type":"text","text":"sys2"}],
        "messages": [{"role":"user","content":[{"type":"text","text":"hi"}]}]
    }`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if shape.SystemPrompt != "sys1\nsys2" {
		t.Errorf("system = %q", shape.SystemPrompt)
	}
	if shape.UserMessage != "hi" {
		t.Errorf("user = %q", shape.UserMessage)
	}
}

func TestParseClaudePayload_ResolvesThinkingEffortAndStructuredOutput(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
		"model":"claude-opus-4-8-medium",
		"thinking":{"type":"adaptive","display":"summarized"},
		"output_config":{"effort":"xhigh","format":{"type":"json_schema","schema":{"type":"object","properties":{"result":{"type":"integer"}},"required":["result"]}}},
		"messages":[{"role":"user","content":"calculate"}]
	}`))
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	req := buildChatRequest(shape, nil)
	if req.Model != "claude-opus-4-8-thinking-xhigh" {
		t.Fatalf("resolved model = %q", req.Model)
	}
	if req.Mode != executor.APIConversationMode(false) {
		t.Fatalf("mode = %d, want API ask mode", req.Mode)
	}
	if !strings.Contains(req.SystemPrompt, "No tools are available") {
		t.Fatalf("no-tool API guard missing from prompt: %q", req.SystemPrompt)
	}
	if !strings.Contains(req.SystemPrompt, `"required":["result"]`) {
		t.Fatalf("structured output schema missing from prompt: %q", req.SystemPrompt)
	}
}
