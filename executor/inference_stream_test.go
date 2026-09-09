package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/cursor-proto/auth"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

func TestInferenceRequestFromAgentRunProjectsHistoryModelAndTools(t *testing.T) {
	conversationID := "conversation-123"
	groupID := "group-456"
	customSystemPrompt := "be concise"
	maxMode := true
	run := &cursorpb.AgentV1_AgentRunRequest{
		ConversationId:      &conversationID,
		ConversationGroupId: &groupID,
		CustomSystemPrompt:  &customSystemPrompt,
		RequestedModel:      &cursorpb.AgentV1_RequestedModel{ModelId: "grok-4.6", MaxMode: true, BuiltInModel: true, IsVariantStringRepresentation: true, Parameters: []*cursorpb.AgentV1_RequestedModel_ModelParameterValue{{Id: "temperature", Value: "0.2"}}},
		ModelDetails:        &cursorpb.AgentV1_ModelDetails{ModelId: "ignored-model", MaxMode: &maxMode},
		McpTools:            &cursorpb.AgentV1_McpTools{McpTools: []*cursorpb.AgentV1_McpToolDefinition{{ToolName: "lookup", Description: "look things up", InputSchema: []byte(`{"type":"object"}`)}}},
		Action: &cursorpb.AgentV1_ConversationAction{Action: &cursorpb.AgentV1_ConversationAction_UserMessageAction{UserMessageAction: &cursorpb.AgentV1_UserMessageAction{
			ConversationHistory: &cursorpb.AgentV1_ConversationHistory{Messages: []*cursorpb.AgentV1_ConversationHistoryMessage{
				{Message: &cursorpb.AgentV1_ConversationHistoryMessage_User{User: &cursorpb.AgentV1_ConversationHistoryUserMessage{Content: []*cursorpb.AgentV1_ConversationHistoryUserContent{{Content: &cursorpb.AgentV1_ConversationHistoryUserContent_Text{Text: &cursorpb.AgentV1_ConversationHistoryTextContent{Text: "old user"}}}}}}},
				{Message: &cursorpb.AgentV1_ConversationHistoryMessage_Assistant{Assistant: &cursorpb.AgentV1_ConversationHistoryAssistantMessage{Content: []*cursorpb.AgentV1_ConversationHistoryAssistantContent{{Content: &cursorpb.AgentV1_ConversationHistoryAssistantContent_Text{Text: &cursorpb.AgentV1_ConversationHistoryTextContent{Text: "old assistant"}}}, {Content: &cursorpb.AgentV1_ConversationHistoryAssistantContent_Reasoning{Reasoning: &cursorpb.AgentV1_ConversationHistoryReasoningContent{Text: "because"}}}}}}},
				{Message: &cursorpb.AgentV1_ConversationHistoryMessage_Tool{Tool: &cursorpb.AgentV1_ConversationHistoryToolMessage{ToolCallId: "call-1", ToolName: "lookup", Content: []*cursorpb.AgentV1_ConversationHistoryToolResultContent{{Content: &cursorpb.AgentV1_ConversationHistoryToolResultContent_Text{Text: &cursorpb.AgentV1_ConversationHistoryTextContent{Text: "tool result"}}}}}}},
			}},
			PrependUserMessages: []*cursorpb.AgentV1_UserMessage{{Text: "prepended"}},
			UserMessage:         &cursorpb.AgentV1_UserMessage{Text: "current"},
		}}},
	}

	got, err := inferenceRequestFromAgentRun(run)
	if err != nil {
		t.Fatalf("inferenceRequestFromAgentRun: %v", err)
	}
	if got.GetConversationId() != conversationID || got.GetConversationGroupId() != groupID {
		t.Fatalf("conversation ids = %q/%q", got.GetConversationId(), got.GetConversationGroupId())
	}
	model := got.GetRequestedModel()
	if model == nil || model.GetModelId() != "grok-4.6" || !model.GetMaxMode() || !model.GetBuiltInModel() || !model.GetIsVariantStringRepresentation() {
		t.Fatalf("requested model = %v", model)
	}
	if len(model.GetParameters()) != 1 || model.GetParameters()[0].GetId() != "temperature" || model.GetParameters()[0].GetValue() != "0.2" {
		t.Fatalf("model parameters = %v", model.GetParameters())
	}
	if len(got.GetMessages()) != 6 {
		t.Fatalf("message count = %d, want 6", len(got.GetMessages()))
	}
	if got.GetMessages()[0].GetRole() != cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_SYSTEM || got.GetMessages()[0].GetText() != customSystemPrompt {
		t.Fatalf("system message = %v", got.GetMessages()[0])
	}
	if got.GetMessages()[1].GetText() != "old user" || got.GetMessages()[2].GetText() != "old assistant" {
		t.Fatalf("text history = %v, %v", got.GetMessages()[1], got.GetMessages()[2])
	}
	if len(got.GetMessages()[2].GetReasoningParts()) != 1 || got.GetMessages()[2].GetReasoningParts()[0].GetText() != "because" {
		t.Fatalf("assistant reasoning = %v", got.GetMessages()[2].GetReasoningParts())
	}
	toolMessage := got.GetMessages()[3]
	if len(toolMessage.GetToolContent().GetParts()) != 1 || string(toolMessage.GetToolContent().GetParts()[0].GetResult()) != "tool result" {
		t.Fatalf("tool history = %v", toolMessage)
	}
	if got.GetMessages()[4].GetText() != "prepended" || got.GetMessages()[5].GetText() != "current" {
		t.Fatalf("current messages = %v, %v", got.GetMessages()[4], got.GetMessages()[5])
	}
	if len(got.GetTools()) != 1 || got.GetTools()[0].GetName() != "lookup" || string(got.GetTools()[0].GetParameters()) != `{"type":"object"}` {
		t.Fatalf("tools = %v", got.GetTools())
	}
}

