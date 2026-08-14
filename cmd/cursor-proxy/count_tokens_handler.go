// count_tokens_handler.go implements `POST /v1/messages/count_tokens` —
// Anthropic's token-counting endpoint.
//
// IMPORTANT: Cursor's backend does not expose a real tokenizer, so this
// endpoint returns an ESTIMATE. We flatten system + history + last user
// turn into plain text and apply a byte-per-token heuristic (≈ runes / 3.5,
// rounded up, with a floor of 1). This matches Anthropic's response shape
// exactly (`{"input_tokens": N}`), just with an approximate value.
//
// Clients that treat count_tokens as gospel (rate-limiting, quota
// enforcement) will over- or under-count by ~10% versus Anthropic's real
// tokenizer. The estimate is stable and monotonic in input length, which is
// what matters for progress bars and pre-flight sanity checks — the two
// use cases we've seen in the wild.
package main

import (
	"encoding/json"
	"math"
	"net/http"
	"strings"
	"unicode/utf8"
)

// tokensPerRune is the divisor used to estimate token count from Unicode
// rune count. 3.5 is close to Anthropic's tokenizer for English + code and
// slightly high for CJK — a conservative estimate is preferable here since
// callers use this for quota checks.
const tokensPerRune = 3.5

// estimateTokens returns an integer token estimate for `text`. Floor of 1
// so callers can distinguish empty-input from any-input, matching what
// Anthropic's tokenizer does.
func estimateTokens(text string) int {
	if text == "" {
		return 0
	}
	runes := utf8.RuneCountInString(text)
	est := int(math.Ceil(float64(runes) / tokensPerRune))
	if est < 1 {
		est = 1
	}
	return est
}

// countTokensHandler serves POST /v1/messages/count_tokens. It reuses the
// existing anthropicMessagesRequest schema for input parsing so the wire
// contract stays aligned with /v1/messages.
func countTokensHandler(w http.ResponseWriter, r *http.Request) {
	var req anthropicMessagesRequest
	if err := decodeJSONRequest(w, r, &req, false); err != nil {
		http.Error(w, err.Error(), jsonRequestErrorStatus(err))
		return
	}

	var b strings.Builder
	if sys := strings.TrimSpace(flattenAnthropicSystem(req.System)); sys != "" {
		b.WriteString(sys)
		b.WriteString("\n")
	}
	for _, m := range req.Messages {
		text := flattenAnthropicContent(m.Content)
		if text == "" {
			continue
		}
		b.WriteString(m.Role)
		b.WriteString(": ")
		b.WriteString(text)
		b.WriteString("\n")
	}

	resp := map[string]any{"input_tokens": estimateTokens(b.String())}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}
