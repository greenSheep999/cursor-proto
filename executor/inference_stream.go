package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/router-for-me/cursor-proto/auth"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

const inferenceStreamPath = "/aiserver.v1.InferenceService/Stream"

// inferenceRequestFromAgentRun projects the agent envelope used by RunSSE
// onto the smaller request accepted by InferenceService/Stream. This mirrors
// SandClaimer's sand_rpc.js projection, while retaining history and declared
// MCP tools that the JS shim does not currently forward.
func inferenceRequestFromAgentRun(run *cursorpb.AgentV1_AgentRunRequest) (*cursorpb.AiserverV1_InferenceStreamRequest, error) {
	if run == nil {
		return nil, fmt.Errorf("agent run request is required")
	}

	requestedModel, err := inferenceRequestedModel(run)
	if err != nil {
		return nil, err
	}
	request := &cursorpb.AiserverV1_InferenceStreamRequest{
		RequestedModel: requestedModel,
	}
	if conversationID := strings.TrimSpace(run.GetConversationId()); conversationID != "" {
		request.ConversationId = &conversationID
	}
	if groupID := strings.TrimSpace(run.GetConversationGroupId()); groupID != "" {
		request.ConversationGroupId = &groupID
	}

	// SandClaimer supplies these defaults when the Agent requested-model
	// variant does not carry parameters. Keep the same wire shape so a direct
	// Grok probe tests the same model selection as the desktop shim.
	if len(requestedModel.Parameters) == 0 {
		request.RequestedModel.Parameters = []*cursorpb.AiserverV1_InferenceModelParameterValue{
			{Id: "effort", Value: "high"},
			{Id: "fast", Value: "true"},
		}
	}

	var messages []*cursorpb.AiserverV1_InferenceCoreMessage
	if systemPrompt := strings.TrimSpace(run.GetCustomSystemPrompt()); systemPrompt != "" {
		messages = append(messages, inferenceTextMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_SYSTEM, systemPrompt))
	}

	if action := run.GetAction(); action != nil {
		userAction := action.GetUserMessageAction()
		if userAction == nil {
			return nil, fmt.Errorf("inference stream does not support agent action %T", action.GetAction())
		}
		var historyMessages []*cursorpb.AiserverV1_InferenceCoreMessage
		historyMessages, err = inferenceMessagesFromHistory(userAction.GetConversationHistory())
		if err != nil {
			return nil, err
		}
		messages = append(messages, historyMessages...)

		for _, prepended := range userAction.GetPrependUserMessages() {
			if prepended == nil || strings.TrimSpace(prepended.GetText()) == "" {
				continue
			}
			messages = append(messages, inferenceTextMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_USER, prepended.GetText()))
		}

		if user := userAction.GetUserMessage(); user != nil {
			if user.GetSelectedContext() != nil {
				return nil, fmt.Errorf("inference stream does not support Agent selected_context; use run_sse")
			}
			if text := user.GetText(); strings.TrimSpace(text) != "" {
				messages = append(messages, inferenceTextMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_USER, text))
			}
		}
	}
	if len(messages) == 0 {
		// This is the exact fallback in SandClaimer's specFromAgent: a malformed
		// or empty Agent envelope still results in a valid inference request.
		messages = append(messages, inferenceTextMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_USER, "hello"))
	}
	request.Messages = messages

	if run.GetMcpTools() != nil {
		request.Tools, err = inferenceToolsFromAgent(run.GetMcpTools().GetMcpTools())
		if err != nil {
			return nil, err
		}
	} else if action := run.GetAction(); action != nil && action.GetUserMessageAction() != nil {
		request.Tools, err = inferenceToolsFromAgent(action.GetUserMessageAction().GetRequestContext().GetTools())
		if err != nil {
			return nil, err
		}
	}
	return request, nil
}

