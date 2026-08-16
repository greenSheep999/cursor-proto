package kernel

// ABI method handlers for the three executor entry points:
//
//   executor.execute        — non-streaming call: collect the whole
//                              response and return it as one envelope.
//   executor.execute_stream — streaming call: emit each Cursor event
//                              back through host.stream.emit / close.
//   executor.count_tokens   — local heuristic estimate.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/translator"
)

// debugLog is a stderr trace helper for tracking down streaming
// bugs. Enabled by CURSOR_PLUGIN_DEBUG=1 in the environment; a
// no-op otherwise so production builds stay quiet.
func debugLog(msg string, args ...any) {
	if os.Getenv("CURSOR_PLUGIN_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[cursor-plugin] "+msg+"\n", args...)
}

// runChatFn matches the signature of executor.Client.RunChat. The
// handlers take it as a parameter (indirectly, via chatRunner) so
// tests can substitute a fake stream without spinning up a real
// Cursor client.
type chatRunner interface {
	RunChat(ctx context.Context, req *executor.ChatRequest) (<-chan executor.ChatEvent, error)
}

// runnerFactory produces a chatRunner for the request. Production
// builds return a cached *executor.Client from globalClientCache;
// tests inject their own fake.
var runnerFactory = func(authID string, storage []byte) (chatRunner, string, error) {
	client, err := globalClientCache.getClient(authID, storage)
	if err != nil {
		return nil, "", err
	}
	email := ""
	if client.Account != nil {
		email = client.Account.Email
	}
	return client, email, nil
}

// handleExecutorExecute implements executor.execute.
func handleExecutorExecute(payload []byte) ([]byte, int) {
	var req executorRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse executor request: %v", err), false), 0
	}
	shape, errParse := parseByFormat(req.Payload, req.Format)
	if errParse != nil {
		return errorEnvelope("bad_payload", errParse.Error(), false), 0
	}
	if strings.TrimSpace(req.Model) != "" {
		shape.Model = req.Model
	}
	if strings.TrimSpace(shape.Model) == "" {
		return errorEnvelope("bad_payload", "model is required", false), 0
	}
	excluded, errExcluded := storageExcludesModel(req.StorageJSON, shape.Model)
	if errExcluded != nil {
		return errorEnvelope("bad_auth", errExcluded.Error(), true), 0
	}
	if excluded {
		return errorEnvelope("model_excluded", fmt.Sprintf("model %s is excluded for this cursor account", shape.Model), true), 0
	}

	runner, _, errClient := runnerFactory(req.AuthID, req.StorageJSON)
	if errClient != nil {
		return errorEnvelope("bad_auth", errClient.Error(), true), 0
	}

	chatReq := buildChatRequest(shape, req.Headers)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events, errRun := runner.RunChat(ctx, chatReq)
	if errRun != nil {
		return errorEnvelope("upstream_error", errRun.Error(), true), 0
	}

	format := normaliseFormat(req.Format, req.SourceFormat)
	body, errCollect := collectNonStreaming(format, shape.Model, shape.Tools, events)
	if errCollect != nil {
		return errorEnvelope("upstream_error", errCollect.Error(), true), 0
	}
	headers := map[string][]string{"Content-Type": {"application/json"}}
	resp := executorResponse{Payload: body, Headers: headers}
	buf, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		return errorEnvelope("marshal_response", errMarshal.Error(), false), 0
	}
	return okEnvelopeJSON(string(buf)), 0
}

// collectNonStreaming iterates the RunChat channel and produces a
// full response body in the requested output format.
func collectNonStreaming(format, model string, tools []executor.ToolDefinition, events <-chan executor.ChatEvent) ([]byte, error) {
	switch format {
	case "claude":
		return buildClaudeNonStreaming(model, tools, events)
	default:
		return buildOpenAINonStreaming(model, tools, events)
	}
}

const emptyUpstreamResponseMessage = "empty response from upstream (no content, tool calls, or token usage)"

var errEmptyUpstreamResponse = errors.New(emptyUpstreamResponseMessage)

