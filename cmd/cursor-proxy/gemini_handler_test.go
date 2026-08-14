package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/cursor-proto/executor"
)

func TestGeminiRouter_NonStreaming(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{
		assistantBlobEvent("Hello"),
		assistantBlobEvent("Hello, world"),
		turnEndedEvent(8, 3, 0),
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1beta/models/{tail}", geminiRouter(fake, nil))

	body := `{
		"contents": [
			{"role":"user","parts":[{"text":"say hi"}]}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-flash:generateContent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("content-type"); got != "application/json" {
		t.Fatalf("content-type = %q, want application/json", got)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	cand := got["candidates"].([]any)[0].(map[string]any)
	parts := cand["content"].(map[string]any)["parts"].([]any)
	if text := parts[0].(map[string]any)["text"].(string); text != "Hello, world" {
		t.Fatalf("text = %q, want %q", text, "Hello, world")
	}
	if cand["finishReason"].(string) != "STOP" {
		t.Fatalf("finishReason = %v, want STOP", cand["finishReason"])
	}
	usage := got["usageMetadata"].(map[string]any)
	if int(usage["promptTokenCount"].(float64)) != 8 {
		t.Fatalf("promptTokenCount = %v, want 8", usage["promptTokenCount"])
	}

	// Executor received the model with the models/ prefix stripped.
	if fake.lastReq.Model != "gemini-1.5-flash" {
		t.Fatalf("Model = %q, want gemini-1.5-flash", fake.lastReq.Model)
	}
	if fake.lastReq.UserMessage != "say hi" {
		t.Fatalf("UserMessage = %q, want say hi", fake.lastReq.UserMessage)
	}
}

func TestGeminiRouter_Streaming(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{
		assistantBlobEvent("Hi"),
		turnEndedEvent(4, 1, 0),
	}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1beta/models/{tail}", geminiRouter(fake, nil))

	body := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-1.5-flash:streamGenerateContent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if ct := rec.Header().Get("content-type"); ct != "text/event-stream" {
		t.Fatalf("content-type = %q, want text/event-stream", ct)
	}

	raw, _ := io.ReadAll(rec.Body)
	// Expect a text-part frame then a STOP-with-usage frame.
	blocks := bytes.Split(bytes.TrimSpace(raw), []byte("\n\n"))
	if len(blocks) < 2 {
		t.Fatalf("expected >=2 frames, got %d\n%s", len(blocks), string(raw))
	}
	// Terminal frame has finishReason=STOP.
	last := decodeGeminiFrameOrFail(t, blocks[len(blocks)-1])
	cand := last["candidates"].([]any)[0].(map[string]any)
	if cand["finishReason"].(string) != "STOP" {
		t.Fatalf("terminal finishReason = %v, want STOP", cand["finishReason"])
	}
	usage := last["usageMetadata"].(map[string]any)
	if int(usage["totalTokenCount"].(float64)) != 5 {
		t.Fatalf("totalTokenCount = %v, want 5", usage["totalTokenCount"])
	}
}

func TestGeminiRouter_ModelPrefixAccepted(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{turnEndedEvent(0, 0, 0)}}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1beta/models/{tail}", geminiRouter(fake, nil))

	body := `{"contents":[{"role":"user","parts":[{"text":"x"}]}]}`
	// Client sends models/foo; we don't nest that because the ServeMux
	// pattern already captured `foo:generateContent`, but this test verifies
	// StripGeminiModelPrefix is applied on paths like `models%2Ffoo` when
	// callers escape the slash. The Gemini SDK does not do this in practice,
	// but the helper should tolerate it.
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/models%2Fgemini-1.5-pro:generateContent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	// Either 200 or 400 with no user content — the important check is that
	// the model passed to the executor has no `models/` prefix.
	if fake.lastReq != nil && strings.HasPrefix(fake.lastReq.Model, "models/") {
		t.Fatalf("executor received Model=%q with unstripped models/ prefix", fake.lastReq.Model)
	}
	_ = rec
}

func TestGeminiRouter_UnknownMethod(t *testing.T) {
	fake := &fakeChatRunner{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1beta/models/{tail}", geminiRouter(fake, nil))

	body := `{"contents":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini:countTokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestGeminiRouter_SystemInstruction(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{turnEndedEvent(0, 0, 0)}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1beta/models/{tail}", geminiRouter(fake, nil))

	body := `{
		"systemInstruction": {"parts":[{"text":"Be concise."}]},
		"contents":[{"role":"user","parts":[{"text":"Hi"}]}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if fake.lastReq.SystemPrompt != "Be concise." {
		t.Fatalf("SystemPrompt = %q, want %q", fake.lastReq.SystemPrompt, "Be concise.")
	}
}

func TestGeminiRouter_MultiTurnHistory(t *testing.T) {
	fake := &fakeChatRunner{events: []executor.ChatEvent{turnEndedEvent(0, 0, 0)}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1beta/models/{tail}", geminiRouter(fake, nil))

	body := `{
		"contents":[
			{"role":"user","parts":[{"text":"first"}]},
			{"role":"model","parts":[{"text":"reply"}]},
			{"role":"user","parts":[{"text":"latest"}]}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if fake.lastReq.UserMessage != "latest" {
		t.Fatalf("UserMessage = %q, want latest", fake.lastReq.UserMessage)
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

func TestGeminiRouter_ToolsFunctionDeclarations(t *testing.T) {
	observedTools = newToolRing(4096)
	fake := &fakeChatRunner{events: []executor.ChatEvent{turnEndedEvent(0, 0, 0)}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1beta/models/{tail}", geminiRouter(fake, nil))

	body := `{
		"contents":[{"role":"user","parts":[{"text":"x"}]}],
		"tools":[{
			"functionDeclarations":[
				{"name":"get_time","description":"time","parameters":{"type":"object"}},
				{"description":"missing name gets dropped"}
			]
		}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(body))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if len(fake.lastReq.Tools) != 1 {
		t.Fatalf("Tools len = %d, want 1 (empty name should be dropped): %+v", len(fake.lastReq.Tools), fake.lastReq.Tools)
	}
	if fake.lastReq.Tools[0].Name != "get_time" {
		t.Fatalf("Tools[0].Name = %q, want get_time", fake.lastReq.Tools[0].Name)
	}
	obs := observedTools.snapshotSince(time.Now().Add(-time.Minute))
	if len(obs) != 1 || obs[0].name != "get_time" {
		t.Fatalf("observed tools = %+v, want get_time", obs)
	}
}

func TestGeminiModelsListHandler(t *testing.T) {
	lister := &fakeModelLister{names: []string{"composer-2.5", "gemini-1.5-flash"}}
	req := httptest.NewRequest(http.MethodGet, "/v1beta/models", nil)
	rec := httptest.NewRecorder()

	geminiModelsListHandler(lister)(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	models := got["models"].([]any)
	if len(models) != 2 {
		t.Fatalf("models len = %d, want 2", len(models))
	}
	first := models[0].(map[string]any)
	if first["name"].(string) != "models/composer-2.5" {
		t.Fatalf("models[0].name = %v", first["name"])
	}
	methods := first["supportedGenerationMethods"].([]any)
	if len(methods) < 2 || methods[0].(string) != "generateContent" {
		t.Fatalf("supportedGenerationMethods = %v", methods)
	}
}

// decodeGeminiFrameOrFail is the handler-test twin of the translator
// package's helper. It decodes one SSE frame block into a map or fails
// the test.
func decodeGeminiFrameOrFail(t *testing.T, block []byte) map[string]any {
	t.Helper()
	s := strings.TrimSpace(string(block))
	if !strings.HasPrefix(s, "data: ") {
		t.Fatalf("frame missing data: prefix: %q", s)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(strings.TrimPrefix(s, "data: ")), &got); err != nil {
		t.Fatalf("decode: %v (frame=%q)", err, s)
	}
	return got
}