func inferenceRequestedModel(run *cursorpb.AgentV1_AgentRunRequest) (*cursorpb.AiserverV1_InferenceRequestedModel, error) {
	if model := run.GetRequestedModel(); model != nil {
		out := &cursorpb.AiserverV1_InferenceRequestedModel{
			ModelId:                       model.GetModelId(),
			MaxMode:                       model.GetMaxMode(),
			BuiltInModel:                  model.GetBuiltInModel(),
			IsVariantStringRepresentation: model.GetIsVariantStringRepresentation(),
		}
		for _, parameter := range model.GetParameters() {
			if parameter == nil || strings.TrimSpace(parameter.GetId()) == "" {
				continue
			}
			out.Parameters = append(out.Parameters, &cursorpb.AiserverV1_InferenceModelParameterValue{
				Id:    parameter.GetId(),
				Value: parameter.GetValue(),
			})
		}
		if strings.TrimSpace(out.ModelId) == "" {
			return nil, fmt.Errorf("requested model has no model_id")
		}
		return out, nil
	}

	details := run.GetModelDetails()
	if details == nil {
		return nil, fmt.Errorf("agent run request has no requested model or model_details")
	}
	modelID := strings.TrimSpace(details.GetModelId())
	if modelID == "" {
		modelID = strings.TrimSpace(details.GetDisplayModelId())
	}
	if modelID == "" {
		return nil, fmt.Errorf("model_details has no model_id")
	}
	maxMode := true
	if details.MaxMode != nil {
		maxMode = details.GetMaxMode()
	}
	return &cursorpb.AiserverV1_InferenceRequestedModel{
		ModelId:      modelID,
		MaxMode:      maxMode,
		BuiltInModel: true,
	}, nil
}

func inferenceTextMessage(role cursorpb.AiserverV1_InferenceMessageRole, text string) *cursorpb.AiserverV1_InferenceCoreMessage {
	return &cursorpb.AiserverV1_InferenceCoreMessage{
		Role:    role,
		Content: &cursorpb.AiserverV1_InferenceCoreMessage_Text{Text: text},
	}
}

func inferenceMessagesFromHistory(history *cursorpb.AgentV1_ConversationHistory) ([]*cursorpb.AiserverV1_InferenceCoreMessage, error) {
	if history == nil {
		return nil, nil
	}
	out := make([]*cursorpb.AiserverV1_InferenceCoreMessage, 0, len(history.GetMessages()))
	for i, message := range history.GetMessages() {
		if message == nil {
			continue
		}
		mapped, err := inferenceMessageFromHistory(message)
		if err != nil {
			return nil, fmt.Errorf("conversation history message %d: %w", i, err)
		}
		if mapped != nil {
			out = append(out, mapped)
		}
	}
	return out, nil
}

func inferenceMessageFromHistory(message *cursorpb.AgentV1_ConversationHistoryMessage) (*cursorpb.AiserverV1_InferenceCoreMessage, error) {
	switch {
	case message.GetUser() != nil:
		return inferenceUserHistoryMessage(message.GetUser())
	case message.GetAssistant() != nil:
		return inferenceAssistantHistoryMessage(message.GetAssistant())
	case message.GetTool() != nil:
		return inferenceToolHistoryMessage(message.GetTool())
	default:
		return nil, fmt.Errorf("message has no user, assistant, or tool body")
	}
}

func inferenceUserHistoryMessage(message *cursorpb.AgentV1_ConversationHistoryUserMessage) (*cursorpb.AiserverV1_InferenceCoreMessage, error) {
	var text strings.Builder
	parts := make([]*cursorpb.AiserverV1_InferenceContentPart, 0, len(message.GetContent()))
	for i, content := range message.GetContent() {
		if content == nil {
			continue
		}
		switch {
		case content.GetText() != nil:
			text.WriteString(content.GetText().GetText())
			parts = append(parts, &cursorpb.AiserverV1_InferenceContentPart{
				Part: &cursorpb.AiserverV1_InferenceContentPart_Text{
					Text: &cursorpb.AiserverV1_InferenceTextPart{Text: content.GetText().GetText()},
				},
			})
		case content.GetImage() != nil:
			image := content.GetImage()
			if image.GetData() == "" {
				return nil, fmt.Errorf("user content %d contains an image without data", i)
			}
			mime := image.GetMimeType()
			parts = append(parts, &cursorpb.AiserverV1_InferenceContentPart{
				Part: &cursorpb.AiserverV1_InferenceContentPart_Image{
					Image: &cursorpb.AiserverV1_InferenceImagePart{Data: image.GetData(), MimeType: optionalString(mime)},
				},
			})
		default:
			return nil, fmt.Errorf("user content %d has unsupported content %T", i, content.GetContent())
		}
	}
	return inferenceContentMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_USER, text.String(), parts), nil
}

