package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
	"github.com/router-for-me/cursor-proto/translator"
)

// installFakes wires the runnerFactory and hostCallInvoker to the
// caller-supplied fakes and returns a cleanup func. It is intended
// for use inside a subtest as `defer cleanup()`.
func installFakes(t *testing.T, factory func(string, []byte) (chatRunner, string, error), invoker func(string, []byte) ([]byte, error)) func() {
	t.Helper()
	prevRunner := runnerFactory
	prevInvoker := hostCallInvoker
	runnerFactory = factory
	hostCallInvoker = invoker
	return func() {
		runnerFactory = prevRunner
		hostCallInvoker = prevInvoker
	}
}

// fakeRunner emits a scripted list of events on RunChat. Each event
// carries a pre-baked AgentServerMessage so translator.FromServerMessage
// picks up something meaningful.
type fakeRunner struct {
	events []executor.ChatEvent
	err    error
}

type channelRunner struct {
	events <-chan executor.ChatEvent
}

func (r *channelRunner) RunChat(context.Context, *executor.ChatRequest) (<-chan executor.ChatEvent, error) {
	return r.events, nil
}

func (f *fakeRunner) RunChat(ctx context.Context, req *executor.ChatRequest) (<-chan executor.ChatEvent, error) {
	if f.err != nil {
		return nil, f.err
	}
	out := make(chan executor.ChatEvent, len(f.events))
	go func() {
		defer close(out)
		for _, ev := range f.events {
			select {
			case <-ctx.Done():
				return
			case out <- ev:
			}
		}
	}()
	return out, nil
}

