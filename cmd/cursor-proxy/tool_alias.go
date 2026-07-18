// tool_alias.go — helper that runs translator.FromServerMessage
// then aliases the tool name to what the client declared, if the
// client declared a compatible name.
//
// Cursor's model uses its own names for native tools (`shell`,
// `edit`, `pi_bash`, …). We normalise those to lowercase defaults
// (`bash`, `write`, …) in translator/native_tool_adapter.go, but
// codex declares `shell`, opencode declares `bash`, claude-code
// declares `Bash` — no single default works for everyone. This
// wrapper picks the client's exact declared name whenever the
// default we'd emit is in the same alias family as one they
// declared, per cursor2api's 2026-07-19 report.

package main

import (
	"github.com/router-for-me/cursor-proto/executor"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
	"github.com/router-for-me/cursor-proto/translator"
)

// translateEvent runs the server-message translator and, if the
// client declared its own tool set, rewrites the returned event's
// ToolName to match. Returns nil when the source message has no
// event to translate — callers keep their `if trEv == nil {
// continue }` pattern.
func translateEvent(msg *cursorpb.AgentV1_AgentServerMessage, clientToolNames []string) *translator.Event {
	ev := translator.FromServerMessage(msg)
	if ev == nil {
		return nil
	}
	translator.ApplyClientToolAlias(ev, clientToolNames)
	return ev
}

// toolDefNames extracts .Name from a []executor.ToolDefinition. Used
// by handlers whose request conversion (convertResponsesTools /
// convertGeminiTools) already produces the executor shape — those
// don't need to hit the original OpenAI/Anthropic-shaped tools
// slice again.
func toolDefNames(tools []executor.ToolDefinition) []string {
	if len(tools) == 0 {
		return nil
	}
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		if t.Name == "" {
			continue
		}
		out = append(out, t.Name)
	}
	return out
}