func inferenceAssistantHistoryMessage(message *cursorpb.AgentV1_ConversationHistoryAssistantMessage) (*cursorpb.AiserverV1_InferenceCoreMessage, error) {
	var text strings.Builder
	parts := make([]*cursorpb.AiserverV1_InferenceContentPart, 0, len(message.GetContent()))
	out := &cursorpb.AiserverV1_InferenceCoreMessage{Role: cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_ASSISTANT}
	for i, content := range message.GetContent() {
		if content == nil {
			continue
		}
		switch {
		case content.GetText() != nil:
			value := content.GetText().GetText()
			text.WriteString(value)
			parts = append(parts, &cursorpb.AiserverV1_InferenceContentPart{
				Part: &cursorpb.AiserverV1_InferenceContentPart_Text{
					Text: &cursorpb.AiserverV1_InferenceTextPart{Text: value},
				},
			})
		case content.GetReasoning() != nil:
			reasoning := content.GetReasoning()
			out.ReasoningParts = append(out.ReasoningParts, &cursorpb.AiserverV1_InferenceReasoningPart{
				Text:       reasoning.GetText(),
				Signature:  optionalString(reasoning.GetSignature()),
				IsRedacted: false,
				ModelName:  nil,
			})
		case content.GetRedactedReasoning() != nil:
			redacted := content.GetRedactedReasoning()
			out.ReasoningParts = append(out.ReasoningParts, &cursorpb.AiserverV1_InferenceReasoningPart{
				IsRedacted:   true,
				RedactedData: optionalString(redacted.GetData()),
			})
		case content.GetToolCall() != nil:
			toolCall := content.GetToolCall()
			out.ToolCalls = append(out.ToolCalls, &cursorpb.AiserverV1_InferenceToolCall{
				ToolCallId: toolCall.GetToolCallId(),
				ToolName:   toolCall.GetToolName(),
				Args:       []byte(toolCall.GetArgsJson()),
			})
		default:
			return nil, fmt.Errorf("assistant content %d has unsupported content %T", i, content.GetContent())
		}
	}
	if text.Len() > 0 || len(parts) > 0 {
		content := inferenceContentMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_ASSISTANT, text.String(), parts)
		out.Content = content.Content
	}
	return out, nil
}

func inferenceToolHistoryMessage(message *cursorpb.AgentV1_ConversationHistoryToolMessage) (*cursorpb.AiserverV1_InferenceCoreMessage, error) {
	out := &cursorpb.AiserverV1_InferenceCoreMessage{Role: cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_TOOL}
	parts := make([]*cursorpb.AiserverV1_InferenceToolResultPart, 0, len(message.GetContent()))
	for i, content := range message.GetContent() {
		if content == nil {
			continue
		}
		part := &cursorpb.AiserverV1_InferenceToolResultPart{
			ToolCallId: message.GetToolCallId(),
			ToolName:   message.GetToolName(),
			IsError:    message.GetIsError(),
		}
		switch {
		case content.GetText() != nil:
			part.Result = []byte(content.GetText().GetText())
		case content.GetImage() != nil:
			part.Result = []byte(content.GetImage().GetData())
		default:
			return nil, fmt.Errorf("tool content %d has unsupported content %T", i, content.GetContent())
		}
		parts = append(parts, part)
	}
	if len(parts) > 0 {
		out.Content = &cursorpb.AiserverV1_InferenceCoreMessage_ToolContent{
			ToolContent: &cursorpb.AiserverV1_InferenceToolResultContent{Parts: parts},
		}
	}
	return out, nil
}

func inferenceContentMessage(role cursorpb.AiserverV1_InferenceMessageRole, text string, parts []*cursorpb.AiserverV1_InferenceContentPart) *cursorpb.AiserverV1_InferenceCoreMessage {
	message := &cursorpb.AiserverV1_InferenceCoreMessage{Role: role}
	rich := false
	for _, part := range parts {
		if part != nil && (part.GetImage() != nil || part.GetFile() != nil) {
			rich = true
			break
		}
	}
	if rich {
		message.Content = &cursorpb.AiserverV1_InferenceCoreMessage_Parts{Parts: &cursorpb.AiserverV1_InferenceContentParts{Parts: parts}}
	} else {
		message.Content = &cursorpb.AiserverV1_InferenceCoreMessage_Text{Text: text}
	}
	return message
}