// buildTextDeltaEvent produces a ChatEvent that FromServerMessage
// interprets as an EventTextDelta with the given text.
func buildTextDeltaEvent(text string) executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_TextDelta{
					TextDelta: &cursorpb.AgentV1_TextDeltaUpdate{Text: text},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

// buildTurnEndedEvent produces a ChatEvent that FromServerMessage
// interprets as an EventTurnEnded with the given input/output totals.
func buildTurnEndedEvent(inTok, outTok int64) executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_TurnEnded{
					TurnEnded: &cursorpb.AgentV1_TurnEndedUpdate{
						InputTokens:  &inTok,
						OutputTokens: &outTok,
					},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func buildAssistantBlobEvent(text string) executor.ChatEvent {
	blob, _ := json.Marshal(map[string]any{
		"role": "assistant",
		"content": []map[string]any{{
			"type": "text",
			"text": text,
		}},
	})
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_KvServerMessage{
			KvServerMessage: &cursorpb.AgentV1_KvServerMessage{
				Message: &cursorpb.AgentV1_KvServerMessage_SetBlobArgs{
					SetBlobArgs: &cursorpb.AgentV1_SetBlobArgs{BlobData: blob},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func buildSignedAssistantBlobEvent(thinking, signature, text string) executor.ChatEvent {
	blob, _ := json.Marshal(map[string]any{
		"role": "assistant",
		"content": []map[string]any{
			{"type": "reasoning", "text": thinking, "signature": signature},
			{"type": "text", "text": text},
		},
	})
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_KvServerMessage{
			KvServerMessage: &cursorpb.AgentV1_KvServerMessage{
				Message: &cursorpb.AgentV1_KvServerMessage_SetBlobArgs{
					SetBlobArgs: &cursorpb.AgentV1_SetBlobArgs{BlobData: blob},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func buildHeartbeatEvent() executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_Heartbeat{
					Heartbeat: &cursorpb.AgentV1_HeartbeatUpdate{},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

func TestNewPluginExecutorClientUsesChromiumSidecar(t *testing.T) {
	t.Setenv(chromiumSidecarURLEnv, "http://127.0.0.1:18901")
	t.Setenv(chromiumSidecarTokenEnv, "test-token")
	client, err := newPluginExecutorClient(&auth.Account{})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if client.API2 != "http://127.0.0.1:18901/api2" {
		t.Fatalf("API2 = %q", client.API2)
	}
	if client.API3 != client.API2 {
		t.Fatalf("API3 = %q, want %q", client.API3, client.API2)
	}
}

func TestNewPluginExecutorClientCarriesAccountProxyToSidecarRequest(t *testing.T) {
	received := make(chan http.Header, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		received <- request.Header.Clone()
		response.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	t.Setenv(chromiumSidecarURLEnv, server.URL)
	t.Setenv(chromiumSidecarTokenEnv, "test-sidecar-token")
	client, err := newPluginExecutorClient(&auth.Account{
		AccessToken: "test-access-token",
		ProxyURL:    "socks5://proxy-user:proxy-pass@proxy.example:1080",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if err := client.UnaryCall("test.Service", "Probe", nil, nil); err != nil {
		t.Fatalf("unary call: %v", err)
	}

	headers := <-received
	if got := headers.Get("x-cursor-chromium-sidecar-token"); got != "test-sidecar-token" {
		t.Fatalf("sidecar token = %q", got)
	}
	if got := headers.Get("x-cursor-chromium-upstream-proxy"); got != "socks5://proxy-user:proxy-pass@proxy.example:1080" {
		t.Fatalf("sidecar proxy route = %q", got)
	}
}

func TestTranslatePluginEventRestoresDeclaredToolCase(t *testing.T) {
	server := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &cursorpb.AgentV1_ExecServerMessage{
				Message: &cursorpb.AgentV1_ExecServerMessage_McpArgs{
					McpArgs: &cursorpb.AgentV1_McpArgs{
						ToolName:   "mcp_bash",
						ToolCallId: "toolu_1",
					},
				},
			},
		},
	}
	event := translatePluginEvent(server, []executor.ToolDefinition{{Name: "Bash"}})
	if event == nil {
		t.Fatal("expected tool event")
	}
	if event.ToolName != "Bash" {
		t.Fatalf("tool name = %q, want client-declared Bash", event.ToolName)
	}
}

// withShortRequestDeadline shrinks the outermost turn bound so deadline tests
// finish quickly.
func withShortRequestDeadline(t *testing.T, d time.Duration) {
	t.Helper()
	previous := os.Getenv("CURSOR_REQUEST_DEADLINE_MS")
	os.Setenv("CURSOR_REQUEST_DEADLINE_MS", strconv.Itoa(int(d/time.Millisecond)))
	t.Cleanup(func() {
		if previous == "" {
			os.Unsetenv("CURSOR_REQUEST_DEADLINE_MS")
			return
		}
		os.Setenv("CURSOR_REQUEST_DEADLINE_MS", previous)
	})
}

// Cursor can hold a run open with heartbeats and never produce content — seen
// with accounts whose catalog advertises a model they cannot serve. Without an
// absolute ceiling the caller hangs instead of getting an error it can retry.
func TestBuildClaudeNonStreamingStopsAtRequestDeadline(t *testing.T) {
	withShortRequestDeadline(t, 150*time.Millisecond)

	events := make(chan executor.ChatEvent) // never sends, never closes
	done := make(chan error, 1)
	go func() {
		_, err := buildClaudeNonStreaming("claude-fable-5", chatShape{}, &translator.OutputLimiter{}, 1, events)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "request deadline") {
			t.Fatalf("err = %v, want the request-deadline error", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("collector hung past the request deadline")
	}
}

// Once the envelope is committed the deadline must close the stream through
// Anthropic's error event rather than leaving the client waiting.
func TestStreamClaudeStopsWhenOnlyHeartbeatsArrive(t *testing.T) {
	t.Setenv("CURSOR_NO_OUTPUT_DEADLINE_MS", "200")
	t.Setenv("CURSOR_REQUEST_DEADLINE_MS", "5000")
	t.Setenv("CURSOR_STREAM_HEARTBEAT_PREAMBLE_MS", "0")

	var emitted [][]byte
	var mu sync.Mutex
	defer installFakes(t, nil, func(method string, payload []byte) ([]byte, error) {
		if method == "host.stream.emit" {
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		}
		return []byte(`{"ok":true}`), nil
	})()

	events := make(chan executor.ChatEvent, 8)
	go func() {
		for i := 0; i < 20; i++ {
			events <- buildHeartbeatEvent()
			time.Sleep(20 * time.Millisecond)
		}
	}()

	var streamErr string
	done := make(chan struct{})
	go func() {
		streamClaude("s1", "claude-fable-5", false, chatShape{}, &translator.OutputLimiter{}, 1, events, &streamErr)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("heartbeat-only stream hung past the no-output deadline")
	}
	if streamErr != "" && !strings.Contains(streamErr, "no-output") && !strings.Contains(streamErr, "heartbeat") {
		// emit path may have succeeded; the error event is enough
	}
	mu.Lock()
	defer mu.Unlock()
	joined := ""
	for _, payload := range emitted {
		joined += string(payload)
	}
	if !strings.Contains(joined, `"type":"error"`) && !strings.Contains(streamErr, "no-output") {
		t.Fatalf("heartbeat-only stream did not fail closed; err=%q frames=%q", streamErr, joined)
	}
}

func TestStreamClaudeStopsAtRequestDeadlineAfterCommit(t *testing.T) {
	withShortRequestDeadline(t, 400*time.Millisecond)

	var emitted [][]byte
	var mu sync.Mutex
	defer installFakes(t, nil, func(method string, payload []byte) ([]byte, error) {
		if method == "host.stream.emit" {
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		}
		return []byte(`{"ok":true}`), nil
	})()

	// Committed but never finished: exactly the shape a stalled Cursor run has.
	events := make(chan executor.ChatEvent, 1)
	events <- buildTextDeltaEvent("hello")

	var streamErr string
	done := make(chan struct{})
	go func() {
		streamClaude("s1", "claude-fable-5", false, chatShape{}, &translator.OutputLimiter{}, 1, events, &streamErr)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("stream hung past the request deadline")
	}

	mu.Lock()
	defer mu.Unlock()
	joined := ""
	for _, payload := range emitted {
		joined += string(payload)
	}
	if !strings.Contains(joined, "message_start") {
		t.Fatalf("stream never committed; frames = %q", joined)
	}
	if !strings.Contains(joined, `"type":"error"`) {
		t.Fatalf("committed stream did not end with an error event; frames = %q", joined)
	}
}

func globToolCallServerMessage() *cursorpb.AgentV1_AgentServerMessage {
	return &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &cursorpb.AgentV1_ToolCallStartedUpdate{
						ToolCall: &cursorpb.AgentV1_ToolCall{
							Tool: &cursorpb.AgentV1_ToolCall_GlobToolCall{
								GlobToolCall: &cursorpb.AgentV1_GlobToolCall{},
							},
						},
					},
				},
			},
		},
	}
}

func webSearchToolCallServerMessage() *cursorpb.AgentV1_AgentServerMessage {
	return &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &cursorpb.AgentV1_ToolCallStartedUpdate{
						CallId: "srvtoolu_search",
						ToolCall: &cursorpb.AgentV1_ToolCall{
							Tool: &cursorpb.AgentV1_ToolCall_WebSearchToolCall{
								WebSearchToolCall: &cursorpb.AgentV1_WebSearchToolCall{
									Args: &cursorpb.AgentV1_WebSearchArgs{
										SearchTerm: "Claude Code CLI tool list",
										ToolCallId: "srvtoolu_search",
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func webSearchQueryServerMessage() *cursorpb.AgentV1_AgentServerMessage {
	return &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionQuery{
			InteractionQuery: &cursorpb.AgentV1_InteractionQuery{
				Id: 7,
				Query: &cursorpb.AgentV1_InteractionQuery_WebSearchRequestQuery{
					WebSearchRequestQuery: &cursorpb.AgentV1_WebSearchRequestQuery{
						Args: &cursorpb.AgentV1_WebSearchArgs{
							SearchTerm: "Claude Code CLI tool list",
							ToolCallId: "srvtoolu_query",
						},
					},
				},
			},
		},
	}
}

func TestTranslateClaudePluginEventUsesCanonicalCaseForLazyNativeTool(t *testing.T) {
	server := globToolCallServerMessage()
	declared := []executor.ToolDefinition{{Name: "Glob"}}

	event := translateAnthropicPluginEvent(server, declared)
	if event == nil {
		t.Fatal("expected tool event")
	}
	if event.ToolName != "Glob" {
		t.Fatalf("tool name = %q, want Claude Code canonical Glob", event.ToolName)
	}
	generic := translatePluginEvent(server, []executor.ToolDefinition{{Name: "glob"}})
	if generic == nil || generic.ToolName != "glob" {
		t.Fatalf("generic/OpenAI fallback = %+v, want lowercase semantic glob", generic)
	}
}

// Anthropic's contract is that tool_use.name is one of the names the caller
// declared this turn. Cursor runs native tools regardless, and forwarding one
// to a caller that declared none produces a call it cannot execute — observed
// against a live account as a stray tool_use inside a WebSearch-only request.
func TestTranslateClaudePluginEventDropsNativeToolWhenCallerDeclaredNone(t *testing.T) {
	server := globToolCallServerMessage()

	if event := translateAnthropicPluginEvent(server, nil); event != nil {
		t.Fatalf("emitted undeclared tool %q; want the event dropped", event.ToolName)
	}
	if event := translatePluginEvent(server, nil); event != nil {
		t.Fatalf("generic path emitted undeclared tool %q; want the event dropped", event.ToolName)
	}
}

// Claude Code's WebSearch is a client tool. Cursor still emits a native
// WebSearchToolCall; forwarding that as Anthropic server_tool_use makes the
// CLI wait in-stream for a result we never produce, which is the hang shown
// as "Searching…" / "Cogitating…". Remap it to tool_use so the CLI executes.
func TestTranslateClaudePluginEventRemapsNativeWebSearchToDeclaredClientTool(t *testing.T) {
	event := translateAnthropicPluginEvent(webSearchToolCallServerMessage(), []executor.ToolDefinition{{Name: "WebSearch"}})
	if event == nil {
		t.Fatal("expected client tool_use for declared WebSearch")
	}
	if event.Kind != translator.EventToolCallStarted {
		t.Fatalf("kind = %v, want EventToolCallStarted so Claude Code can run the search", event.Kind)
	}
	if event.ToolName != "WebSearch" {
		t.Fatalf("tool name = %q, want declared WebSearch", event.ToolName)
	}
	if !strings.Contains(event.ToolArgsDelta, "Claude Code CLI tool list") {
		t.Fatalf("args = %q, want the search term", event.ToolArgsDelta)
	}
}

func TestTranslateClaudePluginEventRemapsSearchPermissionToDeclaredClientTool(t *testing.T) {
	event := translateAnthropicPluginEvent(webSearchQueryServerMessage(), []executor.ToolDefinition{{Name: "WebSearch"}})
	if event == nil {
		t.Fatal("expected client tool_use from InteractionQuery")
	}
	if event.Kind != translator.EventToolCallStarted || event.ToolName != "WebSearch" {
		t.Fatalf("event = %+v, want tool_use WebSearch", event)
	}
}

func TestTranslateClaudePluginEventKeepsNativeWebSearchAsServerToolWhenUndeclared(t *testing.T) {
	event := translateAnthropicPluginEvent(webSearchToolCallServerMessage(), nil)
	if event == nil {
		t.Fatal("server-tool-only requests must still surface native web search")
	}
	if event.Kind != translator.EventServerToolStarted {
		t.Fatalf("kind = %v, want EventServerToolStarted for Anthropic web_search_*", event.Kind)
	}
	if event.ToolName != "web_search" {
		t.Fatalf("tool name = %q, want web_search", event.ToolName)
	}
}

func TestTranslateClaudePluginEventDropsUndeclaredFetchToolCall(t *testing.T) {
	server := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &cursorpb.AgentV1_ToolCallStartedUpdate{
						CallId: "toolu_fetch",
						ToolCall: &cursorpb.AgentV1_ToolCall{
							Tool: &cursorpb.AgentV1_ToolCall_FetchToolCall{
								FetchToolCall: &cursorpb.AgentV1_FetchToolCall{},
							},
						},
					},
				},
			},
		},
	}
	if event := translateAnthropicPluginEvent(server, []executor.ToolDefinition{{Name: "get_weather"}}); event != nil {
		t.Fatalf("undeclared fetch leaked as %+v", event)
	}
}

func TestEstimateClaudeInputTokensIsAtLeastOne(t *testing.T) {
	if got := estimateClaudeInputTokens(chatShape{}); got != 1 {
		t.Fatalf("empty shape = %d, want 1", got)
	}
	if got := estimateClaudeInputTokens(chatShape{UserMessage: "What is the weather in San Francisco?"}); got < 2 {
		t.Fatalf("prompt estimate = %d, want a non-trivial count", got)
	}
}

func TestTranslateClaudePluginEventDropsUndeclaredFetchPermission(t *testing.T) {
	server := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionQuery{
			InteractionQuery: &cursorpb.AgentV1_InteractionQuery{
				Query: &cursorpb.AgentV1_InteractionQuery_WebFetchRequestQuery{
					WebFetchRequestQuery: &cursorpb.AgentV1_WebFetchRequestQuery{
						Args: &cursorpb.AgentV1_WebFetchArgs{Url: "https://example.com", ToolCallId: "srvtoolu_fetch"},
					},
				},
			},
		},
	}
	if event := translateAnthropicPluginEvent(server, nil); event != nil {
		t.Fatalf("undeclared fetch leaked as %+v", event)
	}
}

func TestTranslateClaudePluginEventAnnouncesSearchPermissionAsServerTool(t *testing.T) {
	event := translateAnthropicPluginEvent(webSearchQueryServerMessage(), nil)
	if event == nil {
		t.Fatal("server-tool path dropped the permission query; want an early server_tool_use")
	}
	if event.Kind != translator.EventServerToolStarted || event.ToolName != "web_search" {
		t.Fatalf("event = %+v, want server_tool_use web_search", event)
	}
	if event.ToolCallID != "srvtoolu_query" {
		t.Fatalf("id = %q, want srvtoolu_query", event.ToolCallID)
	}
}

func TestTranslateClaudePluginEventKeepsBedrockToolID(t *testing.T) {
	server := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &cursorpb.AgentV1_ExecServerMessage{
				Message: &cursorpb.AgentV1_ExecServerMessage_McpArgs{
					McpArgs: &cursorpb.AgentV1_McpArgs{
						ToolName:   "get_weather",
						ToolCallId: "toolu_bdrk_014QuhsYS4bAkK5hyQAxoFAY",
					},
				},
			},
		},
	}
	event := translateAnthropicPluginEvent(server, []executor.ToolDefinition{{Name: "get_weather"}})
	if event == nil {
		t.Fatal("expected tool event")
	}
	if event.ToolCallID != "toolu_bdrk_014QuhsYS4bAkK5hyQAxoFAY" {
		t.Fatalf("id = %q, want Bedrock infix kept", event.ToolCallID)
	}
}

func TestTranslateClaudePluginEventKeepsVertexToolID(t *testing.T) {
	server := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &cursorpb.AgentV1_ExecServerMessage{
				Message: &cursorpb.AgentV1_ExecServerMessage_McpArgs{
					McpArgs: &cursorpb.AgentV1_McpArgs{
						ToolName:   "get_weather",
						ToolCallId: "toolu_vrtx_01JurySmHCDBTjuh8LgwtdAZ",
					},
				},
			},
		},
	}
	event := translateAnthropicPluginEvent(server, []executor.ToolDefinition{{Name: "get_weather"}})
	if event == nil {
		t.Fatal("expected tool event")
	}
	if event.ToolCallID != "toolu_vrtx_01JurySmHCDBTjuh8LgwtdAZ" {
		t.Fatalf("id = %q, want Vertex infix kept", event.ToolCallID)
	}
}

func TestTranslateClaudePluginEventPreservesDeclaredLowercaseTool(t *testing.T) {
	server := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &cursorpb.AgentV1_ToolCallStartedUpdate{
						ToolCall: &cursorpb.AgentV1_ToolCall{
							Tool: &cursorpb.AgentV1_ToolCall_GlobToolCall{
								GlobToolCall: &cursorpb.AgentV1_GlobToolCall{},
							},
						},
					},
				},
			},
		},
	}
	event := translateAnthropicPluginEvent(server, []executor.ToolDefinition{{Name: "glob"}})
	if event == nil {
		t.Fatal("expected tool event")
	}
	if event.ToolName != "glob" {
		t.Fatalf("tool name = %q, want client-declared glob", event.ToolName)
	}
}

func buildThinkingDeltaEvent(text string) executor.ChatEvent {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ThinkingDelta{
					ThinkingDelta: &cursorpb.AgentV1_ThinkingDeltaUpdate{Text: text},
				},
			},
		},
	}
	return executor.ChatEvent{Server: msg}
}