func TestInferenceRequestDefaultsAndModelDetailsFallback(t *testing.T) {
	run := &cursorpb.AgentV1_AgentRunRequest{ModelDetails: &cursorpb.AgentV1_ModelDetails{DisplayModelId: "composer-2.5"}}
	got, err := inferenceRequestFromAgentRun(run)
	if err != nil {
		t.Fatalf("inferenceRequestFromAgentRun: %v", err)
	}
	model := got.GetRequestedModel()
	if model.GetModelId() != "composer-2.5" || !model.GetMaxMode() || !model.GetBuiltInModel() {
		t.Fatalf("fallback model = %v", model)
	}
	if len(model.GetParameters()) != 2 || model.GetParameters()[0].GetId() != "effort" || model.GetParameters()[0].GetValue() != "high" || model.GetParameters()[1].GetId() != "fast" || model.GetParameters()[1].GetValue() != "true" {
		t.Fatalf("default parameters = %v", model.GetParameters())
	}
	if len(got.GetMessages()) != 1 || got.GetMessages()[0].GetText() != "hello" {
		t.Fatalf("empty request fallback = %v", got.GetMessages())
	}
}

func TestInferenceRequestRejectsSelectedContext(t *testing.T) {
	run := &cursorpb.AgentV1_AgentRunRequest{
		RequestedModel: &cursorpb.AgentV1_RequestedModel{ModelId: "grok-4.6"},
		Action: &cursorpb.AgentV1_ConversationAction{Action: &cursorpb.AgentV1_ConversationAction_UserMessageAction{UserMessageAction: &cursorpb.AgentV1_UserMessageAction{
			UserMessage: &cursorpb.AgentV1_UserMessage{Text: "current", SelectedContext: &cursorpb.AgentV1_SelectedContext{}},
		}}},
	}
	_, err := inferenceRequestFromAgentRun(run)
	if err == nil || !strings.Contains(err.Error(), "selected_context") {
		t.Fatalf("error = %v, want selected_context rejection", err)
	}
}

func TestInferenceContentMessageUsesTextUnlessRich(t *testing.T) {
	text := inferenceContentMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_USER, "hello", []*cursorpb.AiserverV1_InferenceContentPart{{Part: &cursorpb.AiserverV1_InferenceContentPart_Text{Text: &cursorpb.AiserverV1_InferenceTextPart{Text: "hello"}}}})
	if text.GetText() != "hello" || text.GetParts() != nil {
		t.Fatalf("text content = %v", text)
	}
	rich := inferenceContentMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_USER, "caption", []*cursorpb.AiserverV1_InferenceContentPart{{Part: &cursorpb.AiserverV1_InferenceContentPart_Text{Text: &cursorpb.AiserverV1_InferenceTextPart{Text: "caption"}}}, {Part: &cursorpb.AiserverV1_InferenceContentPart_Image{Image: &cursorpb.AiserverV1_InferenceImagePart{Data: "base64", MimeType: ptr("image/png")}}}})
	if rich.GetParts() == nil || len(rich.GetParts().GetParts()) != 2 || rich.GetText() != "" {
		t.Fatalf("rich content = %v", rich)
	}
}

