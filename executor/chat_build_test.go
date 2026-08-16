package executor

import "testing"

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
