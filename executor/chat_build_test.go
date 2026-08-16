package executor

import (
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