func TestRunInferenceStreamUsesConnectPathBodyAndSandHeaders(t *testing.T) {
	account := &auth.Account{AccessToken: "session-token", Email: "test@example.com"}
	var received *cursorpb.AiserverV1_InferenceStreamRequest
	var receivedRequest *http.Request
	response := &cursorpb.AiserverV1_InferenceStreamResponse{Response: &cursorpb.AiserverV1_InferenceStreamResponse_TextPart{TextPart: &cursorpb.AiserverV1_InferenceTextStreamPart{Text: "hello"}}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedRequest = r
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read request: %v", err)
		}
		payload, trailer, rest, ok := splitConnectFrame(body)
		if !ok || trailer || len(rest) != 0 {
			t.Fatalf("request frame = ok=%v trailer=%v rest=%d", ok, trailer, len(rest))
		}
		received = &cursorpb.AiserverV1_InferenceStreamRequest{}
		if err := proto.Unmarshal(payload, received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("content-type", "application/grpc-web+proto")
		_, _ = w.Write(addConnectEnvelope(mustMarshal(t, response), false))
	}))
	defer server.Close()

	client := NewClient(account)
	client.API3 = server.URL
	events, err := client.runInferenceStream(context.Background(), &ChatRequest{ClientTypeOverride: "sand"}, account, "request-id", "run-id", &cursorpb.AiserverV1_InferenceStreamRequest{RequestedModel: &cursorpb.AiserverV1_InferenceRequestedModel{ModelId: "grok-4.6"}, Messages: []*cursorpb.AiserverV1_InferenceCoreMessage{inferenceTextMessage(cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_USER, "probe")}})
	if err != nil {
		t.Fatalf("runInferenceStream: %v", err)
	}
	var got []ChatEvent
	for event := range events {
		got = append(got, event)
	}
	if receivedRequest == nil || receivedRequest.URL.Path != inferenceStreamPath {
		t.Fatalf("request path = %v, want %s", receivedRequest.URL, inferenceStreamPath)
	}
	if receivedRequest.Header.Get("content-type") != "application/grpc-web+proto" || receivedRequest.Header.Get("x-cursor-client-type") != "sand" || receivedRequest.Header.Get("x-cursor-client-source") != "" || receivedRequest.Header.Get("x-cursor-client-version") != SandClientVersion || receivedRequest.Header.Get("x-sand-box-namespace") != "prod" {
		t.Fatalf("headers = %#v", receivedRequest.Header)
	}
	if received == nil || received.GetRequestedModel().GetModelId() != "grok-4.6" || received.GetMessages()[0].GetText() != "probe" {
		t.Fatalf("received request = %v", received)
	}
	if len(got) != 2 || got[0].Server.GetInteractionUpdate().GetTextDelta().GetText() != "hello" || got[1].Server.GetInteractionUpdate().GetTurnEnded() == nil {
		t.Fatalf("events = %#v", got)
	}
}

