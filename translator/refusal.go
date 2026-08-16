package translator

import (
	"regexp"
	"strings"
)

var refusalPrefixes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(?:sorry[,.!:\s]+|apologies[,.!:\s]+|i(?:'m| am) sorry[,.!:\s]+)*(?:i\s+)?(?:cannot|can't|won't|will not|am unable to|must decline|refuse)`),
	regexp.MustCompile(`(?i)^(?:i\s+)?(?:can't|cannot|won't)\s+(?:do|provide|share|reveal|output|disclose)\b`),
	regexp.MustCompile(`^(?:抱歉|对不起|很抱歉)?[，,:：。.!！?？\s]*(?:我)?(?:不能|无法|没办法|没法|不会|不可以|拒绝)`),
}

// IsRefusalText recognizes a direct refusal at the beginning of a response.
// Anthropic exposes refusals through stop_reason=refusal, while Cursor only
// returns text plus a generic turn end, so the compatibility layer must infer
// the equivalent protocol reason from the actual response semantics.
func IsRefusalText(text string) bool {
	normalized := strings.TrimSpace(strings.NewReplacer(
		"’", "'",
		"‘", "'",
		"“", `"`,
		"”", `"`,
	).Replace(text))
	if normalized == "" {
		return false
	}
	if len(normalized) > 240 {
		normalized = normalized[:240]
	}
	for _, pattern := range refusalPrefixes {
		if pattern.MatchString(normalized) {
			return true
		}
	}
	return false
}

// AnthropicStopReason maps a completed Cursor response to Anthropic's stop
// reason vocabulary without model-specific branches.
func AnthropicStopReason(text string, sawToolCall bool) string {
	if sawToolCall {
		return "tool_use"
	}
	if IsRefusalText(text) {
		return "refusal"
	}
	return "end_turn"
}
