package translator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestGeminiStreamWriter_TextThenTurnEnded(t *testing.T) {
	w := NewGeminiStreamWriter("gemini-1.5-flash")
	var buf bytes.Buffer
	buf.Write(w.Encode(&Event{Kind: EventTextDelta, Text: "Hello, "}))
	buf.Write(w.Encode(&Event{Kind: EventTextDelta, Text: "world"}))
	buf.Write(w.Encode(&Event{Kind: EventTurnEnded, Usage: &Usage{
		InputTokens: 10, OutputTokens: 3, CacheReadTokens: 2,
	}}))

	blocks := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n\n"))
	if len(blocks) != 3 {
		t.Fatalf("expected 3 frames, got %d\n%s", len(blocks), buf.String())
	}

	// Frame 0: first text delta.
	f0 := decodeGeminiFrame(t, blocks[0])
	first := f0["candidates"].([]any)[0].(map[string]any)
	parts := first["content"].(map[string]any)["parts"].([]any)
	if got := parts[0].(map[string]any)["text"].(string); got != "Hello, " {
		t.Fatalf("frame[0] text = %q, want %q", got, "Hello, ")
	}
	// Frame 2: terminal with finishReason=STOP and usageMetadata.
	f2 := decodeGeminiFrame(t, blocks[2])
	cand := f2["candidates"].([]any)[0].(map[string]any)
	if cand["finishReason"].(string) != "STOP" {
		t.Fatalf("finishReason = %v, want STOP", cand["finishReason"])
	}
	usage := f2["usageMetadata"].(map[string]any)
	if int(usage["promptTokenCount"].(float64)) != 10 {
		t.Fatalf("promptTokenCount = %v, want 10", usage["promptTokenCount"])
	}
	if int(usage["candidatesTokenCount"].(float64)) != 3 {
		t.Fatalf("candidatesTokenCount = %v, want 3", usage["candidatesTokenCount"])
	}
	if int(usage["totalTokenCount"].(float64)) != 13 {
		t.Fatalf("totalTokenCount = %v, want 13", usage["totalTokenCount"])
	}
	if int(usage["cachedContentTokenCount"].(float64)) != 2 {
		t.Fatalf("cachedContentTokenCount = %v, want 2", usage["cachedContentTokenCount"])
	}
}

func TestGeminiStreamWriter_ToolCall(t *testing.T) {
	w := NewGeminiStreamWriter("gemini-1.5-flash")
	var buf bytes.Buffer
	buf.Write(w.Encode(&Event{
		Kind:          EventToolCallStarted,
		ToolCallID:    "call_1",
		ToolName:      "get_weather",
		ToolArgsDelta: `{"city":"SF"}`,
	}))
	buf.Write(w.Encode(&Event{Kind: EventTurnEnded}))

	blocks := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n\n"))
	if len(blocks) != 2 {
		t.Fatalf("expected 2 frames (functionCall + terminal), got %d\n%s", len(blocks), buf.String())
	}
	// First frame carries functionCall.
	f0 := decodeGeminiFrame(t, blocks[0])
	parts := f0["candidates"].([]any)[0].(map[string]any)["content"].(map[string]any)["parts"].([]any)
	fc := parts[0].(map[string]any)["functionCall"].(map[string]any)
	if fc["name"].(string) != "get_weather" {
		t.Fatalf("functionCall.name = %v, want get_weather", fc["name"])
	}
	args := fc["args"].(map[string]any)
	if args["city"].(string) != "SF" {
		t.Fatalf("functionCall.args = %+v, want city=SF", args)
	}
	// Second frame has STOP.
	f1 := decodeGeminiFrame(t, blocks[1])
	cand := f1["candidates"].([]any)[0].(map[string]any)
	if cand["finishReason"].(string) != "STOP" {
		t.Fatalf("finishReason = %v, want STOP", cand["finishReason"])
	}
}

func TestGeminiNonStreamingAccumulator(t *testing.T) {
	acc := &GeminiNonStreamingAccumulator{Model: "gemini-1.5-flash"}
	acc.Consume(&Event{Kind: EventTextDelta, Text: "Hi"})
	acc.Consume(&Event{Kind: EventTextDelta, Text: " there"})
	acc.Consume(&Event{Kind: EventTurnEnded, Usage: &Usage{InputTokens: 4, OutputTokens: 2}})

	body := acc.Response()
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, string(body))
	}
	cand := got["candidates"].([]any)[0].(map[string]any)
	parts := cand["content"].(map[string]any)["parts"].([]any)
	if got := parts[0].(map[string]any)["text"].(string); got != "Hi there" {
		t.Fatalf("text = %q, want %q", got, "Hi there")
	}
	if cand["finishReason"].(string) != "STOP" {
		t.Fatalf("finishReason = %v, want STOP", cand["finishReason"])
	}
	usage := got["usageMetadata"].(map[string]any)
	if int(usage["totalTokenCount"].(float64)) != 6 {
		t.Fatalf("totalTokenCount = %v, want 6", usage["totalTokenCount"])
	}
}

func TestBuildGeminiModelsList(t *testing.T) {
	got := BuildGeminiModelsList([]string{"composer-2.5", "claude-4.5-sonnet"})
	models := got["models"].([]map[string]any)
	if len(models) != 2 {
		t.Fatalf("len = %d, want 2", len(models))
	}
	if models[0]["name"].(string) != "models/composer-2.5" {
		t.Fatalf("models[0].name = %v", models[0]["name"])
	}
	if models[0]["baseModelId"].(string) != "composer-2.5" {
		t.Fatalf("models[0].baseModelId = %v", models[0]["baseModelId"])
	}
	methods := models[0]["supportedGenerationMethods"].([]string)
	if len(methods) != 2 || methods[0] != "generateContent" {
		t.Fatalf("methods = %v", methods)
	}
}

func TestStripGeminiModelPrefix(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"models/composer-2.5", "composer-2.5"},
		{"composer-2.5", "composer-2.5"},
		{"models/", ""},
	}
	for _, tc := range cases {
		if got := StripGeminiModelPrefix(tc.in); got != tc.want {
			t.Fatalf("StripGeminiModelPrefix(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// decodeGeminiFrame decodes one `data: {json}` block into a map.
func decodeGeminiFrame(t *testing.T, block []byte) map[string]any {
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