func inferenceToolsFromAgent(tools []*cursorpb.AgentV1_McpToolDefinition) ([]*cursorpb.AiserverV1_InferenceAgentTool, error) {
	if len(tools) == 0 {
		return nil, nil
	}
	out := make([]*cursorpb.AiserverV1_InferenceAgentTool, 0, len(tools))
	for i, tool := range tools {
		if tool == nil {
			continue
		}
		name := strings.TrimSpace(tool.GetToolName())
		if name == "" {
			name = strings.TrimSpace(tool.GetName())
		}
		if name == "" {
			return nil, fmt.Errorf("MCP tool %d has no name", i)
		}
		parameters := append([]byte(nil), tool.GetInputSchema()...)
		if len(parameters) == 0 && tool.GetInputSchemaJson() != "" {
			parameters = []byte(tool.GetInputSchemaJson())
		}
		out = append(out, &cursorpb.AiserverV1_InferenceAgentTool{
			Name:        name,
			Description: tool.GetDescription(),
			Parameters:  parameters,
		})
	}
	return out, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// runInferenceStream sends one Connect-framed request and returns a decoded
// stream. It is deliberately opt-in through ChatRequest.RPCMode because its
// quota/billing bucket is still being measured against RunSSE.
func (c *Client) runInferenceStream(ctx context.Context, req *ChatRequest, acc *auth.Account, requestID, runID string, inferenceReq *cursorpb.AiserverV1_InferenceStreamRequest) (<-chan ChatEvent, error) {
	if inferenceReq == nil {
		return nil, fmt.Errorf("inference stream request is required")
	}
	payload, err := proto.Marshal(inferenceReq)
	if err != nil {
		return nil, fmt.Errorf("marshal InferenceStreamRequest: %w", err)
	}
	url := strings.TrimRight(c.API3, "/") + inferenceStreamPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(addConnectEnvelope(payload, false)))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("content-type", "application/grpc-web+proto")
	httpReq.Header.Set("accept", "application/grpc-web+proto")
	ApplyCommonHeadersWithClientType(httpReq, acc, requestID, req.ClientTypeOverride)
	c.applySidecarToken(httpReq)
	httpReq.Header.Set("x-original-request-id", runID)

	resp, err := c.NewStreamClient().Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("InferenceService/Stream dial: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, readErr := readBody(resp)
		resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("InferenceService/Stream http %d: read body: %w", resp.StatusCode, readErr)
		}
		return nil, fmt.Errorf("InferenceService/Stream http %d: %s", resp.StatusCode, string(body))
	}

	events := make(chan ChatEvent, 32)
	go readInferenceStream(ctx, resp.Body, events)
	return events, nil
}

type inferenceStreamState struct {
	toolStarted  map[string]bool
	toolComplete map[string]bool
	toolArgs     map[string]string
	sawTurnEnded bool
	sawError     bool
}

func newInferenceStreamState() *inferenceStreamState {
	return &inferenceStreamState{
		toolStarted:  make(map[string]bool),
		toolComplete: make(map[string]bool),
		toolArgs:     make(map[string]string),
	}
}

