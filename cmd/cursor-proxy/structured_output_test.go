package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAppendStructuredOutputInstructionJSONSchema(t *testing.T) {
	raw := json.RawMessage(`{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false}}}`)
	got := appendStructuredOutputInstruction("be concise", raw)
	for _, want := range []string{"be concise", "Return only valid JSON", `"required":["answer"]`, `"additionalProperties":false`} {
		if !strings.Contains(got, want) {
			t.Fatalf("instruction %q does not contain %q", got, want)
		}
	}
}

func TestAppendResponsesStructuredOutputInstruction(t *testing.T) {
	raw := json.RawMessage(`{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object"}}}`)
	got := appendResponsesStructuredOutputInstruction("", raw)
	if !strings.Contains(got, `"type":"object"`) {
		t.Fatalf("instruction = %q", got)
	}
}