func isEmptyPluginUpstreamResponse(hasOutput bool, usage *translator.Usage) bool {
	if hasOutput {
		return false
	}
	return usage == nil || usage.InputTokens == 0 &&
		usage.OutputTokens == 0 &&
		usage.CacheReadTokens == 0 &&
		usage.CacheWriteTokens == 0 &&
		usage.ReasoningTokens == 0
}

// buildOpenAINonStreaming mirrors nonStreamOpenAI in cmd/cursor-proxy.
// It intentionally omits the cache-simulator logic — the plugin does
// not have opinions about caching, that's a host-level concern.
//
// When Cursor emits a KV blob (the assembled assistant text so far),
// we use it as the authoritative response. When only text deltas
// arrive (e.g. legacy stream shape or when a KV blob never gets
// flushed), we accumulate the deltas so the final response is still
// populated.
func buildOpenAINonStreaming(model string, tools []executor.ToolDefinition, events <-chan executor.ChatEvent) ([]byte, error) {
	acc := translator.NonStreamingAccumulator{Model: model}
	sawBlob := false
	deltaText := ""
	for ev := range events {
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			acc.Text = blob.AssistantText
			sawBlob = true
			continue
		}
		trEv := translatePluginEvent(ev.Server, tools)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			deltaText += trEv.Text
		case translator.EventToolCallStarted:
			acc.Consume(trEv)
		case translator.EventTurnEnded:
			acc.Usage = trEv.Usage
			acc.FinishStop = true
		}
	}
	if !sawBlob && deltaText != "" {
		acc.Text = deltaText
	}
	if isEmptyPluginUpstreamResponse(acc.Text != "" || len(acc.ToolCalls) > 0, acc.Usage) {
		return nil, errEmptyUpstreamResponse
	}
	return acc.Response("chatcmpl-" + auth.GenerateSessionID()), nil
}

// buildClaudeNonStreaming mirrors nonStreamAnthropic in cmd/cursor-proxy.
// Falls back to accumulating text deltas when Cursor never emits a
// KV blob (see buildOpenAINonStreaming for the rationale).
func buildClaudeNonStreaming(model string, tools []executor.ToolDefinition, events <-chan executor.ChatEvent) ([]byte, error) {
	assistantText := ""
	sawBlob := false
	deltaText := ""
	var usage *translator.Usage
	var toolUses []map[string]any
	for ev := range events {
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			assistantText = blob.AssistantText
			sawBlob = true
			continue
		}
		trEv := translatePluginEvent(ev.Server, tools)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			deltaText += trEv.Text
		case translator.EventToolCallStarted:
			var input any = map[string]any{}
			if trEv.ToolArgsDelta != "" {
				var parsed any
				if err := json.Unmarshal([]byte(trEv.ToolArgsDelta), &parsed); err == nil {
					input = parsed
				}
			}
			toolUses = append(toolUses, map[string]any{
				"type":  "tool_use",
				"id":    trEv.ToolCallID,
				"name":  trEv.ToolName,
				"input": input,
			})
		case translator.EventTurnEnded:
			usage = trEv.Usage
		}
	}
	if !sawBlob && deltaText != "" {
		assistantText = deltaText
	}
	if isEmptyPluginUpstreamResponse(assistantText != "" || len(toolUses) > 0, usage) {
		return nil, errEmptyUpstreamResponse
	}
	content := []map[string]any{}
	if assistantText != "" {
		content = append(content, map[string]any{"type": "text", "text": assistantText})
	}
	for _, tu := range toolUses {
		content = append(content, tu)
	}
	stopReason := translator.AnthropicStopReason(assistantText, len(toolUses) > 0)
	resp := map[string]any{
		"id":            "msg_" + strings.ReplaceAll(auth.GenerateSessionID(), "-", ""),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
	}
	if usage != nil {
		resp["usage"] = translator.BuildAnthropicUsage(usage)
	}
	buf, _ := json.Marshal(resp)
	return buf, nil
}