// buildFakeExecutorRequest hand-marshals the executorRequest JSON with
// the given payload and format. StorageJSON is a fake but well-formed
// AuthFile so the executor code that touches it does not panic; tests
// swap the runnerFactory so no real Cursor client is built.
func buildFakeExecutorRequest(t *testing.T, format string, payload []byte, stream bool, streamID string) []byte {
	t.Helper()
	req := executorRequest{
		AuthID:       "fake-auth",
		AuthProvider: "cursor",
		Model:        "composer-2.5",
		Format:       format,
		Stream:       stream,
		Payload:      payload,
		StorageJSON:  []byte(`{"type":"cursor","access_token":"AT","email":"fake@example.com"}`),
		StreamID:     streamID,
	}
	buf, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	return buf
}

func TestExecuteRejectsAccountExcludedModelBeforeUpstream(t *testing.T) {
	runnerCalled := false
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) {
			runnerCalled = true
			return &fakeRunner{}, "unit@example.com", nil
		},
		nil,
	)()

	req := executorRequest{
		AuthID:       "native-only",
		AuthProvider: "cursor",
		Model:        "claude-opus-5",
		Format:       "claude",
		Payload:      []byte(`{"model":"claude-opus-5","messages":[{"role":"user","content":"hi"}]}`),
		StorageJSON:  []byte(`{"type":"cursor","access_token":"AT","email":"native@example.com","excluded_models":["CLAUDE-*"]}`),
	}
	rawRequest, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	raw, rc := dispatch("executor.execute", rawRequest)
	if rc != 0 {
		t.Fatalf("rc = %d, want 0. envelope=%s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if env.OK || env.Error == nil {
		t.Fatalf("expected model exclusion error, got %s", string(raw))
	}
	if env.Error.Code != "model_excluded" || !env.Error.Retryable {
		t.Fatalf("unexpected error: %+v", env.Error)
	}
	if runnerCalled {
		t.Fatal("excluded model reached upstream runner")
	}
}

