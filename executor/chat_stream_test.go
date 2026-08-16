package executor

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/cursor-proto/auth"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// TestReadSSEStream_AutoStopOnToolCall_InteractionUpdate locks in the v0.3.2
// fix: Cursor's modern generic tool-call branch emits ToolCallStarted on the
// InteractionUpdate path, not on ExecServerMessage.McpArgs. Watching only the
// legacy MCP branch left OpenAI-compat callers hanging for the full 60s
// heartbeat deadline. This test drives a synthetic frame carrying only the
// InteractionUpdate branch and asserts the stream closes within the grace
// window.
func TestReadSSEStream_AutoStopOnToolCall_InteractionUpdate(t *testing.T) {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &cursorpb.AgentV1_ToolCallStartedUpdate{},
				},
			},
		},
	}
	body := frameForTest(t, msg)

	closed, count := runReadSSE(body, true /*autoStopOnToolCall*/)

	select {
	case n := <-closed:
		if n == 0 {
			t.Fatal("expected at least one ChatEvent, got 0")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("readSSEStream did not close within 3s after ToolCallStarted; " +
			"chat.go:300 must watch InteractionUpdate.ToolCallStarted, not " +
			"just ExecServerMessage.McpArgs")
	}
	_ = count
}

// TestReadSSEStream_AutoStopOnToolCall_McpArgs guards the legacy MCP branch so
// the fix does not regress it.
func TestReadSSEStream_AutoStopOnToolCall_McpArgs(t *testing.T) {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_ExecServerMessage{
			ExecServerMessage: &cursorpb.AgentV1_ExecServerMessage{
				Message: &cursorpb.AgentV1_ExecServerMessage_McpArgs{
					McpArgs: &cursorpb.AgentV1_McpArgs{},
				},
			},
		},
	}
	body := frameForTest(t, msg)

	closed, _ := runReadSSE(body, true)

	select {
	case <-closed:
	case <-time.After(3 * time.Second):
		t.Fatal("readSSEStream did not close after McpArgs (legacy MCP branch)")
	}
}

// frameForTest wraps a proto message in one Connect frame (1-byte flags + 4B
// big-endian length + payload). Matches splitConnectFrame's expectations.
func frameForTest(t *testing.T, msg proto.Message) io.ReadCloser {
	t.Helper()
	payload, err := proto.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal test message: %v", err)
	}
	buf := make([]byte, 5+len(payload))
	buf[0] = 0x00
	binary.BigEndian.PutUint32(buf[1:5], uint32(len(payload)))
	copy(buf[5:], payload)
	return io.NopCloser(bytes.NewReader(buf))
}

// runReadSSE drives readSSEStream against `body` and drains the event channel
// inline. It returns a channel that fires (with the event count) once
// readSSEStream has fully returned AND the drain has completed — that single
// signal is race-free because the drain runs in the same goroutine as the
// send on the result channel. autoStopOnToolCall is the flag under test;
// autoStopOnTurnEnd is left false so it doesn't confound the deadline.
func runReadSSE(body io.ReadCloser, autoStopOnToolCall bool) (<-chan int, int) {
	events := make(chan ChatEvent, 16)
	result := make(chan int, 1)
	go func() {
		readSSEStream(body, events, false, autoStopOnToolCall, nil, false)
	}()
	go func() {
		count := 0
		for range events {
			count++
		}
		result <- count
	}()
	return result, 0
}

func TestBuildInteractionResponseApproved_WebSearch(t *testing.T) {
	query := &cursorpb.AgentV1_InteractionQuery{
		Id: 42,
		Query: &cursorpb.AgentV1_InteractionQuery_WebSearchRequestQuery{
			WebSearchRequestQuery: &cursorpb.AgentV1_WebSearchRequestQuery{},
		},
	}
	got := buildInteractionResponseApproved(query)
	want := []byte{0x32, 0x06, 0x08, 0x2a, 0x12, 0x02, 0x0a, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("approved response = %x, want %x", got, want)
	}
}

