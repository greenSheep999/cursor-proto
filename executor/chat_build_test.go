package executor

import "testing"

func TestBuildAgentRunRequest_Attachments(t *testing.T) {
	client := &Client{}
	req, err := client.buildAgentRunRequest(&ChatRequest{
		Model:       "claude-opus-4-8-medium",
		UserMessage: "inspect",
		Attachments: []Attachment{
			{Kind: "image", Filename: "pixel.png", MimeType: "image/png", Data: []byte("image"), Width: 1, Height: 1},
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
	if dimension := selected.GetSelectedImages()[0].GetDimension(); dimension.GetWidth() != 1 || dimension.GetHeight() != 1 {
		t.Fatalf("selected image dimension = %+v", dimension)
	}
	if len(selected.GetSelectedDocuments()) != 1 || string(selected.GetSelectedDocuments()[0].GetData()) != "pdf" {
		t.Fatalf("selected documents = %+v", selected.GetSelectedDocuments())
	}
}
