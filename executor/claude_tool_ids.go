package executor

import (
	"encoding/json"
	"strings"
)

// NewToolUseID returns a Vertex/Anthropic-safe tool_use.id.
func NewToolUseID() string {
	return newHistoryToolID()
}

// RepairClaudeMessagesJSON copies tool_result.tool_use_id onto a preceding
// assistant tool_use that arrived without id, and synthesizes an id when no
// result is available. TokenSheep/NewAPI omitempty-strips empty ids.
func RepairClaudeMessagesJSON(raw []byte) []byte {
	var req map[string]any
	if json.Unmarshal(raw, &req) != nil {
		return raw
	}
	messages, ok := req["messages"].([]any)
	if !ok {
		return raw
	}
	type loc struct{ msg, part int }
	var pending []loc
	changed := false
	for mi, item := range messages {
		msg, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		parts, ok := msg["content"].([]any)
		if !ok {
			continue
		}
		for pi, partItem := range parts {
			part, ok := partItem.(map[string]any)
			if !ok {
				continue
			}
			switch part["type"] {
			case "tool_use":
				if role == "assistant" && strings.TrimSpace(asString(part["id"])) == "" {
					pending = append(pending, loc{mi, pi})
				}
			case "tool_result":
				if len(pending) == 0 {
					continue
				}
				use := pending[0]
				pending = pending[1:]
				id := strings.TrimSpace(asString(part["tool_use_id"]))
				if id == "" {
					id = NewToolUseID()
					part["tool_use_id"] = id
					changed = true
				}
				if setMessagePartID(messages, use.msg, use.part, id) {
					changed = true
				}
			}
		}
	}
	for _, use := range pending {
		if setMessagePartID(messages, use.msg, use.part, NewToolUseID()) {
			changed = true
		}
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(req)
	if err != nil {
		return raw
	}
	return out
}

func setMessagePartID(messages []any, msgIdx, partIdx int, id string) bool {
	if msgIdx < 0 || msgIdx >= len(messages) {
		return false
	}
	msg, ok := messages[msgIdx].(map[string]any)
	if !ok {
		return false
	}
	parts, ok := msg["content"].([]any)
	if !ok || partIdx < 0 || partIdx >= len(parts) {
		return false
	}
	part, ok := parts[partIdx].(map[string]any)
	if !ok {
		return false
	}
	part["id"] = id
	return true
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