func TestBuildInteractionResponseApproved_WebSearchOmitsZeroID(t *testing.T) {
	query := &cursorpb.AgentV1_InteractionQuery{
		Query: &cursorpb.AgentV1_InteractionQuery_WebSearchRequestQuery{
			WebSearchRequestQuery: &cursorpb.AgentV1_WebSearchRequestQuery{},
		},
	}
	got := buildInteractionResponseApproved(query)
	want := []byte{0x32, 0x04, 0x12, 0x02, 0x0a, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("approved response = %x, want %x", got, want)
	}
}

func TestBidiAppendUsesConnectEnvelope(t *testing.T) {
	t.Helper()
	type capturedRequest struct {
		contentType string
		body        []byte
	}
	captured := make(chan capturedRequest, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		captured <- capturedRequest{contentType: r.Header.Get("content-type"), body: body}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client := NewClient(&auth.Account{AccessToken: "test-token"})
	client.API2 = server.URL
	if err := client.bidiAppend(context.Background(), "request-id", 1, []byte{0x32, 0x00}); err != nil {
		t.Fatalf("bidiAppend: %v", err)
	}

	request := <-captured
	if request.contentType != "application/grpc-web+proto" {
		t.Fatalf("content-type = %q, want application/grpc-web+proto", request.contentType)
	}
	payload, trailer, rest, ok := splitConnectFrame(request.body)
	if !ok || trailer || len(rest) != 0 {
		t.Fatalf("BidiAppend body is not one Connect data frame: ok=%v trailer=%v rest=%d body=%x", ok, trailer, len(rest), request.body)
	}
	var appendRequest cursorpb.AiserverV1_BidiAppendRequest
	if err := proto.Unmarshal(payload, &appendRequest); err != nil {
		t.Fatalf("unmarshal framed BidiAppendRequest: %v", err)
	}
	if appendRequest.GetRequestId().GetRequestId() != "request-id" || appendRequest.GetAppendSeqno() != 1 {
		t.Fatalf("unexpected BidiAppend request: %v", &appendRequest)
	}
	if appendRequest.GetData() != "" {
		t.Fatalf("data field = %q, want empty when data_binary is used", appendRequest.GetData())
	}
	if !bytes.Equal(appendRequest.GetDataBinary(), []byte{0x32, 0x00}) {
		t.Fatalf("data_binary = %x, want 3200", appendRequest.GetDataBinary())
	}
}

func TestReadSSEStream_DoesNotStopForEnabledWebSearch(t *testing.T) {
	msg := &cursorpb.AgentV1_AgentServerMessage{
		Message: &cursorpb.AgentV1_AgentServerMessage_InteractionUpdate{
			InteractionUpdate: &cursorpb.AgentV1_InteractionUpdate{
				Message: &cursorpb.AgentV1_InteractionUpdate_ToolCallStarted{
					ToolCallStarted: &cursorpb.AgentV1_ToolCallStartedUpdate{
						ToolCall: &cursorpb.AgentV1_ToolCall{
							Tool: &cursorpb.AgentV1_ToolCall_WebSearchToolCall{
								WebSearchToolCall: &cursorpb.AgentV1_WebSearchToolCall{},
							},
						},
					},
				},
			},
		},
	}
	events := make(chan ChatEvent, 4)
	done := make(chan struct{})
	go func() {
		readSSEStream(frameForTest(t, msg), events, false, true, nil, true)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("finite test body should finish without waiting for tool-call grace deadline")
	}
}

// Historical: TestIsUserFacingToolCallStarted_* used to lock a
// filter that ignored Composer's internal planning tools
// (create_plan / update_todos / read_todos / task) in
// AutoStopOnToolCall. That filter caused SSE hangs — Composer
// waits for a create_plan_response tool_result from the client,
// but skipping autoStop meant we never surfaced the tool_call
// back to the client, so nothing ever responded. Reverted here;
// autoStop now fires on any ToolCallStarted (matches a0b578d).