func TestRunInferenceStreamRestoresIDEVersionForExplicitIDEOverride(t *testing.T) {
	account := &auth.Account{AccessToken: "session-token", ClientType: "sand"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-cursor-client-type") != "ide" || r.Header.Get("x-cursor-client-version") != CursorClientVersion || r.Header.Get("x-sand-box-namespace") != "" {
			t.Errorf("headers = %#v", r.Header)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := NewClient(account)
	client.API3 = server.URL
	_, err := client.runInferenceStream(context.Background(), &ChatRequest{ClientTypeOverride: "ide"}, account, "request-id", "run-id", &cursorpb.AiserverV1_InferenceStreamRequest{})
	if err != nil {
		t.Fatalf("runInferenceStream: %v", err)
	}
}

func TestInferenceResponseEventsAndToolCompletion(t *testing.T) {
	state := newInferenceStreamState()
	textEvents, err := inferenceResponseEvents(&cursorpb.AiserverV1_InferenceStreamResponse{Response: &cursorpb.AiserverV1_InferenceStreamResponse_TextPart{TextPart: &cursorpb.AiserverV1_InferenceTextStreamPart{Text: "text"}}}, state, []byte("raw"))
	if err != nil || len(textEvents) != 1 || textEvents[0].Server.GetInteractionUpdate().GetTextDelta().GetText() != "text" {
		t.Fatalf("text events = %#v, err=%v", textEvents, err)
	}
	thinkingEvents, err := inferenceResponseEvents(&cursorpb.AiserverV1_InferenceStreamResponse{Response: &cursorpb.AiserverV1_InferenceStreamResponse_ThinkingPart{ThinkingPart: &cursorpb.AiserverV1_InferenceThinkingStreamPart{Text: "think"}}}, state, nil)
	if err != nil || len(thinkingEvents) != 1 || thinkingEvents[0].Server.GetInteractionUpdate().GetThinkingDelta().GetText() != "think" {
		t.Fatalf("thinking events = %#v, err=%v", thinkingEvents, err)
	}
	usageEvents, err := inferenceResponseEvents(&cursorpb.AiserverV1_InferenceStreamResponse{Response: &cursorpb.AiserverV1_InferenceStreamResponse_ExtendedUsage{ExtendedUsage: &cursorpb.AiserverV1_InferenceExtendedUsageInfo{InputTokens: 11, OutputTokens: 7, CacheReadTokens: 3, CacheWriteTokens: 2}}}, state, nil)
	if err != nil || len(usageEvents) != 1 || usageEvents[0].Server.GetInteractionUpdate().GetTurnEnded().GetInputTokens() != 11 {
		t.Fatalf("usage events = %#v, err=%v", usageEvents, err)
	}
	toolEvents, err := inferenceResponseEvents(&cursorpb.AiserverV1_InferenceStreamResponse{Response: &cursorpb.AiserverV1_InferenceStreamResponse_ToolCallPart{ToolCallPart: &cursorpb.AiserverV1_InferenceToolCallStreamPart{ToolCallId: "call-1", ToolName: "lookup", Args: `{"q":"x"}`, IsComplete: true}}}, state, nil)
	if err != nil || len(toolEvents) != 2 || toolEvents[0].Server.GetInteractionUpdate().GetToolCallStarted() == nil || toolEvents[1].Server.GetInteractionUpdate().GetToolCallCompleted() == nil {
		t.Fatalf("tool events = %#v, err=%v", toolEvents, err)
	}
	if toolEvents[1].Server.GetInteractionUpdate().GetToolCallCompleted().GetToolCall().GetMcpToolCall().GetArgs().GetName() != "lookup" {
		t.Fatalf("completed tool = %v", toolEvents[1].Server.GetInteractionUpdate().GetToolCallCompleted())
	}
	errorEvents, err := inferenceResponseEvents(&cursorpb.AiserverV1_InferenceStreamResponse{Response: &cursorpb.AiserverV1_InferenceStreamResponse_Error{Error: &cursorpb.AiserverV1_InferenceStreamError{Code: "permission_denied", Message_: "no"}}}, state, nil)
	if err != nil || len(errorEvents) != 1 || errorEvents[0].Status == nil || errorEvents[0].Status.Code != 7 || errorEvents[0].Status.OK() {
		t.Fatalf("error events = %#v, err=%v", errorEvents, err)
	}
}

func TestReadInferenceStreamHandlesGzipAndMalformedFrames(t *testing.T) {
	response := &cursorpb.AiserverV1_InferenceStreamResponse{Response: &cursorpb.AiserverV1_InferenceStreamResponse_TextPart{TextPart: &cursorpb.AiserverV1_InferenceTextStreamPart{Text: "gzip"}}}
	compressed := new(bytes.Buffer)
	writer := gzip.NewWriter(compressed)
	_, _ = writer.Write(mustMarshal(t, response))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := io.NopCloser(bytes.NewReader(addConnectEnvelope(compressed.Bytes(), true)))
	ch := make(chan ChatEvent, 8)
	readInferenceStream(context.Background(), body, ch)
	var events []ChatEvent
	for event := range ch {
		events = append(events, event)
	}
	if len(events) != 2 || events[0].Server.GetInteractionUpdate().GetTextDelta().GetText() != "gzip" || events[1].Server.GetInteractionUpdate().GetTurnEnded() == nil {
		t.Fatalf("gzip events = %#v", events)
	}

	bad := make(chan ChatEvent, 2)
	readInferenceStream(context.Background(), io.NopCloser(bytes.NewReader([]byte{0, 0, 0, 0, 3, 1})), bad)
	var badEvents []ChatEvent
	for event := range bad {
		badEvents = append(badEvents, event)
	}
	if len(badEvents) != 1 || badEvents[0].Status == nil || badEvents[0].Status.OK() || !strings.Contains(badEvents[0].Status.Message, "truncated") {
		t.Fatalf("malformed frame events = %#v", badEvents)
	}
}

func TestRunInferenceStreamHTTPError(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "upstream failed")
			}))
			defer server.Close()
			account := &auth.Account{AccessToken: "session-token"}
			client := NewClient(account)
			client.API3 = server.URL
			_, err := client.runInferenceStream(context.Background(), &ChatRequest{}, account, "request-id", "run-id", &cursorpb.AiserverV1_InferenceStreamRequest{})
			// Keep the assertion focused on the status code while allowing the
			// bounded response body to vary between test transports.
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("InferenceService/Stream http %d", status)) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func mustMarshal(t *testing.T, message proto.Message) []byte {
	t.Helper()
	data, err := proto.Marshal(message)
	if err != nil {
		t.Fatalf("marshal %T: %v", message, err)
	}
	return data
}
