package executor

import (
	"bytes"
	"encoding/binary"
	"io"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

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
		readSSEStream(body, events, false, autoStopOnToolCall)
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

// TestIsUserFacingToolCallStarted locks the "internal planning tool"
// filter used by AutoStopOnToolCall. Composer emits create_plan /
// update_todos / task / read_todos as internal orchestration moves
// before the actual user-facing tool (pi_write, pi_bash). Without
// this filter, AutoStopOnToolCall would fire on create_plan and
// close the SSE before the real tool arrived — reproduced by
// cursor2api on 2026-07-19 (sse-tool-use-a0b578d-report.md).
func TestIsUserFacingToolCallStarted_InternalPlanningIgnored(t *testing.T) {
	cases := []struct {
		name string
		tc   *cursorpb.AgentV1_ToolCall
	}{
		{"create_plan", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_CreatePlanToolCall{
				CreatePlanToolCall: &cursorpb.AgentV1_CreatePlanToolCall{},
			},
		}},
		{"update_todos", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_UpdateTodosToolCall{
				UpdateTodosToolCall: &cursorpb.AgentV1_UpdateTodosToolCall{},
			},
		}},
		{"read_todos", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_ReadTodosToolCall{
				ReadTodosToolCall: &cursorpb.AgentV1_ReadTodosToolCall{},
			},
		}},
		{"task", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_TaskToolCall{
				TaskToolCall: &cursorpb.AgentV1_TaskToolCall{},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &cursorpb.AgentV1_ToolCallStartedUpdate{ToolCall: tc.tc}
			if isUserFacingToolCallStarted(s) {
				t.Errorf("isUserFacingToolCallStarted(%s) = true, want false — "+
					"internal planning tool should NOT trigger SSE close", tc.name)
			}
		})
	}
}

// TestIsUserFacingToolCallStarted_RealToolsPass verifies user-facing
// tools (pi_write / shell / grep / edit / mcp / etc.) still trigger
// the AutoStopOnToolCall deadline correctly.
func TestIsUserFacingToolCallStarted_RealToolsPass(t *testing.T) {
	cases := []struct {
		name string
		tc   *cursorpb.AgentV1_ToolCall
	}{
		{"pi_write", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_PiWriteToolCall{
				PiWriteToolCall: &cursorpb.AgentV1_PiWriteToolCall{},
			},
		}},
		{"shell", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_ShellToolCall{
				ShellToolCall: &cursorpb.AgentV1_ShellToolCall{},
			},
		}},
		{"edit", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_EditToolCall{
				EditToolCall: &cursorpb.AgentV1_EditToolCall{},
			},
		}},
		{"mcp", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_McpToolCall{
				McpToolCall: &cursorpb.AgentV1_McpToolCall{},
			},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &cursorpb.AgentV1_ToolCallStartedUpdate{ToolCall: tc.tc}
			if !isUserFacingToolCallStarted(s) {
				t.Errorf("isUserFacingToolCallStarted(%s) = false, want true", tc.name)
			}
		})
	}
}

// TestIsUserFacingToolCallStarted_NilSafe guards against a nil
// ToolCallStartedUpdate or nested nil ToolCall crashing the caller.
func TestIsUserFacingToolCallStarted_NilSafe(t *testing.T) {
	if isUserFacingToolCallStarted(nil) {
		t.Error("nil update must return false")
	}
	if isUserFacingToolCallStarted(&cursorpb.AgentV1_ToolCallStartedUpdate{}) {
		t.Error("update with nil ToolCall must return false")
	}
}
