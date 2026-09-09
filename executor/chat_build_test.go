package executor

import (
	"strings"
	"testing"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

func TestBuildAgentRunRequest_Attachments(t *testing.T) {
	client := &Client{}
	req, err := client.buildAgentRunRequest(&ChatRequest{
		Model:       "claude-opus-4-8-medium",
		UserMessage: "inspect",
		Attachments: []Attachment{
			{Kind: "image", Filename: "pixel.png", MimeType: "image/png", Data: []byte("image")},
			{Kind: "document", Filename: "report.pdf", MimeType: "application/pdf", Data: []byte("pdf")},
		},
	}, "message-id")
	if err != nil {
		t.Fatalf("buildAgentRunRequest: %v", err)
	}
	selected := req.GetAction().GetUserMessageAction().GetUserMessage().GetSelectedContext()
	if selected == nil {
		t.Fatal("selected context is nil")
	}
	if len(selected.GetSelectedImages()) != 1 || string(selected.GetSelectedImages()[0].GetData()) != "image" {
		t.Fatalf("selected images = %+v", selected.GetSelectedImages())
	}
	image := selected.GetSelectedImages()[0]
	if image.GetPath() != "" || image.GetDimension() != nil {
		t.Fatalf("selected image includes fields absent from Cursor client wire shape: %+v", image)
	}
	if len(selected.GetSelectedDocuments()) != 1 || string(selected.GetSelectedDocuments()[0].GetData()) != "pdf" {
		t.Fatalf("selected documents = %+v", selected.GetSelectedDocuments())
	}
	if path := selected.GetSelectedDocuments()[0].GetPath(); path != "" {
		t.Fatalf("selected document path = %q, want empty", path)
	}
}

func TestBuildAgentRunRequest_EnablesNativeWebTools(t *testing.T) {
	client := &Client{}
	req, err := client.buildAgentRunRequest(&ChatRequest{
		Model:       "claude-opus-5-medium",
		UserMessage: "search",
		WebSearch:   true,
		WebFetch:    true,
	}, "message-id")
	if err != nil {
		t.Fatalf("buildAgentRunRequest: %v", err)
	}
	ctx := req.GetAction().GetUserMessageAction().GetRequestContext()
	if !ctx.GetWebSearchEnabled() || !ctx.GetWebFetchEnabled() {
		t.Fatalf("native web flags not enabled: search=%v fetch=%v", ctx.GetWebSearchEnabled(), ctx.GetWebFetchEnabled())
	}
}

func TestBuildConversationHistory_ToolUseAndResult(t *testing.T) {
	hist := buildConversationHistory([]HistoryTurn{
		{Role: "user", Content: "Check Shanghai weather."},
		{Role: "assistant", Content: "I'll check.\n" + `{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{"city":"Shanghai"}}`},
		{Role: "user", Content: `{"type":"tool_result","tool_use_id":"toolu_123","content":"Sunny, 28 C."}`},
	})
	if hist == nil || len(hist.Messages) != 3 {
		t.Fatalf("history messages = %+v, want 3", hist)
	}
	asst := hist.Messages[1].GetAssistant()
	if asst == nil || len(asst.Content) != 2 {
		t.Fatalf("assistant content = %+v", asst)
	}
	if asst.Content[0].GetText().GetText() != "I'll check." {
		t.Fatalf("assistant text = %q", asst.Content[0].GetText().GetText())
	}
	call := asst.Content[1].GetToolCall()
	if call.GetToolName() != "get_weather" || call.GetToolCallId() != "toolu_123" {
		t.Fatalf("tool call = %+v", call)
	}
	if call.GetArgsJson() != `{"city":"Shanghai"}` {
		t.Fatalf("args = %q", call.GetArgsJson())
	}
	tool := hist.Messages[2].GetTool()
	if tool == nil || tool.GetToolCallId() != "toolu_123" {
		t.Fatalf("tool message = %+v", tool)
	}
	if tool.GetContent()[0].GetText().GetText() != "Sunny, 28 C." {
		t.Fatalf("tool result text = %q", tool.GetContent()[0].GetText().GetText())
	}
}

func TestBuildConversationHistory_FillsEmptyToolUseIDFromResult(t *testing.T) {
	hist := buildConversationHistory([]HistoryTurn{
		{Role: "user", Content: "uname"},
		{Role: "assistant", Content: `{"type":"tool_use","name":"Bash","input":{"command":"uname -sm"}}`},
		{Role: "user", Content: `{"type":"tool_result","tool_use_id":"call_abc123","content":"Darwin arm64"}`},
	})
	if hist == nil || len(hist.Messages) != 3 {
		t.Fatalf("history messages = %+v, want 3", hist)
	}
	call := hist.Messages[1].GetAssistant().GetContent()[0].GetToolCall()
	if call.GetToolCallId() != "call_abc123" {
		t.Fatalf("empty tool_use id should take the tool_result id, got %q", call.GetToolCallId())
	}
	if hist.Messages[2].GetTool().GetToolCallId() != "call_abc123" {
		t.Fatalf("tool result id = %q", hist.Messages[2].GetTool().GetToolCallId())
	}
}

func TestParseContentFragmentsAndContinuation(t *testing.T) {
	raw := `{"type":"tool_result","tool_use_id":"toolu_123","content":"Sunny, 28 C."}`
	got := ParseContentFragments(raw)
	if len(got) != 1 || got[0].Kind != ContentToolResult || got[0].Result != "Sunny, 28 C." {
		t.Fatalf("fragments = %+v", got)
	}
	transcript := spliceHistory([]HistoryTurn{
		{Role: "assistant", Content: `{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{"city":"Paris"}}`},
		{Role: "user", Content: raw},
	}, "summarize")
	if !containsAll(transcript, "[tool_use name=get_weather id=toolu_123]", "[tool_result id=toolu_123]", "Sunny, 28 C.") {
		t.Fatalf("transcript = %q", transcript)
	}
}

func containsAll(s string, needles ...string) bool {
	for _, needle := range needles {
		if !strings.Contains(s, needle) {
			return false
		}
	}
	return true
}

func TestBuildAgentRunRequest_UsesRoutedRequestedModel(t *testing.T) {
	client := &Client{}
	chat := &ChatRequest{
		Model:       "claude-opus-4-8",
		UserMessage: "hello",
		resolvedModel: &cursorpb.AgentV1_RequestedModel{
			ModelId: "claude-opus-4-8",
			Parameters: []*cursorpb.AgentV1_RequestedModel_ModelParameterValue{{
				Id: "effort", Value: "high",
			}},
		},
	}

	req, err := client.buildAgentRunRequest(chat, "message-id")
	if err != nil {
		t.Fatalf("buildAgentRunRequest: %v", err)
	}
	if req.GetModelDetails() != nil {
		t.Fatalf("model_details should be omitted for routed catalog models: %+v", req.GetModelDetails())
	}
	if req.GetRequestedModel().GetModelId() != "claude-opus-4-8" {
		t.Fatalf("requested model = %+v", req.GetRequestedModel())
	}
	if !req.GetClientSupportsRoutedModelUpdate() {
		t.Fatal("client_supports_routed_model_update is false")
	}
	if !req.GetClientSupportsPromptContextUsageRpc() {
		t.Fatal("client_supports_prompt_context_usage_rpc is false")
	}
}
