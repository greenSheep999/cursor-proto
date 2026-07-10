package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"empty", "", 0},
		{"single char", "a", 1},
		{"short english", "hello world", 4}, // 11 runes / 3.5 = 3.14 → 4
		{"multibyte", "こんにちは", 2},           // 5 runes / 3.5 = 1.43 → 2
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := estimateTokens(tc.in)
			if got != tc.want {
				t.Fatalf("estimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

func TestCountTokensHandler(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "string system + single user message",
			body: `{
				"model": "claude-4.5-sonnet",
				"system": "You are helpful.",
				"messages": [
					{"role":"user","content":"Hello"}
				]
			}`,
		},
		{
			name: "array system + array-of-blocks user message",
			body: `{
				"model": "m",
				"system": [{"type":"text","text":"Be concise."}],
				"messages": [
					{"role":"user","content":[{"type":"text","text":"Hi"}]}
				]
			}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(tc.body))
			rec := httptest.NewRecorder()

			countTokensHandler(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d (body=%s)", rec.Code, rec.Body.String())
			}
			if got := rec.Header().Get("content-type"); got != "application/json" {
				t.Fatalf("content-type = %q, want application/json", got)
			}
			var got map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v\n%s", err, rec.Body.String())
			}
			tokens, ok := got["input_tokens"].(float64)
			if !ok {
				t.Fatalf("input_tokens missing or wrong type: %+v", got)
			}
			if tokens < 1 {
				t.Fatalf("expected >=1 estimated token, got %v", tokens)
			}
		})
	}
}

func TestCountTokensHandler_BadJSON(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader("not json"))
	rec := httptest.NewRecorder()

	countTokensHandler(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestCountTokensHandler_Monotonic(t *testing.T) {
	small := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	big := `{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("hi ", 500) + `"}]}`

	smallResp := runCountTokens(t, small)
	bigResp := runCountTokens(t, big)

	if bigResp <= smallResp {
		t.Fatalf("estimate not monotonic: small=%d big=%d", smallResp, bigResp)
	}
}

func runCountTokens(t *testing.T, body string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	countTokensHandler(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return int(got["input_tokens"].(float64))
}
