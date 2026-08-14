package main

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/translator"
)

const emptyUpstreamMessage = "empty response from upstream (no content, tool calls, or token usage)"

func isEmptyUpstreamResponse(hasOutput bool, usage *translator.Usage) bool {
	if hasOutput {
		return false
	}
	return usage == nil || usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 &&
		usage.CacheWriteTokens == 0 &&
		usage.ReasoningTokens == 0
}

// upstreamHTTPStatus maps a Cursor gRPC-status trailer into an HTTP status
// code appropriate for the OpenAI/Anthropic/Gemini surface. Cursor uses the
// standard google.rpc.Code enum values.
func upstreamHTTPStatus(status *executor.TrailerStatus) int {
	if status == nil {
		return http.StatusBadGateway
	}
	switch status.Code {
	case 3: // InvalidArgument
		return http.StatusBadRequest
	case 5: // NotFound
		return http.StatusNotFound
	case 7: // PermissionDenied
		return http.StatusForbidden
	case 8: // ResourceExhausted (Cursor uses this for region + model-gate)
		return http.StatusForbidden
	case 12: // Unimplemented
		return http.StatusNotImplemented
	case 16: // Unauthenticated
		return http.StatusUnauthorized
	}
	return http.StatusBadGateway
}

// upstreamErrorType maps the trailer to an OpenAI-style error `type` string.
// Callers use it when synthesizing an OpenAI/Anthropic error envelope.
func upstreamErrorType(status *executor.TrailerStatus) string {
	if status == nil {
		return "upstream_error"
	}
	if status.Code == 8 || status.Code == 7 {
		return "permission_error"
	}
	if status.Code == 16 {
		return "authentication_error"
	}
	if status.Code == 3 {
		return "invalid_request_error"
	}
	return "api_error"
}

// writeUpstreamOpenAIError renders an OpenAI-shaped error envelope. Suitable
// for /v1/chat/completions, /v1/responses, /v1/completions.
func writeUpstreamOpenAIError(w http.ResponseWriter, status *executor.TrailerStatus) {
	code := upstreamHTTPStatus(status)
	log.Printf("[proxy] upstream trailer error (HTTP %d): %s", code, errMessage(status))
	body := map[string]any{
		"error": map[string]any{
			"message": errMessage(status),
			"type":    upstreamErrorType(status),
			"code":    upstreamCodeString(status),
		},
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[proxy] failed to encode upstream error: %v", err)
	}
}

func writeEmptyUpstreamOpenAIError(w http.ResponseWriter) {
	log.Printf("[proxy] %s", emptyUpstreamMessage)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": emptyUpstreamMessage,
			"type":    "upstream_error",
			"code":    "EMPTY_UPSTREAM_RESPONSE",
		},
	})
}

// writeUpstreamAnthropicError renders an Anthropic-shaped error envelope for
// /v1/messages.
func writeUpstreamAnthropicError(w http.ResponseWriter, status *executor.TrailerStatus) {
	code := upstreamHTTPStatus(status)
	log.Printf("[proxy] upstream trailer error (HTTP %d): %s", code, errMessage(status))
	body := map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    upstreamErrorType(status),
			"message": errMessage(status),
		},
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[proxy] failed to encode upstream error: %v", err)
	}
}

func writeEmptyUpstreamAnthropicError(w http.ResponseWriter) {
	log.Printf("[proxy] %s", emptyUpstreamMessage)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "upstream_error",
			"message": emptyUpstreamMessage,
		},
	})
}

// writeUpstreamGeminiError renders a Gemini-style error envelope for
// /v1beta/models/{model}:generateContent.
func writeUpstreamGeminiError(w http.ResponseWriter, status *executor.TrailerStatus) {
	code := upstreamHTTPStatus(status)
	log.Printf("[proxy] upstream trailer error (HTTP %d): %s", code, errMessage(status))
	body := map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": errMessage(status),
			"status":  upstreamCodeString(status),
		},
	}
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("[proxy] failed to encode upstream error: %v", err)
	}
}

func writeEmptyUpstreamGeminiError(w http.ResponseWriter) {
	log.Printf("[proxy] %s", emptyUpstreamMessage)
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"code":    http.StatusBadGateway,
			"message": emptyUpstreamMessage,
			"status":  "EMPTY_UPSTREAM_RESPONSE",
		},
	})
}

func errMessage(status *executor.TrailerStatus) string {
	if status == nil {
		return "upstream error"
	}
	if err := status.Err(); err != nil {
		return err.Error()
	}
	return "upstream error"
}

// upstreamCodeString returns the enum name of the detail's Error field, or a
// short google.rpc.Code label when the detail is absent.
func upstreamCodeString(status *executor.TrailerStatus) string {
	if status == nil {
		return "UNKNOWN"
	}
	if status.Detail != nil {
		if s := status.Detail.GetError().String(); s != "" {
			return s
		}
	}
	switch status.Code {
	case 3:
		return "INVALID_ARGUMENT"
	case 5:
		return "NOT_FOUND"
	case 7:
		return "PERMISSION_DENIED"
	case 8:
		return "RESOURCE_EXHAUSTED"
	case 16:
		return "UNAUTHENTICATED"
	}
	return "UNAVAILABLE"
}

// drainEvents consumes the remainder of the ChatEvent channel so the goroutine
// backing the SSE stream can exit cleanly.
func drainEvents(ch <-chan executor.ChatEvent) {
	for range ch {
	}
}
