package main

import (
	"testing"

	"github.com/router-for-me/cursor-proto/executor"
)

func TestPrefixForSimCacheIsolatesRequestDomains(t *testing.T) {
	oldEmail := wireAccountEmailString()
	t.Cleanup(func() { SetWireAccountEmail(oldEmail) })
	history := []executor.HistoryTurn{{Role: "user", Content: "hello"}}
	tools := []executor.ToolDefinition{{Name: "lookup", InputSchema: map[string]any{"type": "object"}}}

	SetWireAccountEmail("first@example.com")
	base := prefixForSimCache("openai-chat", "model-a", tools, "system", history)
	cases := []struct {
		name string
		got  string
	}{
		{"account", func() string {
			SetWireAccountEmail("second@example.com")
			return prefixForSimCache("openai-chat", "model-a", tools, "system", history)
		}()},
		{"protocol", prefixForSimCache("anthropic-messages", "model-a", tools, "system", history)},
		{"model", prefixForSimCache("openai-chat", "model-b", tools, "system", history)},
		{"tools", prefixForSimCache("openai-chat", "model-a", []executor.ToolDefinition{{Name: "other"}}, "system", history)},
	}
	for _, tc := range cases {
		if tc.got == base {
			t.Errorf("%s change did not alter cache prefix", tc.name)
		}
	}
}