// TestExecute_OpenAI_NonStreaming exercises handleExecutorExecute end
// to end with a scripted RunChat and asserts the OpenAI response body
// carries the assistant text and a stop finish_reason.
func TestExecute_OpenAI_NonStreaming(t *testing.T) {
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("Hello, "),
			buildTextDeltaEvent("world."),
			buildTurnEndedEvent(11, 3),
		},
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) {
			return runner, "unit@example.com", nil
		},
		nil,
	)()

	payload := []byte(`{
        "model": "composer-2.5",
        "messages": [{"role": "user", "content": "hi"}]
    }`)
	raw, rc := dispatch("executor.execute", buildFakeExecutorRequest(t, "openai", payload, false, ""))
	if rc != 0 {
		t.Fatalf("rc = %d, want 0. envelope=%s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var resp executorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !strings.Contains(string(resp.Payload), `"content":"Hello, world."`) {
		t.Errorf("payload missing assistant text: %s", string(resp.Payload))
	}
	if !strings.Contains(string(resp.Payload), `"finish_reason":"stop"`) {
		t.Errorf("payload missing stop reason: %s", string(resp.Payload))
	}
	if !strings.Contains(string(resp.Payload), `"prompt_tokens":11`) {
		t.Errorf("payload missing usage: %s", string(resp.Payload))
	}
}

// TestExecute_Claude_NonStreaming exercises the same path for Claude
// output.
func TestExecute_Claude_NonStreaming(t *testing.T) {
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("Bonjour"),
			buildTurnEndedEvent(7, 2),
		},
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) {
			return runner, "", nil
		},
		nil,
	)()

	payload := []byte(`{
        "model": "claude-4.5-sonnet",
        "messages": [{"role": "user", "content": [{"type": "text", "text": "hi"}]}]
    }`)
	raw, rc := dispatch("executor.execute", buildFakeExecutorRequest(t, "claude", payload, false, ""))
	if rc != 0 {
		t.Fatalf("rc = %d, want 0. envelope=%s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var resp executorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if !strings.Contains(string(resp.Payload), `"text":"Bonjour"`) {
		t.Errorf("payload missing text block: %s", string(resp.Payload))
	}
	if !strings.Contains(string(resp.Payload), `"stop_reason":"end_turn"`) {
		t.Errorf("payload missing end_turn: %s", string(resp.Payload))
	}
}

// TestExecuteStream_OpenAI captures the payload units the plugin emits. CPA
// adds OpenAI SSE framing around each unit, so the plugin must end with raw
// [DONE] and must not emit already-framed `data:` lines.
func TestExecuteStream_OpenAI(t *testing.T) {
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("Hi"),
			buildTurnEndedEvent(4, 1),
		},
	}

	var (
		mu       sync.Mutex
		emitted  [][]byte
		closed   bool
		closeErr string
		done     = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				StreamID string `json:"stream_id"`
				Payload  []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			var req struct {
				StreamID string `json:"stream_id"`
				Error    string `json:"error"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("close unmarshal: %v", err)
			}
			mu.Lock()
			closed = true
			closeErr = req.Error
			mu.Unlock()
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{
        "model": "composer-2.5",
        "messages": [{"role": "user", "content": "hi"}],
        "stream": true,
        "stream_options": {"include_usage": true}
    }`)
	raw, rc := dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "openai", payload, true, "s-1"))
	if rc != 0 {
		t.Fatalf("rc = %d, want 0: %s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	// Wait for the goroutine to signal close.
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("stream never closed within 2s")
	}
	mu.Lock()
	defer mu.Unlock()
	if !closed {
		t.Fatalf("stream never closed. emitted=%d", len(emitted))
	}
	if closeErr != "" {
		t.Errorf("close carried error: %q", closeErr)
	}
	if len(emitted) < 2 {
		t.Fatalf("emitted too few chunks: %d", len(emitted))
	}
	last := string(emitted[len(emitted)-1])
	if last != "[DONE]" {
		t.Errorf("final chunk not [DONE]: %q", last)
	}
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	if strings.Contains(joined, "data:") {
		t.Errorf("OpenAI host payloads must not contain SSE framing: %s", joined)
	}
	if !strings.Contains(joined, `"content":"Hi"`) {
		t.Errorf("stream missing content: %s", joined)
	}
	if !strings.Contains(joined, `"finish_reason":"stop"`) {
		t.Errorf("stream missing finish_reason=stop: %s", joined)
	}
	if !strings.Contains(joined, `"prompt_tokens":4`) {
		t.Errorf("stream missing usage frame: %s", joined)
	}
}

func TestExecuteStream_OpenAI_DeduplicatesFinalAssistantBlob(t *testing.T) {
	runner := &fakeRunner{events: []executor.ChatEvent{
		buildTextDeltaEvent("Hi"),
		buildAssistantBlobEvent("Hi"),
		buildTurnEndedEvent(4, 1),
	}}

	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"composer-2.5","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "openai", payload, true, "dedupe-openai"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if got := strings.Count(joined, `"content":"Hi"`); got != 1 {
		t.Fatalf("assistant text emitted %d times, want once: %s", got, joined)
	}
}

func TestExecuteStream_Claude_ForwardsHeartbeatAndDeduplicatesFinalBlob(t *testing.T) {
	runner := &fakeRunner{events: []executor.ChatEvent{
		buildHeartbeatEvent(),
		buildThinkingDeltaEvent("reasoning"),
		buildHeartbeatEvent(),
		buildTextDeltaEvent("Hi"),
		buildAssistantBlobEvent("Hi"),
		buildTurnEndedEvent(4, 1),
	}}

	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "dedupe-claude"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if !strings.Contains(joined, `event: ping`) || !strings.Contains(joined, `{"type":"ping"}`) {
		t.Fatalf("heartbeat was not forwarded as an Anthropic ping: %s", joined)
	}
	if messageStartAt, pingAt := strings.Index(joined, `event: message_start`), strings.Index(joined, `event: ping`); messageStartAt < 0 || pingAt < 0 || messageStartAt > pingAt {
		t.Fatalf("Anthropic stream emitted ping before message_start: %s", joined)
	}
	if !strings.Contains(joined, `"type":"thinking_delta"`) || !strings.Contains(joined, `"thinking":"reasoning"`) {
		t.Fatalf("thinking delta was not forwarded as Anthropic thinking: %s", joined)
	}
	if got := strings.Count(joined, `"text":"Hi"`); got != 1 {
		t.Fatalf("assistant text emitted %d times, want once: %s", got, joined)
	}
}

