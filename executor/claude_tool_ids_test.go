package executor

import (
	"strings"
	"testing"
)

func TestRepairClaudeMessagesJSON_PairsFromResult(t *testing.T) {
	out := RepairClaudeMessagesJSON([]byte(`{
		"messages":[
			{"role":"user","content":"uname"},
			{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"uname -sm"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_abc123","content":"Darwin arm64"}]}
		]
	}`))
	if !strings.Contains(string(out), `"id":"call_abc123"`) {
		t.Fatalf("expected paired id, out=%s", out)
	}
}

func TestRepairClaudeMessagesJSON_SynthesizesWhenUnpaired(t *testing.T) {
	out := RepairClaudeMessagesJSON([]byte(`{
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{}}]}
		]
	}`))
	if !strings.Contains(string(out), `"id":"toolu_`) {
		t.Fatalf("expected synthesized id, out=%s", out)
	}
}