func readInferenceStream(ctx context.Context, body io.ReadCloser, out chan<- ChatEvent) {
	defer close(out)
	defer body.Close()

	stopCancelWatcher := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = body.Close()
		case <-stopCancelWatcher:
		}
	}()
	defer close(stopCancelWatcher)

	state := newInferenceStreamState()
	buf := make([]byte, 0, 8192)
	tmp := make([]byte, 4096)
	emitStatus := func(status *TrailerStatus, raw []byte) {
		state.sawError = status != nil && !status.OK()
		out <- ChatEvent{Trailer: true, Raw: append([]byte(nil), raw...), Status: status}
	}
	emitSyntheticTurnEnd := func() {
		if state.sawTurnEnded || state.sawError {
			return
		}
		state.sawTurnEnded = true
		out <- inferenceTurnEndedEvent(0, 0, 0, 0)
	}

	for {
		n, readErr := body.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for len(buf) > 0 {
				flags := buf[0]
				payload, isTrailer, rest, ok := splitConnectFrame(buf)
				if !ok {
					break
				}
				payload = append([]byte(nil), payload...)
				buf = append(buf[:0], rest...)
				if isTrailer {
					status := ParseTrailer(payload)
					if status == nil {
						status = &TrailerStatus{Code: 13, Message: "empty InferenceService/Stream trailer"}
					}
					if status.OK() {
						emitSyntheticTurnEnd()
					}
					emitStatus(status, payload)
					return
				}
				if flags&0x01 != 0 {
					var err error
					payload, err = gunzipConnectPayload(payload)
					if err != nil {
						emitStatus(&TrailerStatus{Code: 13, Message: "decode compressed InferenceService/Stream frame: " + err.Error()}, payload)
						return
					}
				}

				response := &cursorpb.AiserverV1_InferenceStreamResponse{}
				if err := proto.Unmarshal(payload, response); err != nil {
					emitStatus(&TrailerStatus{Code: 13, Message: "decode InferenceStreamResponse: " + err.Error()}, payload)
					return
				}
				events, err := inferenceResponseEvents(response, state, payload)
				if err != nil {
					emitStatus(&TrailerStatus{Code: 13, Message: err.Error()}, payload)
					return
				}
				for _, event := range events {
					if event.Server != nil && event.Server.GetInteractionUpdate().GetTurnEnded() != nil {
						state.sawTurnEnded = true
					}
					if event.Trailer && event.Status != nil && !event.Status.OK() {
						state.sawError = true
					}
					out <- event
				}
			}
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return
			}
			if readErr == io.EOF && len(buf) == 0 {
				emitSyntheticTurnEnd()
				return
			}
			if readErr == io.EOF {
				emitStatus(&TrailerStatus{Code: 13, Message: "truncated InferenceService/Stream frame"}, buf)
				return
			}
			emitStatus(&TrailerStatus{Code: 13, Message: "read InferenceService/Stream: " + readErr.Error()}, nil)
			return
		}
	}
}

func gunzipConnectPayload(payload []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

// inferenceResponseEvents converts one direct-stream response into the
// AgentServerMessage shape already consumed by the proxy translators.
func inferenceResponseEvents(response *cursorpb.AiserverV1_InferenceStreamResponse, state *inferenceStreamState, raw []byte) ([]ChatEvent, error) {
	if response == nil {
		return nil, fmt.Errorf("nil InferenceStreamResponse")
	}
	withRaw := func(event ChatEvent) ChatEvent {
		event.Raw = append([]byte(nil), raw...)
		return event
	}
	textEvent := func(text string) ChatEvent {
		return withRaw(ChatEvent{Server: &cursorpb.AgentV1_AgentServerMessage{
			Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
					Message: &cursorpb.AgentV1_InteractionUpdate_TextDelta{
						TextDelta: &cursorpb.AgentV1_TextDeltaUpdate{Text: text},
					},
				},
			},
		}})
	}
	thinkingEvent := func(text string) ChatEvent {
		return withRaw(ChatEvent{Server: &cursorpb.AgentV1_AgentServerMessage{
			Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
					Message: &cursorpb.AgentV1_InteractionUpdate_ThinkingDelta{
						ThinkingDelta: &cursorpb.AgentV1_ThinkingDeltaUpdate{Text: text},
					},
				},
			},
		}})
	}

	if part := response.GetTextPart(); part != nil {
		if part.GetText() == "" {
			return nil, nil
		}
		return []ChatEvent{textEvent(part.GetText())}, nil
	}
	if part := response.GetThinkingPart(); part != nil {
		if part.GetText() == "" {
			return nil, nil
		}
		return []ChatEvent{thinkingEvent(part.GetText())}, nil
	}
	if usage := response.GetUsage(); usage != nil {
		return []ChatEvent{withRaw(inferenceTurnEndedEvent(
			int64(usage.GetPromptTokens()), int64(usage.GetCompletionTokens()), 0, 0,
		))}, nil
	}
	if usage := response.GetExtendedUsage(); usage != nil {
		return []ChatEvent{withRaw(inferenceTurnEndedEvent(
			int64(usage.GetInputTokens()), int64(usage.GetOutputTokens()), int64(usage.GetCacheReadTokens()), int64(usage.GetCacheWriteTokens()),
		))}, nil
	}
	if part := response.GetToolCallPart(); part != nil {
		return inferenceToolCallPartEvents(part, state, raw), nil
	}
	if info := response.GetResponseInfo(); info != nil {
		return inferenceResponseInfoEvents(info, state, raw), nil
	}
	if streamError := response.GetError(); streamError != nil {
		status := inferenceStreamErrorStatus(streamError)
		state.sawError = true
		return []ChatEvent{withRaw(ChatEvent{Trailer: true, Status: status})}, nil
	}
	// Metadata, invocation ids, and image-description responses have no
	// equivalent AgentServerMessage. Preserve their raw protobuf frame so a
	// future caller can inspect it instead of silently discarding it.
	return []ChatEvent{withRaw(ChatEvent{})}, nil
}