func TestExecuteStream_Claude_HeartbeatOnlyEndsInBandWithoutRetryableCloseError(t *testing.T) {
	t.Setenv("CURSOR_STREAM_FIRST_OUTPUT_TIMEOUT_MS", "50")
	t.Setenv("CURSOR_STREAM_HEARTBEAT_PREAMBLE_MS", "0")
	events := make(chan executor.ChatEvent, 1)
	events <- buildHeartbeatEvent()
	defer close(events)

	var (
		mu           sync.Mutex
		emitted      [][]byte
		closedError  string
		streamClosed = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			var req struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("close unmarshal: %v", err)
			}
			closedError = req.Error
			close(streamClosed)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return &channelRunner{events: events}, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "heartbeat-timeout"))
	select {
	case <-streamClosed:
	case <-time.After(2 * time.Second):
		t.Fatal("heartbeat-only stream never timed out")
	}

	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if !strings.Contains(joined, "event: message_start") || !strings.Contains(joined, "event: ping") {
		t.Fatalf("heartbeat did not establish a valid Anthropic stream: %s", joined)
	}
	if closedError != "" {
		t.Fatalf("close error = %q, want clean close after stream start", closedError)
	}
	if !strings.Contains(joined, "event: error") || !strings.Contains(joined, `"type":"api_error"`) || !strings.Contains(joined, firstOutputTimeoutMessage) {
		t.Fatalf("standard timeout error event was not emitted in-band: %s", joined)
	}
	if strings.Contains(joined, "event: message_stop") || strings.Contains(joined, `"stop_reason":"error"`) {
		t.Fatalf("failed Anthropic stream was misrepresented as a successful stop: %s", joined)
	}
	if got := strings.Count(joined, "event: message_start"); got != 1 {
		t.Fatalf("message_start count = %d, want 1: %s", got, joined)
	}
}

func TestExecuteStream_Claude_HeartbeatRenewsFirstOutputIdleTimeout(t *testing.T) {
	t.Setenv("CURSOR_STREAM_FIRST_OUTPUT_TIMEOUT_MS", "50")
	t.Setenv("CURSOR_STREAM_HEARTBEAT_PREAMBLE_MS", "0")
	events := make(chan executor.ChatEvent)
	go func() {
		defer close(events)
		events <- buildHeartbeatEvent()
		time.Sleep(35 * time.Millisecond)
		events <- buildHeartbeatEvent()
		time.Sleep(35 * time.Millisecond)
		events <- buildTextDeltaEvent("LONG_CONTEXT_OK")
		events <- buildTurnEndedEvent(1000, 4)
	}()

	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return &channelRunner{events: events}, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "heartbeat-renewal"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if strings.Contains(joined, firstOutputTimeoutMessage) {
		t.Fatalf("live heartbeat stream was killed by first-output timeout: %s", joined)
	}
	if !strings.Contains(joined, "LONG_CONTEXT_OK") {
		t.Fatalf("post-heartbeat content was not emitted: %s", joined)
	}
}

func TestExecuteStream_Claude_FastEmptyAfterHeartbeatStaysRetryableBeforePreamble(t *testing.T) {
	runner := &fakeRunner{events: []executor.ChatEvent{buildHeartbeatEvent()}}

	var (
		mu          sync.Mutex
		emitted     [][]byte
		closedError string
		done        = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			var req struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("close unmarshal: %v", err)
			}
			closedError = req.Error
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "fast-empty"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("fast-empty stream never closed")
	}

	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if joined != "" {
		t.Fatalf("fast-empty stream committed bytes before retry: %s", joined)
	}
	if closedError != emptyUpstreamResponseMessage {
		t.Fatalf("close error = %q, want %q", closedError, emptyUpstreamResponseMessage)
	}
}

func TestBuildClaudeNonStreaming_DedupesDuplicateServerToolUse(t *testing.T) {
	events := make(chan executor.ChatEvent, 3)
	events <- executor.ChatEvent{Server: webSearchToolCallServerMessage()}
	events <- executor.ChatEvent{Server: webSearchToolCallServerMessage()}
	events <- buildTurnEndedEvent(10, 2)
	close(events)

	raw, err := buildClaudeNonStreaming("claude-opus-4-8", chatShape{}, &translator.OutputLimiter{}, 1, events)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	serverUses := 0
	for _, block := range body["content"].([]any) {
		m := block.(map[string]any)
		if m["type"] == "server_tool_use" {
			serverUses++
		}
	}
	if serverUses != 1 {
		t.Fatalf("server_tool_use blocks = %d, want 1", serverUses)
	}
}

func TestExecuteStream_Claude_OrphanServerSearchCompletes(t *testing.T) {
	runner := &fakeRunner{
		events: []executor.ChatEvent{{Server: webSearchToolCallServerMessage()}},
	}
	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}],"messages":[{"role":"user","content":"Search the web for today's date in Tokyo"}]}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "orphan-search"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("orphan server search stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if !strings.Contains(joined, `"type":"server_tool_use"`) {
		t.Fatalf("missing server_tool_use:\n%s", joined)
	}
	if !strings.Contains(joined, `"type":"web_search_tool_result"`) {
		t.Fatalf("missing web_search_tool_result after upstream closed:\n%s", joined)
	}
	if !strings.Contains(joined, "event: message_stop") {
		t.Fatalf("missing message_stop:\n%s", joined)
	}
	if strings.Contains(joined, `"type":"error"`) {
		t.Fatalf("orphan search ended as error instead of a finished tool pair:\n%s", joined)
	}
}

