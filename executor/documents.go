package executor

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"
)

const maxInlinedDocumentText = 500_000

// PrepareDocumentAttachments converts document attachments whose text we can
// extract into an inline `<document>` block on the user turn, and returns the
// attachments that still need to travel as binary.
//
// Cursor accepts SelectedContext.selected_documents on the wire but a request
// carrying one never produces a response: the paired RunSSE stays open with no
// semantic event and no trailer, so the caller hangs until its own deadline.
// Inlining the text is the only shape that reliably reaches the model, so
// RunChat applies this to every surface rather than leaving it to callers.
func PrepareDocumentAttachments(userText string, attachments []Attachment) (string, []Attachment) {
	if len(attachments) == 0 {
		return userText, nil
	}

	kept := make([]Attachment, 0, len(attachments))
	documents := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Kind != "document" {
			kept = append(kept, attachment)
			continue
		}

		text, ok := extractDocumentText(attachment)
		if !ok {
			// An undecodable document would hang the turn if forwarded, so
			// describe it instead of sending bytes Cursor will not answer.
			documents = append(documents, fmt.Sprintf(
				"<document filename=%q media_type=%q>\n[binary document that could not be converted to text]\n</document>",
				documentFilename(attachment), attachment.MimeType))
			continue
		}
		if len(text) > maxInlinedDocumentText {
			text = truncateUTF8ByBytes(text, maxInlinedDocumentText) + "\n[document truncated]"
		}
		documents = append(documents, fmt.Sprintf("<document filename=\"%s\">\n%s\n</document>",
			html.EscapeString(documentFilename(attachment)), text))
	}

	if len(documents) == 0 {
		return userText, kept
	}
	parts := make([]string, 0, len(documents)+1)
	if strings.TrimSpace(userText) != "" {
		parts = append(parts, userText)
	}
	parts = append(parts, documents...)
	return strings.Join(parts, "\n\n"), kept
}

func truncateUTF8ByBytes(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(text) <= maxBytes {
		return text
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end]
}

func documentFilename(attachment Attachment) string {
	if name := strings.TrimSpace(attachment.Filename); name != "" {
		return name
	}
	return "document"
}

func extractDocumentText(attachment Attachment) (string, bool) {
	mimeType := strings.ToLower(strings.TrimSpace(attachment.MimeType))
	if mimeType == "application/pdf" || strings.HasSuffix(strings.ToLower(attachment.Filename), ".pdf") {
		reader, err := pdf.NewReader(bytes.NewReader(attachment.Data), int64(len(attachment.Data)))
		if err != nil {
			return "", false
		}
		plainText, err := reader.GetPlainText()
		if err != nil {
			return "", false
		}
		data, err := io.ReadAll(io.LimitReader(plainText, maxInlinedDocumentText+1))
		if err != nil {
			return "", false
		}
		text := strings.TrimSpace(string(data))
		return text, text != ""
	}

	if strings.HasPrefix(mimeType, "text/") || mimeType == "application/json" || mimeType == "application/xml" {
		if !utf8.Valid(attachment.Data) {
			return "", false
		}
		text := strings.TrimSpace(string(attachment.Data))
		return text, text != ""
	}
	return "", false
}
