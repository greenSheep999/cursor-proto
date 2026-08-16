package executor

import (
	"fmt"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// ToolDefinition is a caller-supplied MCP tool advertised to the model.
//
// InputSchema is a JSON Schema object (the same shape callers pass over the
// wire for OpenAI `function.parameters` or Anthropic `input_schema`). At wire
// time we wrap it in a `google.protobuf.Value` — Cursor 3.10's
// McpToolDefinition.input_schema field is a Value, not a Struct. Encoding it
// as Struct stalls the SSE stream silently. See docs/pitfalls-2.3.41.md and
// docs/phase-7a-mcp.md for the write-up.
type ToolDefinition struct {
	Name        string
	Description string
	// InputSchema is a JSON Schema object. It gets wrapped into a
	// google.protobuf.Value at wire time.
	InputSchema map[string]any
}

// mcpProviderIdentifier is the fixed provider tag we register user-supplied
// MCP tools under. The value matches the JS reference implementation
// (buildMcpToolsWrapper in reference/js-src/agentClient.js).
const mcpProviderIdentifier = "cursor-tools"

const externalToolPrefix = "mcp_"

// SanitizeMcpToolName returns the wire name for a caller-supplied tool. It
// places every external tool in a dedicated namespace so it cannot collide
// with Cursor's built-in tools. Some built-in-looking names fail explicitly,
// while combinations such as Bash + Read + Edit + Write only emit heartbeats
// and never start generation. Prefixing every caller tool avoids both failure
// modes without maintaining a version-specific reserved-name list.
func SanitizeMcpToolName(name string) string {
	return externalToolPrefix + name
}

// RestoreMcpToolName inverts SanitizeMcpToolName so tool-call events surface
// the caller's original name.
func RestoreMcpToolName(name string) string {
	if strings.HasPrefix(name, externalToolPrefix) {
		return strings.TrimPrefix(name, externalToolPrefix)
	}
	return name
}

// encodeInputSchema wraps a JSON Schema map in a google.protobuf.Value and
// returns the serialized bytes. The result is what goes into
// McpToolDefinition.input_schema on the wire.
//
// The generated Go proto declares InputSchema as `[]byte` because the source
// proto used an opaque message reference (`lY`) rather than the well-known
// Value type — but the wire tag is length-delimited (field 3, wire type 2),
// so a marshaled Value fits exactly the same slot.
func encodeInputSchema(schema map[string]any) ([]byte, error) {
	if schema == nil {
		schema = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	stru, err := structpb.NewStruct(schema)
	if err != nil {
		return nil, fmt.Errorf("input_schema to Struct: %w", err)
	}
	val := structpb.NewStructValue(stru)
	b, err := proto.Marshal(val)
	if err != nil {
		return nil, fmt.Errorf("marshal google.protobuf.Value: %w", err)
	}
	return b, nil
}

// buildMcpToolDefinitions converts caller tools into the wire-form repeated
// McpToolDefinition list used in both AgentRunRequest.mcp_tools and
// RequestContext.tools.
func buildMcpToolDefinitions(tools []ToolDefinition) ([]*cursorpb.AgentV1_McpToolDefinition, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]*cursorpb.AgentV1_McpToolDefinition, 0, len(tools))
	for i, t := range tools {
		if t.Name == "" {
			return nil, fmt.Errorf("tool %d: name is required", i)
		}
		safe := SanitizeMcpToolName(t.Name)
		schemaBytes, err := encodeInputSchema(t.InputSchema)
		if err != nil {
			return nil, fmt.Errorf("tool %q: %w", t.Name, err)
		}
		out = append(out, &cursorpb.AgentV1_McpToolDefinition{
			Name:               safe,
			Description:        t.Description,
			InputSchema:        schemaBytes,
			ProviderIdentifier: mcpProviderIdentifier,
			ToolName:           safe,
		})
	}
	return out, nil
}

// buildMcpInstructions builds the single McpInstructions entry that tells the
// model how to invoke the caller's tools. The JS reference concatenates a
// one-line summary of every tool; we do the same, sorted for stability.
func buildMcpInstructions(tools []ToolDefinition) *cursorpb.AgentV1_McpInstructions {
	if len(tools) == 0 {
		return nil
	}
	names := make([]string, 0, len(tools))
	byName := make(map[string]string, len(tools))
	for _, t := range tools {
		safe := SanitizeMcpToolName(t.Name)
		names = append(names, safe)
		desc := t.Description
		if desc == "" {
			desc = "No description"
		}
		byName[safe] = desc
	}
	sort.Strings(names)

	var b strings.Builder
	b.WriteString("Available MCP tools:\n")
	for _, n := range names {
		b.WriteString("- ")
		b.WriteString(n)
		b.WriteString(": ")
		b.WriteString(byName[n])
		b.WriteString("\n")
	}
	b.WriteString("\nCall tools using their exact name and JSON arguments.")

	// The generated proto exposes field 1 as `ServerName` and field 3 as
	// `ServerIdentifier`. The JS reference (which is the source of truth for
	// what Cursor's server accepts) writes the provider identifier into
	// field 1. We populate both so the server sees a consistent identity
	// regardless of which slot it reads.
	return &cursorpb.AgentV1_McpInstructions{
		ServerName:       mcpProviderIdentifier,
		Instructions:     b.String(),
		ServerIdentifier: mcpProviderIdentifier,
	}
}