func inferenceTurnEndedEvent(input, output, cacheRead, cacheWrite int64) ChatEvent {
	return ChatEvent{Server: &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_TurnEnded{
					TurnEnded: &cursorpb.AgentV1_TurnEndedUpdate{
						InputTokens:      ptr(input),
						OutputTokens:     ptr(output),
						CacheReadTokens:  ptr(cacheRead),
						CacheWriteTokens: ptr(cacheWrite),
					},
				},
			},
		},
	}}
}

func inferenceResponseInfoEvents(info *cursorpb.AiserverV1_InferenceResponseInfo, state *inferenceStreamState, raw []byte) []ChatEvent {
	var out []ChatEvent
	for _, message := range info.GetMessages() {
		if message == nil {
			continue
		}
		if content := message.GetContent(); content != "" {
			out = append(out, ChatEvent{Raw: append([]byte(nil), raw...), Server: &cursorpb.AgentV1_AgentServerMessage{
				Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
					InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
						Message: &cursorpb.AgentV1_InteractionUpdate_TextDelta{TextDelta: &cursorpb.AgentV1_TextDeltaUpdate{Text: content}},
					},
				},
			}})
		}
		for _, reasoning := range message.GetReasoningParts() {
			if reasoning == nil || reasoning.GetText() == "" {
				continue
			}
			out = append(out, ChatEvent{Raw: append([]byte(nil), raw...), Server: &cursorpb.AgentV1_AgentServerMessage{
				Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
					InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
						Message: &cursorpb.AgentV1_InteractionUpdate_ThinkingDelta{ThinkingDelta: &cursorpb.AgentV1_ThinkingDeltaUpdate{Text: reasoning.GetText()}},
					},
				},
			}})
		}
		for _, toolCall := range message.GetToolCalls() {
			if toolCall == nil {
				continue
			}
			part := &cursorpb.AiserverV1_InferenceToolCallStreamPart{
				ToolCallId: toolCall.GetToolCallId(),
				ToolName:   toolCall.GetToolName(),
				Args:       string(toolCall.GetArgs()),
				IsComplete: true,
			}
			out = append(out, inferenceToolCallPartEvents(part, state, raw)...)
		}
	}
	if info.GetErrorMessage() != "" {
		out = append(out, ChatEvent{Trailer: true, Raw: append([]byte(nil), raw...), Status: &TrailerStatus{Code: 13, Message: info.GetErrorMessage()}})
		state.sawError = true
	}
	return out
}

