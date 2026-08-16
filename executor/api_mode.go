package executor

import cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"

// APIConversationMode selects Cursor's conversation mode for an API request.
// Plain inference belongs in ASK mode so the model answers directly without
// invoking Cursor's IDE-native tools. Requests that expose client tools need
// AGENT mode so those tool calls can route back to the caller.
func APIConversationMode(hasTools bool) uint32 {
	if hasTools {
		return uint32(cursorpb.AgentV1_AGENT_MODE_AGENT)
	}
	return uint32(cursorpb.AgentV1_AGENT_MODE_ASK)
}