// handleExecutorExecuteStream implements executor.execute_stream. It
// returns synchronously (with empty chunks so the host uses the
// async stream_id path) and drives a background goroutine that emits
// SSE frames via host.stream.emit until RunChat is done.
func handleExecutorExecuteStream(payload []byte) ([]byte, int) {
	var req executorRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse executor request: %v", err), false), 0
	}
	if strings.TrimSpace(req.StreamID) == "" {
		return errorEnvelope("bad_request", "stream_id is required for executor.execute_stream", false), 0
	}
	shape, errParse := parseByFormat(req.Payload, req.Format)
	if errParse != nil {
		return errorEnvelope("bad_payload", errParse.Error(), false), 0
	}
	if strings.TrimSpace(req.Model) != "" {
		shape.Model = req.Model
	}
	if strings.TrimSpace(shape.Model) == "" {
		return errorEnvelope("bad_payload", "model is required", false), 0
	}
	excluded, errExcluded := storageExcludesModel(req.StorageJSON, shape.Model)
	if errExcluded != nil {
		return errorEnvelope("bad_auth", errExcluded.Error(), true), 0
	}
	if excluded {
		return errorEnvelope("model_excluded", fmt.Sprintf("model %s is excluded for this cursor account", shape.Model), true), 0
	}

	runner, _, errClient := runnerFactory(req.AuthID, req.StorageJSON)
	if errClient != nil {
		return errorEnvelope("bad_auth", errClient.Error(), true), 0
	}
	chatReq := buildChatRequest(shape, req.Headers)

	// Kick off the run before returning so any immediate wire errors
	// surface as an envelope failure instead of a silent stream close.
	ctx, cancel := context.WithCancel(context.Background())
	events, errRun := runner.RunChat(ctx, chatReq)
	if errRun != nil {
		cancel()
		return errorEnvelope("upstream_error", errRun.Error(), true), 0
	}

	format := normaliseFormat(req.Format, req.SourceFormat)
	headers := map[string][]string{"Content-Type": {"text/event-stream"}}

	go streamEvents(ctx, cancel, req.StreamID, format, shape.Model, shape.IncludeUsage, shape.Thinking, shape.Tools, events)

	// Async streaming: return synchronously with empty chunks. The
	// host will read chunks off the stream bridge as we emit them.
	resp := executorStreamResponse{Headers: headers}
	buf, errMarshal := json.Marshal(resp)
	if errMarshal != nil {
		cancel()
		return errorEnvelope("marshal_response", errMarshal.Error(), false), 0
	}
	return okEnvelopeJSON(string(buf)), 0
}

// streamEvents runs in a background goroutine for the lifetime of one
// executor.execute_stream call. It pumps Cursor events into the host
// stream bridge and always closes the stream on exit.
func streamEvents(ctx context.Context, cancel context.CancelFunc, streamID, format, model string, includeUsage, expectThinkingSignature bool, tools []executor.ToolDefinition, events <-chan executor.ChatEvent) {
	defer cancel()

	var streamErr string
	defer func() {
		closePayload, _ := json.Marshal(map[string]string{
			"stream_id": streamID,
			"error":     streamErr,
		})
		_, _ = callHost("host.stream.close", closePayload)
	}()

	switch format {
	case "claude":
		streamClaude(streamID, model, expectThinkingSignature, tools, events, &streamErr)
	default:
		streamOpenAI(streamID, model, includeUsage, tools, events, &streamErr)
	}
	_ = ctx // kept for future context-aware emit
}