func inferenceToolCallPartEvents(part *cursorpb.AiserverV1_InferenceToolCallStreamPart, state *inferenceStreamState, raw []byte) []ChatEvent {
	if part == nil {
		return nil
	}
	id := strings.TrimSpace(part.GetToolCallId())
	if id == "" {
		if part.ToolIndex != nil {
			id = "inference-tool-" + strconv.Itoa(int(part.GetToolIndex()))
		} else {
			id = "inference-tool-0"
		}
	}
	name := strings.TrimSpace(part.GetToolName())
	if name == "" {
		name = "unknown"
	}
	args := part.GetArgs()
	state.toolArgs[id] += args
	fullArgs := state.toolArgs[id]

	var out []ChatEvent
	if !state.toolStarted[id] {
		state.toolStarted[id] = true
		toolCall, valid := inferenceAgentToolCall(id, name, fullArgs)
		out = append(out, ChatEvent{Raw: append([]byte(nil), raw...), Server: &cursorpb.AgentV1_AgentServerMessage{
			Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
					Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallStarted{
						ToolCallStarted: &cursorpb.AgentV1_ToolCallStartedUpdate{CallId: id, ToolCall: toolCall},
					},
				},
			},
		}})
		if !valid && args != "" {
			out = append(out, inferencePartialToolCallEvent(id, args, raw))
		}
	} else if args != "" {
		out = append(out, inferencePartialToolCallEvent(id, args, raw))
	}
	if part.GetIsComplete() && !state.toolComplete[id] {
		state.toolComplete[id] = true
		toolCall, _ := inferenceAgentToolCall(id, name, fullArgs)
		out = append(out, ChatEvent{Raw: append([]byte(nil), raw...), Server: &cursorpb.AgentV1_AgentServerMessage{
			Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
				InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
					Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallCompleted{
						ToolCallCompleted: &cursorpb.AgentV1_ToolCallCompletedUpdate{CallId: id, ToolCall: toolCall},
					},
				},
			},
		}})
	}
	return out
}

func inferencePartialToolCallEvent(id, args string, raw []byte) ChatEvent {
	return ChatEvent{Raw: append([]byte(nil), raw...), Server: &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_PartialToolCall{
					PartialToolCall: &cursorpb.AgentV1_PartialToolCallUpdate{CallId: id, ArgsTextDelta: args},
				},
			},
		},
	}}
}

func inferenceAgentToolCall(id, name, args string) (*cursorpb.AgentV1_ToolCall, bool) {
	mcpArgs, valid := inferenceMcpArgs(id, name, args)
	return &cursorpb.AgentV1_ToolCall{
		ToolCallId: ptr(id),
		Tool: &cursorpb.AgentV1_ToolCall_McpToolCall{
			McpToolCall: &cursorpb.AgentV1_McpToolCall{Args: mcpArgs},
		},
	}, valid
}

func inferenceMcpArgs(id, name, raw string) (*cursorpb.AgentV1_McpArgs, bool) {
	args := &cursorpb.AgentV1_McpArgs{ToolCallId: id, Name: name, ToolName: name}
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return args, true
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(trimmed), &object); err != nil {
		return args, false
	}
	args.Args = make(map[string][]byte, len(object))
	for key, encoded := range object {
		var value any
		if err := json.Unmarshal(encoded, &value); err != nil {
			return args, false
		}
		protoValue, err := structpb.NewValue(value)
		if err != nil {
			return args, false
		}
		valueBytes, err := proto.Marshal(protoValue)
		if err != nil {
			return args, false
		}
		args.Args[key] = valueBytes
	}
	return args, true
}

func inferenceStreamErrorStatus(streamError *cursorpb.AiserverV1_InferenceStreamError) *TrailerStatus {
	if streamError == nil {
		return &TrailerStatus{Code: 13, Message: "unknown InferenceService/Stream error"}
	}
	codeText := strings.TrimSpace(streamError.GetCode())
	code := 0
	if parsed, err := strconv.Atoi(codeText); err == nil {
		code = parsed
	}
	if code == 0 {
		normalized := strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(codeText, "-", "_"), " ", "_"))
		switch normalized {
		case "unauthenticated", "authentication", "authentication_error":
			code = 16
		case "permission_denied", "permission", "forbidden":
			code = 7
		case "resource_exhausted", "rate_limit", "rate_limited", "output_token_limit", "overloaded":
			code = 8
		case "invalid_argument", "input_token_limit", "content_filter":
			code = 3
		case "unavailable":
			code = 14
		default:
			code = 13
		}
	}
	if code == 0 {
		code = 13
	}
	message := streamError.GetMessage_()
	if message == "" {
		message = codeText
	}
	if message == "" {
		message = streamError.GetErrorType().String()
	}
	return &TrailerStatus{Code: code, Message: message}
}
