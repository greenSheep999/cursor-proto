package executor

import (
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
)

func TestSanitizeMcpToolName(t *testing.T) {
	cases := map[string]string{
		"get_weather": "mcp_get_weather",
		"TodoWrite":   "mcp_TodoWrite",
		"WebFetch":    "mcp_WebFetch",
		"Task":        "mcp_Task",
		"Delete":      "mcp_Delete",
		"mcp_Custom":  "mcp_mcp_Custom",
	}
	for in, want := range cases {
		if got := SanitizeMcpToolName(in); got != want {
			t.Errorf("SanitizeMcpToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRestoreMcpToolName(t *testing.T) {
	cases := map[string]string{
		"mcp_TodoWrite":     "TodoWrite",
		"mcp_WebFetch":      "WebFetch",
		"mcp_get_weather":   "get_weather",
		"mcp_mcp_Custom":    "mcp_Custom",
		"unprefixed_server": "unprefixed_server",
	}
	for in, want := range cases {
		if got := RestoreMcpToolName(in); got != want {
			t.Errorf("RestoreMcpToolName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEncodeInputSchemaRoundTrip(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"location": map[string]any{
				"type":        "string",
				"description": "City name",
			},
		},
		"required": []any{"location"},
	}
	b, err := encodeInputSchema(schema)
	if err != nil {
		t.Fatalf("encodeInputSchema: %v", err)
	}
	if len(b) == 0 {
		t.Fatal("empty bytes")
	}
	// Round-trip via structpb.Value.
	var v structpb.Value
	if err := proto.Unmarshal(b, &v); err != nil {
		t.Fatalf("unmarshal Value: %v", err)
	}
	sv := v.GetStructValue()
	if sv == nil {
		t.Fatal("no struct_value")
	}
	if sv.Fields["type"].GetStringValue() != "object" {
		t.Errorf("type field lost: %v", sv.Fields["type"])
	}
	if sv.Fields["properties"].GetStructValue() == nil {
		t.Errorf("properties field lost")
	}
	// Required must be a list of strings.
	lv := sv.Fields["required"].GetListValue()
	if lv == nil || len(lv.Values) != 1 || lv.Values[0].GetStringValue() != "location" {
		t.Errorf("required lost: %v", sv.Fields["required"])
	}
}

func TestBuildMcpToolDefinitions(t *testing.T) {
	defs, err := buildMcpToolDefinitions([]ToolDefinition{
		{
			Name:        "get_weather",
			Description: "Look up the weather",
			InputSchema: map[string]any{"type": "object"},
		},
		{
			Name:        "TodoWrite",
			Description: "Update todos",
			InputSchema: nil,
		},
	})
	if err != nil {
		t.Fatalf("buildMcpToolDefinitions: %v", err)
	}
	if len(defs) != 2 {
		t.Fatalf("want 2 defs, got %d", len(defs))
	}
	if defs[0].Name != "mcp_get_weather" || defs[0].ToolName != "mcp_get_weather" {
		t.Errorf("unexpected name/tool_name: %s/%s", defs[0].Name, defs[0].ToolName)
	}
	if defs[0].ProviderIdentifier != "cursor-tools" {
		t.Errorf("provider = %q, want cursor-tools", defs[0].ProviderIdentifier)
	}
	if defs[1].Name != "mcp_TodoWrite" {
		t.Errorf("reserved name should be prefixed, got %q", defs[1].Name)
	}
	if len(defs[0].InputSchema) == 0 {
		t.Errorf("input schema empty")
	}
}

func TestBuildMcpToolDefinitionsMissingName(t *testing.T) {
	_, err := buildMcpToolDefinitions([]ToolDefinition{
		{Name: "", Description: "no name"},
	})
	if err == nil {
		t.Fatal("expected error for missing name")
	}
}

func TestBuildMcpInstructionsShape(t *testing.T) {
	instr := buildMcpInstructions([]ToolDefinition{
		{Name: "get_weather", Description: "Look up weather"},
		{Name: "TodoWrite", Description: "Update todos"},
	})
	if instr == nil {
		t.Fatal("nil instructions")
	}
	if instr.ServerName != "cursor-tools" || instr.ServerIdentifier != "cursor-tools" {
		t.Errorf("provider identifiers = %q/%q", instr.ServerName, instr.ServerIdentifier)
	}
	if !strings.Contains(instr.Instructions, "mcp_get_weather") ||
		!strings.Contains(instr.Instructions, "mcp_TodoWrite") {
		t.Errorf("missing tool names in instructions:\n%s", instr.Instructions)
	}
}

func TestBuildMcpInstructionsEmpty(t *testing.T) {
	if got := buildMcpInstructions(nil); got != nil {
		t.Errorf("empty tools should return nil, got %+v", got)
	}
}