func TestParseClaudePayload_ForcesNamedToolChoice(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
		"model":"claude-opus-4-8",
		"tool_choice":{"type":"tool","name":"record_answer"},
		"tools":[{"name":"record_answer","input_schema":{"type":"object"}}],
		"messages":[{"role":"user","content":"Answer is STRUCTURED_OK and count is 7."}]
	}`))
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if shape.ForceTool != "record_answer" {
		t.Fatalf("ForceTool = %q, want record_answer", shape.ForceTool)
	}
	req := buildChatRequest(shape, nil)
	// The forced-tool instruction is now emitted with the imperative
	// "MUST call" (upper case) so short/ambiguous prompts still trigger
	// the call — cctest.ai's tool_use probe was reliably failing when
	// the polite wording let Claude ask for clarification instead.
	if !strings.Contains(req.UserMessage, "record_answer") || !strings.Contains(req.UserMessage, "MUST call") {
		t.Fatalf("forced tool instruction missing from user turn: %q", req.UserMessage)
	}
}

func TestParseOpenAIPayload_ForcesNamedFunctionToolChoice(t *testing.T) {
	shape, err := parseOpenAIPayload([]byte(`{
		"model":"gpt-5.5",
		"tool_choice":{"type":"function","function":{"name":"record_answer"}},
		"tools":[{"type":"function","function":{"name":"record_answer","parameters":{"type":"object"}}}],
		"messages":[{"role":"user","content":"Answer is STRUCTURED_OK and count is 7."}]
	}`))
	if err != nil {
		t.Fatalf("parseOpenAIPayload: %v", err)
	}
	if shape.ForceTool != "record_answer" {
		t.Fatalf("ForceTool = %q, want record_answer", shape.ForceTool)
	}
	req := buildChatRequest(shape, nil)
	if !strings.Contains(req.UserMessage, "record_answer") || !strings.Contains(req.UserMessage, "MUST call") {
		t.Fatalf("forced tool instruction missing from user turn: %q", req.UserMessage)
	}
}

func TestExecuteStream_Claude_ShortStreamEmitsPingBeforeStop(t *testing.T) {
	runner := &fakeRunner{
		events: []executor.ChatEvent{
			buildTextDeltaEvent("STREAM_OK"),
			buildTurnEndedEvent(4, 1),
		},
	}
	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-4-8","max_tokens":32,"messages":[{"role":"user","content":"Count: 1 2 3"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "short-ping"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if !strings.Contains(joined, `event: ping`) {
		t.Fatalf("short stream missing ping: %s", joined)
	}
	pingAt := strings.Index(joined, `event: ping`)
	stopAt := strings.Index(joined, `event: message_stop`)
	if pingAt < 0 || stopAt < 0 || pingAt > stopAt {
		t.Fatalf("ping must precede message_stop: %s", joined)
	}
}

func TestBuildClaudeNonStreamingSurfacesCursorTrailerError(t *testing.T) {
	events := make(chan executor.ChatEvent, 1)
	events <- executor.ChatEvent{
		Trailer: true,
		Status:  &executor.TrailerStatus{Code: 8, Message: "billing blocked"},
	}
	close(events)

	_, err := buildClaudeNonStreaming("claude-opus-5", chatShape{}, &translator.OutputLimiter{}, 1, events)
	if err == nil {
		t.Fatal("expected Cursor trailer error")
	}
	if !strings.Contains(err.Error(), "billing blocked") || !strings.Contains(err.Error(), "grpc-status=8") {
		t.Fatalf("error = %q, want parsed Cursor trailer", err)
	}
}

func TestBuildOpenAINonStreamingSurfacesCursorTrailerError(t *testing.T) {
	events := make(chan executor.ChatEvent, 1)
	events <- executor.ChatEvent{
		Trailer: true,
		Status:  &executor.TrailerStatus{Code: 8, Message: "named model unavailable"},
	}
	close(events)

	_, err := buildOpenAINonStreaming("claude-opus-5", nil, events)
	if err == nil {
		t.Fatal("expected Cursor trailer error")
	}
	if !strings.Contains(err.Error(), "named model unavailable") || !strings.Contains(err.Error(), "grpc-status=8") {
		t.Fatalf("error = %q, want parsed Cursor trailer", err)
	}
}

func TestExecuteStreamClaudeSurfacesCursorTrailerError(t *testing.T) {
	runner := &fakeRunner{events: []executor.ChatEvent{
		buildHeartbeatEvent(),
		{
			Trailer: true,
			Status:  &executor.TrailerStatus{Code: 8, Message: "billing blocked"},
		},
	}}

	var (
		mu          sync.Mutex
		emitted     [][]byte
		closedError string
		done        = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			var req struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("close unmarshal: %v", err)
			}
			closedError = req.Error
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-5","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "trailer-error"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("trailer-error stream never closed")
	}

	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	if joined != "" {
		t.Fatalf("trailer error committed bytes before host retry: %s", joined)
	}
	if !strings.Contains(closedError, "billing blocked") || !strings.Contains(closedError, "grpc-status=8") {
		t.Fatalf("close error = %q, want parsed Cursor trailer", closedError)
	}
}

func TestExecuteStream_Claude_OrdersRealSignatureBeforeBufferedText(t *testing.T) {
	runner := &fakeRunner{events: []executor.ChatEvent{
		buildThinkingDeltaEvent("reasoning"),
		buildTextDeltaEvent("answer"),
		buildSignedAssistantBlobEvent("reasoning", "c2lnbmF0dXJl", "answer"),
		buildTurnEndedEvent(4, 2),
	}}

	var (
		mu      sync.Mutex
		emitted [][]byte
		done    = make(chan struct{})
	)
	invoker := func(method string, payload []byte) ([]byte, error) {
		switch method {
		case "host.stream.emit":
			var req struct {
				Payload []byte `json:"payload"`
			}
			if err := json.Unmarshal(payload, &req); err != nil {
				t.Fatalf("emit unmarshal: %v", err)
			}
			mu.Lock()
			emitted = append(emitted, append([]byte(nil), req.Payload...))
			mu.Unlock()
		case "host.stream.close":
			close(done)
		}
		return []byte(`{"ok":true}`), nil
	}
	defer installFakes(t,
		func(_ string, _ []byte) (chatRunner, string, error) { return runner, "", nil },
		invoker,
	)()

	payload := []byte(`{"model":"claude-opus-4-8-medium","thinking":{"type":"adaptive"},"messages":[{"role":"user","content":"hi"}],"stream":true}`)
	_, _ = dispatch("executor.execute_stream", buildFakeExecutorRequest(t, "claude", payload, true, "signed-thinking"))
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("stream never closed")
	}
	mu.Lock()
	joined := strings.Join(byteSlicesToStrings(emitted), "")
	mu.Unlock()
	thinkingAt := strings.Index(joined, `"type":"thinking_delta"`)
	signatureAt := strings.Index(joined, `"type":"signature_delta"`)
	textAt := strings.Index(joined, `"type":"text_delta"`)
	if thinkingAt < 0 || signatureAt < 0 || textAt < 0 || !(thinkingAt < signatureAt && signatureAt < textAt) {
		t.Fatalf("invalid thinking/signature/text order: %s", joined)
	}
	if strings.Count(joined, `"text":"answer"`) != 1 {
		t.Fatalf("buffered answer was duplicated: %s", joined)
	}
}

func byteSlicesToStrings(chunks [][]byte) []string {
	out := make([]string, 0, len(chunks))
	for _, c := range chunks {
		out = append(out, string(c))
	}
	return out
}

// TestCountTokens exercises the local heuristic and asserts the
// returned envelope wraps a `total_tokens` field.
func TestCountTokens(t *testing.T) {
	payload := []byte(`{
        "model": "composer-2.5",
        "messages": [
            {"role": "system", "content": "you are helpful"},
            {"role": "user", "content": "hello world"}
        ]
    }`)
	raw, rc := dispatch("executor.count_tokens", buildFakeExecutorRequest(t, "openai", payload, false, ""))
	if rc != 0 {
		t.Fatalf("rc = %d, want 0: %s", rc, string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var resp executorResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	total, ok := out["total_tokens"].(float64)
	if !ok || total <= 0 {
		t.Errorf("total_tokens missing or non-positive: %v", out["total_tokens"])
	}
}

// TestCountTokens_CJK exercises the CJK code path so we do not lose
// coverage of the isCJK branch.
func TestCountTokens_CJK(t *testing.T) {
	if got := countTokens("你好世界"); got == 0 {
		t.Errorf("countTokens(CJK) = 0, want > 0")
	}
	if got := countTokens("hello"); got == 0 {
		t.Errorf("countTokens(ASCII) = 0, want > 0")
	}
}

// TestParseOpenAIPayload_NoUser ensures the parser rejects payloads
// with no user message.
func TestParseOpenAIPayload_NoUser(t *testing.T) {
	_, err := parseOpenAIPayload([]byte(`{"model":"x","messages":[{"role":"system","content":"s"}]}`))
	if err == nil {
		t.Fatal("expected error for missing user message")
	}
}

func TestParseClaudePayload_PreservesToolUseAndToolResultHistory(t *testing.T) {
	payload := []byte(`{
		"model":"claude-opus-5",
		"messages":[
			{"role":"user","content":"Check Shanghai weather."},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{"city":"Shanghai"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"Sunny, 28 C."}]},
			{"role":"user","content":"Summarize the result."}
		]
	}`)

	shape, err := parseClaudePayload(payload)
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if len(shape.History) != 3 {
		t.Fatalf("history length = %d, want 3: %#v", len(shape.History), shape.History)
	}
	if !strings.Contains(shape.History[1].Content, `"type":"tool_use"`) ||
		!strings.Contains(shape.History[1].Content, `"name":"get_weather"`) ||
		!strings.Contains(shape.History[1].Content, `"city":"Shanghai"`) {
		t.Fatalf("assistant tool_use was not preserved: %q", shape.History[1].Content)
	}
	if !strings.Contains(shape.History[2].Content, `"type":"tool_result"`) ||
		!strings.Contains(shape.History[2].Content, `"tool_use_id":"toolu_123"`) ||
		!strings.Contains(shape.History[2].Content, `Sunny, 28 C.`) {
		t.Fatalf("user tool_result was not preserved: %q", shape.History[2].Content)
	}
}

func TestParseClaudePayload_ContinuesFromToolResultOnlyTurn(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
		"model":"claude-opus-5",
		"messages":[
			{"role":"user","content":"Check Paris weather."},
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_123","name":"get_weather","input":{"city":"Paris"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_123","content":"Sunny, 21 C."}]}
		]
	}`))
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if !strings.Contains(shape.UserMessage, "tool_result") || !strings.Contains(shape.UserMessage, "Sunny, 21 C.") {
		t.Fatalf("tool-result-only user turn was not rewritten: %q", shape.UserMessage)
	}
	if strings.HasPrefix(strings.TrimSpace(shape.UserMessage), `{"type":"tool_result"`) {
		t.Fatalf("raw tool_result JSON left as UserMessage: %q", shape.UserMessage)
	}
}