// emit sends one payload chunk through the host bridge.
func emit(streamID string, payload []byte) error {
	req, err := json.Marshal(map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	if err != nil {
		return err
	}
	_, errCall := callHost("host.stream.emit", req)
	return errCall
}

func emitStreamKeepalive(streamID, format string) error {
	payload := []byte(": ping\n\n")
	if format == "claude" {
		payload = []byte("event: ping\ndata: {\"type\":\"ping\"}\n\n")
	}
	return emit(streamID, payload)
}

// streamOpenAI mirrors streamOpenAI in cmd/cursor-proxy but emits each
// SSE frame through the host stream bridge.
//
// Cursor emits assistant text through two channels: KV blobs that
// carry the assembled text so far, and text-delta interaction
// updates. We prefer KV blobs (they're the canonical wire shape) but
// fall through to text deltas when the server does not send blobs
// (e.g. legacy stream shape).
func streamOpenAI(streamID, model string, includeUsage bool, tools []executor.ToolDefinition, events <-chan executor.ChatEvent, errOut *string) {
	tr := translator.NewOpenAIStreamWriter(model)
	tr.IncludeUsage = includeUsage
	textState := assistantStreamState{}
	sawTurnEnd := false
	sawOutput := false
	var lastUsage *translator.Usage
	for ev := range events {
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			delta := textState.consumeSnapshot(blob.AssistantText)
			if delta != "" {
				sawOutput = true
				if err := emit(streamID, tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})); err != nil {
					*errOut = err.Error()
					return
				}
			}
			continue
		}
		trEv := translatePluginEvent(ev.Server, tools)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			if delta := textState.consumeDelta(trEv.Text); delta != "" {
				sawOutput = true
				if err := emit(streamID, tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})); err != nil {
					*errOut = err.Error()
					return
				}
			}
		case translator.EventThinkingDelta, translator.EventHeartbeat:
			if err := emitStreamKeepalive(streamID, "openai"); err != nil {
				*errOut = err.Error()
				return
			}
		case translator.EventToolCallStarted, translator.EventToolCallDelta:
			sawOutput = true
			if payload := tr.Encode(trEv); len(payload) > 0 {
				if err := emit(streamID, payload); err != nil {
					*errOut = err.Error()
					return
				}
			}
		case translator.EventTurnEnded:
			sawTurnEnd = true
			lastUsage = trEv.Usage
			if payload := tr.Encode(trEv); len(payload) > 0 {
				if err := emit(streamID, payload); err != nil {
					*errOut = err.Error()
					return
				}
			}
		}
	}
	if isEmptyPluginUpstreamResponse(sawOutput, lastUsage) {
		*errOut = emptyUpstreamResponseMessage
		return
	}
	// Synthetic tool_calls terminator when the server never sent
	// turn_ended — same rescue path as cursor-proxy's http handler.
	if !sawTurnEnd && tr.SawToolCall {
		if payload := tr.Encode(&translator.Event{Kind: translator.EventTurnEnded}); len(payload) > 0 {
			if err := emit(streamID, payload); err != nil {
				*errOut = err.Error()
				return
			}
		}
	}
	if payload := tr.FinalUsageFrame(); len(payload) > 0 {
		if err := emit(streamID, payload); err != nil {
			*errOut = err.Error()
			return
		}
	}
	if err := emit(streamID, tr.FinalDone()); err != nil {
		*errOut = err.Error()
		return
	}
}

