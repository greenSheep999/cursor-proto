package executor

import (
	"testing"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

func TestAPIConversationMode(t *testing.T) {
	if got := APIConversationMode(false); got != uint32(cursorpb.AgentV1_AGENT_MODE_ASK) {
		t.Fatalf("mode without tools = %d, want ASK", got)
	}
	if got := APIConversationMode(true); got != uint32(cursorpb.AgentV1_AGENT_MODE_AGENT) {
		t.Fatalf("mode with tools = %d, want AGENT", got)
	}
}
