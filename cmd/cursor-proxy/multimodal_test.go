package main

import (
	"encoding/base64"
	"strings"
	"testing"

	"github.com/router-for-me/cursor-proto/executor"
)

func TestFlattenAnthropicContentWithAttachments(t *testing.T) {
	pdf := base64.StdEncoding.EncodeToString([]byte("pdf-bytes"))
	image := base64.StdEncoding.EncodeToString([]byte("image-bytes"))
	content := []any{
		map[string]any{"type": "text", "text": "inspect both"},
		map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": image}},
		map[string]any{"type": "document", "title": "report.pdf", "source": map[string]any{"type": "base64", "media_type": "application/pdf", "data": pdf}},
	}

	text, attachments := flattenAnthropicContentWithAttachments(content)
	if text != "inspect both" {
		t.Fatalf("text = %q", text)
	}
	if len(attachments) != 2 {
		t.Fatalf("attachments = %+v", attachments)
	}
	if attachments[0].Kind != "image" || string(attachments[0].Data) != "image-bytes" {
		t.Fatalf("image attachment = %+v", attachments[0])
	}
	if attachments[1].Kind != "document" || attachments[1].Filename != "report.pdf" || string(attachments[1].Data) != "pdf-bytes" {
		t.Fatalf("document attachment = %+v", attachments[1])
	}
}

func TestPrepareDocumentAttachmentsInlinesTextDocuments(t *testing.T) {
	text, attachments := prepareDocumentAttachments("summarize", []executor.Attachment{
		{Kind: "image", MimeType: "image/png", Data: []byte("image")},
		{Kind: "document", Filename: "notes.txt", MimeType: "text/plain", Data: []byte("DOCUMENT_SECRET_7391")},
	})

	if !strings.Contains(text, "DOCUMENT_SECRET_7391") || !strings.Contains(text, `filename="notes.txt"`) {
		t.Fatalf("prepared text = %q", text)
	}
	if len(attachments) != 1 || attachments[0].Kind != "image" {
		t.Fatalf("remaining attachments = %+v", attachments)
	}
}
