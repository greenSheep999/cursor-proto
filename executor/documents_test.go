package executor

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestPrepareDocumentAttachmentsTruncatesAtUTF8Boundary(t *testing.T) {
	data := []byte(strings.Repeat("a", maxInlinedDocumentText-1) + "界tail")
	text, kept := PrepareDocumentAttachments("", []Attachment{{
		Kind:     "document",
		Filename: "utf8.txt",
		MimeType: "text/plain",
		Data:     data,
	}})

	if len(kept) != 0 {
		t.Fatalf("kept %d attachments, want none", len(kept))
	}
	if !utf8.ValidString(text) {
		t.Fatal("truncated document contains invalid UTF-8")
	}
	if !strings.Contains(text, "[document truncated]") {
		t.Fatalf("missing truncation marker: %q", text[len(text)-64:])
	}
}