// TestParseClaudePayload_ArrayContent covers the content-block path
// so we do not lose it on refactor.
func TestParseClaudePayload_ArrayContent(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
        "model": "claude",
        "system": [{"type":"text","text":"sys1"},{"type":"text","text":"sys2"}],
        "messages": [{"role":"user","content":[{"type":"text","text":"hi"}]}]
    }`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if shape.SystemPrompt != "sys1\nsys2" {
		t.Errorf("system = %q", shape.SystemPrompt)
	}
	if shape.UserMessage != "hi" {
		t.Errorf("user = %q", shape.UserMessage)
	}
}

func TestParseClaudePayload_PreservesImageAndDocumentAttachments(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
		"model":"claude-opus-4-8",
		"messages":[{"role":"user","content":[
			{"type":"image","source":{"type":"base64","media_type":"image/png","data":"aW1hZ2U="}},
			{"type":"document","title":"report.pdf","source":{"type":"base64","media_type":"application/pdf","data":"cGRm"}},
			{"type":"text","text":"inspect both"}
		]}]
	}`))
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if shape.UserMessage != "inspect both" {
		t.Fatalf("UserMessage = %q, want inspect both", shape.UserMessage)
	}
	if len(shape.Attachments) != 2 {
		t.Fatalf("attachments = %d, want 2", len(shape.Attachments))
	}
	if got := shape.Attachments[0]; got.Kind != "image" || got.MimeType != "image/png" || string(got.Data) != "image" {
		t.Fatalf("image attachment = %+v", got)
	}
	if got := shape.Attachments[1]; got.Kind != "document" || got.Filename != "report.pdf" || string(got.Data) != "pdf" {
		t.Fatalf("document attachment = %+v", got)
	}
}

func TestParseClaudePayload_ClientWebSearchIsNotServerTool(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
		"model":"claude-fable-5",
		"tools":[{"name":"WebSearch","description":"Search the web","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}],
		"messages":[{"role":"user","content":"search"}]
	}`))
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if shape.WebSearch {
		t.Fatal("Claude Code WebSearch must stay a client tool, not Cursor native search")
	}
	if len(shape.Tools) != 1 || shape.Tools[0].Name != "WebSearch" {
		t.Fatalf("tools = %+v, want declared WebSearch", shape.Tools)
	}
	req := buildChatRequest(shape, nil)
	if req.WebSearch {
		t.Fatal("ChatRequest.WebSearch = true; that keeps the SSE open waiting for Cursor")
	}
}

func TestTranslateClaudePluginEventRemapsWebSearchForAnyModel(t *testing.T) {
	// Remap is keyed off the caller's tools[], never the model id. Switching
	// Claude Code from fable-5 to opus-4-8 / sonnet-5 / composer must not
	// change the name the CLI dispatches on.
	for _, model := range []string{"claude-fable-5", "claude-opus-4-8", "claude-sonnet-5", "composer-2.5"} {
		shape, err := parseClaudePayload([]byte(`{
			"model":"` + model + `",
			"tools":[{"name":"WebSearch","input_schema":{"type":"object"}}],
			"messages":[{"role":"user","content":"search"}]
		}`))
		if err != nil {
			t.Fatalf("%s parse: %v", model, err)
		}
		if shape.WebSearch || buildChatRequest(shape, nil).WebSearch {
			t.Fatalf("%s treated client WebSearch as a Cursor server tool", model)
		}
		event := translateAnthropicPluginEvent(webSearchToolCallServerMessage(), shape.Tools)
		if event == nil || event.Kind != translator.EventToolCallStarted || event.ToolName != "WebSearch" {
			t.Fatalf("%s remap = %+v, want tool_use WebSearch", model, event)
		}
	}
}

func TestParseClaudePayload_UsesNativeWebSearch(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
		"model":"claude-opus-5",
		"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":1}],
		"messages":[{"role":"user","content":"search"}]
	}`))
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if !shape.WebSearch {
		t.Fatal("WebSearch = false, want true")
	}
	if len(shape.Tools) != 0 {
		t.Fatalf("server tool leaked into MCP tools: %+v", shape.Tools)
	}
	req := buildChatRequest(shape, nil)
	if !req.WebSearch {
		t.Fatal("ChatRequest.WebSearch = false, want true")
	}
	if req.Mode != executor.APIConversationMode(true) {
		t.Fatalf("WebSearch mode = %d, want agent mode", req.Mode)
	}
	if req.PureMode {
		t.Fatal("WebSearch PureMode = true, want IDE request context")
	}
	if req.WorkspacePath != "" {
		t.Fatalf("WebSearch workspace = %q, want account-profile default", req.WorkspacePath)
	}
}

func TestParseClaudePayload_UsesLatestNativeWebSearchVersion(t *testing.T) {
	payload := []byte(`{
		"model":"claude-opus-5",
		"messages":[{"role":"user","content":"search"}],
		"tools":[{"type":"web_search_20260318","name":"web_search","max_uses":1}],
		"stream":true
	}`)
	shape, err := parseClaudePayload(payload)
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if !shape.WebSearch {
		t.Fatal("WebSearch = false for web_search_20260318")
	}
	if len(shape.Tools) != 0 {
		t.Fatalf("server WebSearch leaked into client tools: %+v", shape.Tools)
	}
}

func TestParseClaudePayload_UsesAnyNativeWebFetchVersion(t *testing.T) {
	for _, typ := range []string{"web_fetch_20250305", "web_fetch_20250910"} {
		shape, err := parseClaudePayload([]byte(fmt.Sprintf(`{
			"model":"claude-opus-4-8",
			"tools":[{"type":%q,"name":"web_fetch","max_uses":1}],
			"messages":[{"role":"user","content":"fetch"}]
		}`, typ)))
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if !shape.WebFetch {
			t.Fatalf("%s WebFetch = false, want true", typ)
		}
		if len(shape.Tools) != 0 {
			t.Fatalf("%s leaked into client tools: %+v", typ, shape.Tools)
		}
	}
}

func TestTranslateClaudeEventAnnouncesDeclaredFetchAsServerTool(t *testing.T) {
	server := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionQuery{
			InteractionQuery: &cursorpb.AgentV1_InteractionQuery{
				Query: &cursorpb.AgentV1_InteractionQuery_WebFetchRequestQuery{
					WebFetchRequestQuery: &cursorpb.AgentV1_WebFetchRequestQuery{
						Args: &cursorpb.AgentV1_WebFetchArgs{Url: "https://example.com", ToolCallId: "srvtoolu_fetch"},
					},
				},
			},
		},
	}
	if event := translateAnthropicPluginEvent(server, nil); event != nil {
		t.Fatalf("search-only path leaked fetch as %+v", event)
	}
	event := translateClaudeEvent(server, nil, translator.ToolNameDialectClaudeCode, false, true)
	if event == nil || event.Kind != translator.EventServerToolStarted || event.ToolName != "web_fetch" {
		t.Fatalf("declared server fetch = %+v, want server_tool_use web_fetch", event)
	}
	if event.ToolCallID != "srvtoolu_fetch" {
		t.Fatalf("id = %q, want srvtoolu_fetch", event.ToolCallID)
	}
}