// streamClaude mirrors streamAnthropic in cmd/cursor-proxy but emits
// SSE frames through the host stream bridge. See streamOpenAI for
// the KV-blob vs text-delta fallback rationale.
func streamClaude(streamID, model string, expectThinkingSignature bool, tools []executor.ToolDefinition, events <-chan executor.ChatEvent, errOut *string) {
	tr := translator.NewAnthropicStreamWriter(model)
	textState := assistantStreamState{}
	sawOutput := false
	signatureSent := false
	var pendingText strings.Builder
	var lastUsage *translator.Usage
	flushPendingText := func() bool {
		if pendingText.Len() == 0 {
			return true
		}
		payload := tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: pendingText.String()})
		pendingText.Reset()
		if len(payload) == 0 {
			return true
		}
		if err := emit(streamID, payload); err != nil {
			*errOut = err.Error()
			return false
		}
		return true
	}
	for ev := range events {
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil {
			if blob.AssistantText != "" {
				delta := textState.consumeSnapshot(blob.AssistantText)
				if delta != "" {
					sawOutput = true
					if expectThinkingSignature && !signatureSent {
						pendingText.WriteString(delta)
					} else if err := emit(streamID, tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})); err != nil {
						*errOut = err.Error()
						return
					}
				}
			}
			if expectThinkingSignature && !signatureSent && blob.Signature != "" {
				signatureSent = true
				if payload := tr.Encode(&translator.Event{Kind: translator.EventSignatureDelta, Text: blob.Signature}); len(payload) > 0 {
					if err := emit(streamID, payload); err != nil {
						*errOut = err.Error()
						return
					}
				}
				if !flushPendingText() {
					return
				}
			}
			continue
		}
		trEv := translatePluginEvent(ev.Server, tools)
		if trEv == nil {
			continue
		}
		switch trEv.Kind {
		case translator.EventTextDelta:
			if delta := textState.consumeDelta(trEv.Text); delta != "" {
				sawOutput = true
				if expectThinkingSignature && !signatureSent {
					pendingText.WriteString(delta)
				} else if err := emit(streamID, tr.Encode(&translator.Event{Kind: translator.EventTextDelta, Text: delta})); err != nil {
					*errOut = err.Error()
					return
				}
			}
		case translator.EventThinkingDelta:
			sawOutput = true
			if payload := tr.Encode(trEv); len(payload) > 0 {
				if err := emit(streamID, payload); err != nil {
					*errOut = err.Error()
					return
				}
			}
		case translator.EventHeartbeat:
			if err := emitStreamKeepalive(streamID, "claude"); err != nil {
				*errOut = err.Error()
				return
			}
		case translator.EventToolCallStarted, translator.EventToolCallDelta, translator.EventToolCallCompleted:
			sawOutput = true
			if payload := tr.Encode(trEv); len(payload) > 0 {
				if err := emit(streamID, payload); err != nil {
					*errOut = err.Error()
					return
				}
			}
		case translator.EventTurnEnded:
			lastUsage = trEv.Usage
		}
	}
	if isEmptyPluginUpstreamResponse(sawOutput, lastUsage) {
		*errOut = emptyUpstreamResponseMessage
		return
	}
	if !flushPendingText() {
		return
	}
	end := &translator.Event{Kind: translator.EventTurnEnded, Usage: lastUsage}
	if payload := tr.Encode(end); len(payload) > 0 {
		if err := emit(streamID, payload); err != nil {
			*errOut = err.Error()
			return
		}
	}
}

// handleExecutorCountTokens implements executor.count_tokens using a
// local character heuristic. The response mirrors the OpenAI usage
// shape so hosts that pattern-match on that field see a consistent
// key. See docs/phase-8b-abi.md for why we do not call Cursor's
// backend to count tokens.
func handleExecutorCountTokens(payload []byte) ([]byte, int) {
	var req executorRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse count request: %v", err), false), 0
	}
	if len(req.Payload) == 0 && len(req.OriginalRequest) > 0 {
		req.Payload = req.OriginalRequest
	}
	shape, err := parseByFormat(req.Payload, req.Format)
	if err != nil {
		return errorEnvelope("bad_payload", err.Error(), false), 0
	}
	var b strings.Builder
	if shape.SystemPrompt != "" {
		b.WriteString(shape.SystemPrompt)
		b.WriteByte('\n')
	}
	for _, turn := range shape.History {
		b.WriteString(turn.Content)
		b.WriteByte('\n')
	}
	b.WriteString(shape.UserMessage)
	tokens := countTokens(b.String())

	body, errMarshal := json.Marshal(map[string]any{
		"total_tokens": tokens,
		"usage": map[string]any{
			"prompt_tokens":     tokens,
			"completion_tokens": 0,
			"total_tokens":      tokens,
		},
	})
	if errMarshal != nil {
		return errorEnvelope("marshal_response", errMarshal.Error(), false), 0
	}
	resp := executorResponse{
		Payload: body,
		Headers: map[string][]string{"Content-Type": {"application/json"}},
	}
	buf, errMarshalResp := json.Marshal(resp)
	if errMarshalResp != nil {
		return errorEnvelope("marshal_response", errMarshalResp.Error(), false), 0
	}
	return okEnvelopeJSON(string(buf)), 0
}
