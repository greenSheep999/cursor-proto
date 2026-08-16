package translator

import (
	"encoding/json"
	"strings"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// BlobEvent is derived from AgentV1_KvServerMessage.SetBlobArgs. Cursor
// commits the model's final assistant message to KV blobs alongside internal
// reasoning; the outermost JSON-shaped blob with role="assistant" is what
// the user should see.
type BlobEvent struct {
	// AssistantText is the final assistant message text, or "" if the blob
	// did not contain one.
	AssistantText string
	// ThoughtText is the model's internal reasoning shown when the blob is a
	// bare short string (Cursor sometimes leaks these; may be discarded).
	ThoughtText string
	// Signature is Cursor's provider-issued reasoning signature from an
	// assistant reasoning content block. It is safe to pass through verbatim
	// as Anthropic signature_delta data; it must never be synthesized.
	Signature string
}

// FromKvBlob attempts to extract an AssistantText / ThoughtText out of a
// KV SetBlobArgs payload. Returns nil if the blob is not text-shaped.
//
// Cursor's assistant-message blob has this shape (from live capture):
//
//	{
//	  "role": "assistant",
//	  "content": [
//	    { "type": "redacted-reasoning", "data": "..." },
//	    { "type": "text", "text": "Hello! How can I help you today?" }
//	  ],
//	  "id": "1"
//	}
//
// Other blobs are internal (system prompt, reasoning, arbitrary text). We
// only surface the assistant-role one.
func FromKvBlob(m *cursorpb.AgentV1_AgentServerMessage) *BlobEvent {
	if m == nil {
		return nil
	}
	kv := m.GetKvServerMessage()
	if kv == nil {
		return nil
	}
	sb := kv.GetSetBlobArgs()
	if sb == nil {
		return nil
	}
	data := sb.GetBlobData()
	if len(data) == 0 {
		return nil
	}

	// Try JSON parse first (this catches the assistant message blob).
	if event := extractAssistantBlobFromJSON(data); event != nil {
		return event
	}
	return nil
}

// extractAssistantTextFromJSON parses `data` as an AI-SDK-style assistant
// message and returns the concatenated `type: text` content.
func extractAssistantTextFromJSON(data []byte) string {
	event := extractAssistantBlobFromJSON(data)
	if event == nil {
		return ""
	}
	return event.AssistantText
}

func extractAssistantBlobFromJSON(data []byte) *BlobEvent {
	// Blobs may have leading garbage bytes; find the first '{' and try from there.
	start := -1
	for i, b := range data {
		if b == '{' {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	var obj struct {
		Role    string `json:"role"`
		Content []struct {
			Type      string `json:"type"`
			Text      string `json:"text"`
			Signature string `json:"signature"`
		} `json:"content"`
	}
	if err := json.Unmarshal(data[start:], &obj); err != nil {
		return nil
	}
	if !strings.EqualFold(obj.Role, "assistant") {
		return nil
	}
	var text, thought strings.Builder
	var signature string
	for _, c := range obj.Content {
		switch c.Type {
		case "text":
			text.WriteString(c.Text)
		case "reasoning", "thinking":
			thought.WriteString(c.Text)
			if signature == "" {
				signature = c.Signature
			}
		}
	}
	if text.Len() == 0 && thought.Len() == 0 && signature == "" {
		return nil
	}
	return &BlobEvent{
		AssistantText: text.String(),
		ThoughtText:   thought.String(),
		Signature:     signature,
	}
}