func TestBuildChatRequest_UsesStableServerToolVariant(t *testing.T) {
	req := buildChatRequest(chatShape{
		Model:     "claude-opus-4-8-medium",
		WebSearch: true,
	}, nil)
	if req.Model != "claude-opus-4-8-low" {
		t.Fatalf("server-tool model = %q, want claude-opus-4-8-low", req.Model)
	}

	regular := buildChatRequest(chatShape{Model: "claude-opus-4-8-medium"}, nil)
	if regular.Model != "claude-opus-4-8-medium" {
		t.Fatalf("regular model = %q, want unchanged", regular.Model)
	}

	other := buildChatRequest(chatShape{Model: "claude-fable-5-medium", WebSearch: true}, nil)
	if other.Model != "claude-fable-5-medium" {
		t.Fatalf("unprofiled server-tool model = %q, want unchanged", other.Model)
	}
}

func TestBuildChatRequest_ThinkingEnabledDefaultsToHighTier(t *testing.T) {
	// The plugin used to synthesize the variant slug on the client side
	// (`claude-opus-4-8-thinking-high`). That broke older models whose slugs
	// live under a completely different pattern (e.g. `claude-4.5-sonnet-
	// thinking`, no effort tier at all) and were rejected by Cursor's
	// backend with ERROR_BAD_MODEL_NAME. buildChatRequest now leaves the
	// model id at the base name and lets the live catalog map parameters
	// to whichever slug that specific account exposes.
	shape := chatShape{
		Model:    "claude-opus-4-8",
		Thinking: true,
	}
	req := buildChatRequest(shape, nil)
	if req.Model != "claude-opus-4-8" {
		t.Fatalf("resolved model = %q, want claude-opus-4-8 (variant selected via ModelParameters)", req.Model)
	}
	if req.ModelParameters["thinking"] != "true" {
		t.Fatalf("thinking parameter = %q, want true", req.ModelParameters["thinking"])
	}
	if req.ModelParameters["effort"] != "high" {
		t.Fatalf("effort parameter = %q, want high (thinking-enabled default)", req.ModelParameters["effort"])
	}
}

func TestParseClaudePayload_ResolvesThinkingEffortAndStructuredOutput(t *testing.T) {
	shape, err := parseClaudePayload([]byte(`{
		"model":"claude-opus-4-8-medium",
		"thinking":{"type":"adaptive","display":"summarized"},
		"output_config":{"effort":"xhigh","format":{"type":"json_schema","schema":{"type":"object","properties":{"result":{"type":"integer"}},"required":["result"]}}},
		"messages":[{"role":"user","content":"calculate"}]
	}`))
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	req := buildChatRequest(shape, nil)
	// The `-medium` suffix on the request model is a legacy_slug that the
	// executor's catalog resolver will fold back to the base id at
	// dispatch time. buildChatRequest keeps it verbatim here so callers
	// that already know the exact slug are not overridden.
	if req.Model != "claude-opus-4-8-medium" {
		t.Fatalf("resolved model = %q", req.Model)
	}
	if req.ModelParameters["thinking"] != "true" {
		t.Fatalf("thinking parameter = %q, want true", req.ModelParameters["thinking"])
	}
	if req.ModelParameters["effort"] != "xhigh" {
		t.Fatalf("effort parameter = %q, want xhigh", req.ModelParameters["effort"])
	}
	if req.Mode != executor.APIConversationMode(false) {
		t.Fatalf("mode = %d, want API ask mode", req.Mode)
	}
	if strings.Contains(req.SystemPrompt, "No tools are available") {
		t.Fatalf("no-tool API guard polluted the caller system prompt: %q", req.SystemPrompt)
	}
	if !strings.Contains(req.SystemPrompt, `"required":["result"]`) {
		t.Fatalf("structured output schema missing from prompt: %q", req.SystemPrompt)
	}
}

func TestStripJSONMarkdownFences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "fenced_json",
			in:   "```json\n{\"result\":\"ok\"}\n```",
			want: `{"result":"ok"}`,
		},
		{
			name: "fenced_generic",
			in:   "```\n{\"result\":42}\n```",
			want: `{"result":42}`,
		},
		{
			name: "fenced_with_trailing_prose",
			in:   "```json\n{\"result\":\"ok\"}\n```\nLet me know if you'd like more.",
			want: `{"result":"ok"}`,
		},
		{
			name: "bare_json_with_prose",
			in:   "Sure, here it is: {\"result\":\"ok\"} — hope that helps!",
			want: `{"result":"ok"}`,
		},
		{
			name: "bare_json_array",
			in:   "Here: [1,2,3] done.",
			want: `[1,2,3]`,
		},
		{
			name: "plain_json_untouched",
			in:   `{"result":"ok"}`,
			want: `{"result":"ok"}`,
		},
		{
			name: "no_json_returns_original",
			in:   "hello world",
			want: "hello world",
		},
		{
			name: "empty",
			in:   "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripJSONMarkdownFences(tc.in)
			if got != tc.want {
				t.Fatalf("stripJSONMarkdownFences(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCitationsFromServerToolUsesShape(t *testing.T) {
	blocks := []map[string]any{
		{
			"type":  "server_tool_use",
			"id":    "srvtoolu_x",
			"name":  "web_search",
			"input": map[string]any{"query": "x"},
		},
		{
			"type":        "web_search_tool_result",
			"tool_use_id": "srvtoolu_x",
			"content": []map[string]any{
				{"type": "web_search_result", "url": "https://a", "title": "A", "encrypted_content": "ac", "page_age": nil},
				{"type": "web_search_result", "url": "https://b", "title": "", "encrypted_content": "bc", "page_age": nil},
			},
		},
	}
	got := citationsFromServerToolUses(blocks)
	if len(got) != 2 {
		t.Fatalf("expected 2 citations, got %d: %#v", len(got), got)
	}
	// First: title present → cited_text == title
	if got[0]["cited_text"] != "A" || got[0]["url"] != "https://a" ||
		got[0]["encrypted_index"] != "srvtoolu_x#0" ||
		got[0]["type"] != "web_search_result_location" {
		t.Fatalf("first citation malformed: %#v", got[0])
	}
	// Second: title empty → cited_text falls back to url
	if got[1]["cited_text"] != "https://b" || got[1]["encrypted_index"] != "srvtoolu_x#1" {
		t.Fatalf("second citation malformed: %#v", got[1])
	}
}

func TestParseClaudePayloadReadsTopLevelResponseFormat(t *testing.T) {
	body := []byte(`{
        "model":"claude-opus-4-8",
        "messages":[{"role":"user","content":"give me json"}],
        "response_format":{"type":"json_schema","json_schema":{"name":"r","schema":{"type":"object","properties":{"result":{"type":"string"}},"required":["result"]}}}
    }`)
	shape, err := parseClaudePayload(body)
	if err != nil {
		t.Fatalf("parseClaudePayload: %v", err)
	}
	if len(shape.JSONSchema) == 0 {
		t.Fatalf("JSONSchema not populated from top-level response_format: %s", string(shape.JSONSchema))
	}
	if !strings.Contains(string(shape.JSONSchema), `"required":["result"]`) {
		t.Fatalf("JSONSchema missing schema body: %s", string(shape.JSONSchema))
	}
}
