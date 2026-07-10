package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/cursor-proto/executor"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// fakeChatRunner is a chatRunner stub that returns a scripted event stream so
// tests can exercise the handler without a live Cursor session.
type fakeChatRunner struct {
	lastReq *executor.ChatRequest
	events  []executor.ChatEvent
	err     error
}

func (f *fakeChatRunner) RunChat(ctx context.Context, req *executor.ChatRequest) (<-chan executor.ChatEvent, error) {
	f.lastReq = req
	if f.err != nil {
		return nil, f.err
	}
	ch := make(chan executor.ChatEvent, len(f.events))
	for _, ev := range f.events {
		ch <- ev
	}
	close(ch)
	return ch, nil
}

// assistantBlobEvent constructs a ChatEvent that decodes into a
// BlobEvent{AssistantText: <text>} — this is how Cursor delivers final
// assistant text in practice. The handler consumes the KV path and
// suffix-diffs against previous blobs, so tests use the KV form for realism.
func assistantBlobEvent(text string) executor.ChatEvent {
	blob := `{"role":"assistant","content":[{"type":"text","text":` + encodeJSONString(text) + `}]}`
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_KvServerMessage{
			KvServerMessage: &cursorpb.AgentV1_KvServerMessage{
				Message: &cursorpb.AgentV1_KvServerMessage_SetBlobArgs{
					SetBlobArgs: &cursorpb.AgentV1_SetBlobArgs{
						BlobData: []byte(blob),
					},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func encodeJSONString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// turnEndedEvent constructs a ChatEvent that decodes into an EventTurnEnded.
func turnEndedEvent(inputTokens, outputTokens, cacheReadTokens int64) executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_TurnEnded{
					TurnEnded: &cursorpb.AgentV1_TurnEndedUpdate{
						InputTokens:     &inputTokens,
						OutputTokens:    &outputTokens,
						CacheReadTokens: &cacheReadTokens,
					},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func TestResponsesHandler_NonStreaming(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{
		// Cursor emits KV blobs with the full accumulated assistant text;
		// the handler suffix-diffs against previous blobs to produce deltas.
		assistantBlobEvent("Hello"),
		assistantBlobEvent("Hello, world"),
		turnEndedEvent(10, 3, 0),
	}}

	body := `{"model":"composer-2.5","input":"say hi","stream":false}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	responsesHandler(fake, nil)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\nbody=%s", err, rec.Body.String())
	}
	if resp["object"].(string) != "response" || resp["status"].(string) != "completed" {
		t.Fatalf("wrong shape: %+v", resp)
	}
	out := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len = %d, want 1", len(out))
	}
	msg := out[0].(map[string]any)
	if got := msg["content"].([]any)[0].(map[string]any)["text"].(string); got != "Hello, world" {
		t.Fatalf("assistant text = %q, want %q", got, "Hello, world")
	}

	// Executor was called with model + userText from the string input, no history.
	if fake.lastReq.Model != "composer-2.5" {
		t.Fatalf("model = %q, want composer-2.5", fake.lastReq.Model)
	}
	if fake.lastReq.UserMessage != "say hi" {
		t.Fatalf("UserMessage = %q, want %q", fake.lastReq.UserMessage, "say hi")
	}
	if len(fake.lastReq.History) != 0 {
		t.Fatalf("History should be empty, got %+v", fake.lastReq.History)
	}
}

func TestResponsesHandler_StreamingEventOrder(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{
		assistantBlobEvent("Hi"),
		turnEndedEvent(5, 1, 0),
	}}

	body := `{"model":"m","input":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	responsesHandler(fake, nil)(rec, req)

	if ct := rec.Header().Get("content-type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	raw, _ := io.ReadAll(rec.Body)
	// Ordered wire events must include response.created ... response.completed.
	wantSubstrings := []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.content_part.added",
		"event: response.output_text.delta",
		"event: response.output_text.done",
		"event: response.content_part.done",
		"event: response.output_item.done",
		"event: response.completed",
	}
	prev := 0
	for _, want := range wantSubstrings {
		idx := bytes.Index(raw[prev:], []byte(want))
		if idx < 0 {
			t.Fatalf("missing or out-of-order frame %q in stream:\n%s", want, string(raw))
		}
		prev += idx + len(want)
	}
}

func TestResponsesHandler_InstructionsBecomeSystemPrompt(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{turnEndedEvent(0, 0, 0)}}

	body := `{"model":"m","instructions":"be concise","input":"hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	responsesHandler(fake, nil)(rec, req)

	if fake.lastReq == nil {
		t.Fatal("RunChat was not called")
	}
	if fake.lastReq.SystemPrompt != "be concise" {
		t.Fatalf("SystemPrompt = %q, want %q", fake.lastReq.SystemPrompt, "be concise")
	}
	if fake.lastReq.UserMessage != "hi" {
		t.Fatalf("UserMessage = %q, want %q", fake.lastReq.UserMessage, "hi")
	}
}

func TestResponsesHandler_ArrayInputHistorySplit(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{turnEndedEvent(0, 0, 0)}}

	body := `{
		"model": "m",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"first"}]},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"reply"}]},
			{"type":"message","role":"user","content":"latest"}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	responsesHandler(fake, nil)(rec, req)

	if fake.lastReq.UserMessage != "latest" {
		t.Fatalf("UserMessage = %q, want %q", fake.lastReq.UserMessage, "latest")
	}
	if len(fake.lastReq.History) != 2 {
		t.Fatalf("History len = %d, want 2 (%+v)", len(fake.lastReq.History), fake.lastReq.History)
	}
	if fake.lastReq.History[0].Role != "user" || fake.lastReq.History[0].Content != "first" {
		t.Fatalf("History[0] = %+v", fake.lastReq.History[0])
	}
	if fake.lastReq.History[1].Role != "assistant" || fake.lastReq.History[1].Content != "reply" {
		t.Fatalf("History[1] = %+v", fake.lastReq.History[1])
	}
}

func TestResponsesHandler_ToolsFlatSchema(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{turnEndedEvent(0, 0, 0)}}

	body := `{
		"model": "m",
		"input": "hi",
		"tools": [
			{"type":"function","name":"get_time","description":"Return time","parameters":{"type":"object"}}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	responsesHandler(fake, nil)(rec, req)

	if fake.lastReq == nil || len(fake.lastReq.Tools) != 1 {
		t.Fatalf("expected 1 tool, got %+v", fake.lastReq)
	}
	if fake.lastReq.Tools[0].Name != "get_time" {
		t.Fatalf("tool name = %q, want get_time", fake.lastReq.Tools[0].Name)
	}
	if !fake.lastReq.AutoStopOnToolCall {
		t.Fatalf("AutoStopOnToolCall should be true when tools are provided")
	}
}

func TestResponsesHandler_EmptyInput400(t *testing.T) {
	fake := &fakeChatRunner{}
	body := `{"model":"m","input":""}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	rec := httptest.NewRecorder()

	responsesHandler(fake, nil)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}
