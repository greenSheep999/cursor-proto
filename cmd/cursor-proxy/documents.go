package main

import (
	"bytes"
	"fmt"
	"html"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/ledongthuc/pdf"

	"github.com/router-for-me/cursor-proto/executor"
)

const maxInlinedDocumentText = 500_000

func prepareDocumentAttachments(userText string, attachments []executor.Attachment) (string, []executor.Attachment) {
	if len(attachments) == 0 {
		return userText, nil
	}

	kept := make([]executor.Attachment, 0, len(attachments))
	documents := make([]string, 0, len(attachments))
	for _, attachment := range attachments {
		if attachment.Kind != "document" {
			kept = append(kept, attachment)
			continue
		}

		text, ok := extractDocumentText(attachment)
		if !ok {
			kept = append(kept, attachment)
			continue
		}
		if len(text) > maxInlinedDocumentText {
			text = text[:maxInlinedDocumentText] + "\n[document truncated]"
		}
		filename := strings.TrimSpace(attachment.Filename)
		if filename == "" {
			filename = "document"
		}
		documents = append(documents, fmt.Sprintf("<document filename=\"%s\">\n%s\n</document>", html.EscapeString(filename), text))
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

func extractDocumentText(attachment executor.Attachment) (string, bool) {
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
