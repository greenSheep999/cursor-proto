package main

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/cursor-proto/executor"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

func thinkingBlobEvent(thought, signature, text string) executor.ChatEvent {
	blob := `{"role":"assistant","content":[{"type":"reasoning","text":` + encodeJSONString(thought) + `,"signature":` + encodeJSONString(signature) + `},{"type":"text","text":` + encodeJSONString(text) + `}]}`
	return executor.ChatEvent{Server: &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_KvServerMessage{
			KvServerMessage: &cursorpb.AgentV1_KvServerMessage{
				Message: &cursorpb.AgentV1_KvServerMessage_SetBlobArgs{
					SetBlobArgs: &cursorpb.AgentV1_SetBlobArgs{BlobData: []byte(blob)},
				},
			},
		},
	}}
}

func TestNonStreamAnthropicPreservesProviderThinkingSignature(t *testing.T) {
	events := make(chan executor.ChatEvent, 2)
	events <- thinkingBlobEvent("private reasoning", "provider-signature", "FINAL_OK")
	events <- turnEndedEvent(20, 5, 0)
	close(events)

	recorder := httptest.NewRecorder()
	nonStreamAnthropic(recorder, "claude-test", events, simCacheDecision{}, nil, true)
	body := recorder.Body.String()
	for _, required := range []string{`"type":"thinking"`, `"thinking":"private reasoning"`, `"signature":"provider-signature"`, `"text":"FINAL_OK"`} {
		if !strings.Contains(body, required) {
			t.Fatalf("body missing %s: %s", required, body)
		}
	}
}

func TestStreamAnthropicOrdersThinkingSignatureBeforeText(t *testing.T) {
	events := make(chan executor.ChatEvent, 3)
	events <- textDeltaEvent("FINAL_OK")
	events <- thinkingBlobEvent("private reasoning", "provider-signature", "FINAL_OK")
	events <- turnEndedEvent(20, 5, 0)
	close(events)

	recorder := httptest.NewRecorder()
	streamAnthropic(recorder, "claude-test", events, simCacheDecision{}, nil, true)
	body := recorder.Body.String()
	thinkingAt := strings.Index(body, `"type":"thinking_delta"`)
	signatureAt := strings.Index(body, `"type":"signature_delta"`)
	textAt := strings.Index(body, `"type":"text_delta"`)
	if thinkingAt < 0 || signatureAt < 0 || textAt < 0 || !(thinkingAt < signatureAt && signatureAt < textAt) {
		t.Fatalf("thinking/signature/text order invalid: %s", body)
	}
}
