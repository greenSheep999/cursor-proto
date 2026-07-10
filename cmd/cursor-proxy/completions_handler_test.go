package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/cursor-proto/executor"
)

func TestCompletionsHandler_NonStreaming(t *testing.T) {
	cases := []struct {
		name       string
		body       string
		wantPrompt string
	}{
		{
			name:       "string prompt",
			body:       `{"model":"m","prompt":"say hi","stream":false}`,
			wantPrompt: "say hi",
		},
		{
			name:       "array prompt joined",
			body:       `{"model":"m","prompt":["one","two"]}`,
			wantPrompt: "one\n\ntwo",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeChatRunner{events: []executor.ChatEvent{
				assistantBlobEvent("Hi there"),
				turnEndedEvent(3, 2, 0),
			}}
			req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			completionsHandler(fake, nil)(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
			}
			if fake.lastReq.UserMessage != tc.wantPrompt {
				t.Fatalf("UserMessage = %q, want %q", fake.lastReq.UserMessage, tc.wantPrompt)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v\n%s", err, rec.Body.String())
			}
			if got["object"].(string) != "text_completion" {
				t.Fatalf("object = %v, want text_completion", got["object"])
			}
			choices := got["choices"].([]any)
			ch := choices[0].(map[string]any)
			if ch["text"].(string) != "Hi there" {
				t.Fatalf("choices[0].text = %v, want %q", ch["text"], "Hi there")
			}
			if ch["finish_reason"].(string) != "stop" {
				t.Fatalf("finish_reason = %v, want stop", ch["finish_reason"])
			}
			usage := got["usage"].(map[string]any)
			if int(usage["prompt_tokens"].(float64)) != 3 {
				t.Fatalf("prompt_tokens = %v, want 3", usage["prompt_tokens"])
			}
		})
	}
}

func TestCompletionsHandler_Streaming(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{
		assistantBlobEvent("Hello"),
		assistantBlobEvent("Hello, world"),
		turnEndedEvent(3, 2, 0),
	}}

	body := `{"model":"m","prompt":"hi","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	completionsHandler(fake, nil)(rec, req)

	if ct := rec.Header().Get("content-type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	raw, _ := io.ReadAll(rec.Body)
	// Every non-terminator frame is a text_completion chunk.
	blocks := strings.Split(strings.TrimSpace(string(raw)), "\n\n")
	if len(blocks) < 3 {
		t.Fatalf("expected >=3 SSE frames, got %d\n%s", len(blocks), string(raw))
	}
	// Assemble the text stream by walking frames and checking each is text_completion.
	joined := ""
	sawFinish := false
	sawDone := false
	for _, blk := range blocks {
		if strings.TrimSpace(blk) == "data: [DONE]" {
			sawDone = true
			continue
		}
		payload := strings.TrimPrefix(blk, "data: ")
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			t.Fatalf("decode chunk %q: %v", blk, err)
		}
		if chunk["object"].(string) != "text_completion" {
			t.Fatalf("object = %v, want text_completion (chunk=%s)", chunk["object"], payload)
		}
		ch := chunk["choices"].([]any)[0].(map[string]any)
		if t2 := ch["text"].(string); t2 != "" {
			joined += t2
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr == "stop" {
			sawFinish = true
		}
	}
	if joined != "Hello, world" {
		t.Fatalf("assembled text = %q, want %q", joined, "Hello, world")
	}
	if !sawFinish {
		t.Fatalf("missing terminal finish_reason=stop frame\n%s", string(raw))
	}
	if !sawDone {
		t.Fatalf("missing data: [DONE] frame\n%s", string(raw))
	}
}

func TestCompletionsHandler_EmptyPrompt400(t *testing.T) {
	fake := &fakeChatRunner{}
	body := `{"model":"m","prompt":""}`
	req := httptest.NewRequest(http.MethodPost, "/v1/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	completionsHandler(fake, nil)(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}
