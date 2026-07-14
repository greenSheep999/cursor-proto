package main

// SSE + non-streaming aggregation helpers for /v1/agents/*.
// Isolated from agents_handler.go so the routing logic stays
// readable and the streaming details can evolve independently.
//
// SSE format we emit:
//
//   event: run.event
//   data: {<raw SDK event JSON>}
//
//   event: run.done
//   data: {"final_text": "...", "usage": {...}}
//
//   event: run.error
//   data: {"code": -32004, "message": "..."}
//
// One event per SDK notification; each event ends with a blank
// line (SSE spec). We do NOT set `id:` fields yet — Phase 4 adds
// a per-event monotonic id and Last-Event-Id reconnect support.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/router-for-me/cursor-proto/executor/sdk"
)

// contextWithTimeout returns a derived context with an added deadline.
// Separate helper because our SSE / non-stream paths differ in
// whether they want a bounded deadline at all.
func contextWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(parent, d)
}

// streamSSE writes the standard SSE framing over w for one run's
// event channel. Blocks until the channel closes (run.done /
// run.error) or the caller's context cancels.
//
// Preconditions: response headers not yet written. streamSSE sets
// Content-Type + streaming headers itself, then flushes each event.
func streamSSE(ctx context.Context, w http.ResponseWriter, runID string, events <-chan sdk.RunStreamMsg) {
	flusher, canFlush := w.(http.Flusher)
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("x-accel-buffering", "no") // disable nginx buffering when proxied
	w.WriteHeader(http.StatusOK)

	// Emit a one-shot preamble carrying the runId so a client that
	// consumed only headers can still act on cancellation before
	// the first real event arrives.
	sseWrite(w, "run.started", map[string]any{"run_id": runID})
	if canFlush {
		flusher.Flush()
	}

	for {
		select {
		case msg, ok := <-events:
			if !ok {
				// Channel closed without run.done/run.error — the
				// supervisor closed our subscription (Close() or
				// process exit). Emit a terminal marker so clients
				// don't loop waiting.
				sseWrite(w, "run.closed", map[string]any{
					"run_id": runID,
					"reason": "supervisor_closed",
				})
				if canFlush {
					flusher.Flush()
				}
				return
			}
			writeRunMsgSSE(w, msg)
			if canFlush {
				flusher.Flush()
			}
			// Return on terminal states — the channel will also close
			// after these, but returning explicitly saves one loop
			// iteration and makes the exit path obvious.
			if msg.Done != nil || msg.Error != nil {
				return
			}
		case <-ctx.Done():
			// Caller disconnected. Emit best-effort cancel notice and
			// bail — we don't cancel the run itself here (client can
			// hit /cancel explicitly if they want to abort work).
			return
		}
	}
}

// writeRunMsgSSE serializes one RunStreamMsg as an SSE frame. Exactly
// one of Event / Done / Error is populated per contract; if somehow
// none are we skip silently.
func writeRunMsgSSE(w http.ResponseWriter, msg sdk.RunStreamMsg) {
	switch {
	case msg.Event != nil:
		// Event.Event is opaque raw JSON from the SDK. Write it
		// through unchanged — the caller (Claude Code, cursor2api's
		// UI) decides how to render.
		sseWriteRaw(w, "run.event", msg.Event.Event)
	case msg.Done != nil:
		sseWrite(w, "run.done", msg.Done)
	case msg.Error != nil:
		sseWrite(w, "run.error", msg.Error)
	}
}

// sseWrite marshals v to JSON and writes an SSE `event: <name>` +
// `data: <json>` pair, followed by a blank line to complete the
// frame.
func sseWrite(w http.ResponseWriter, event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		// Fall back to a diagnostic frame — losing an event silently
		// is worse than an obviously malformed one.
		fmt.Fprintf(w, "event: run.error\ndata: {\"code\":-32603,\"message\":\"internal marshal error: %s\"}\n\n", err.Error())
		return
	}
	sseWriteRaw(w, event, b)
}

// sseWriteRaw skips the marshal step when the caller already has a
// JSON blob to emit (used for SDK-opaque event payloads).
func sseWriteRaw(w http.ResponseWriter, event string, data []byte) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
}

// -------- non-streaming aggregation --------

// nonStreamResponse is the JSON body /v1/agents/{id}/runs returns
// when stream=false. Content aggregates all `assistant` deltas into
// one text blob; tool calls emitted before end_turn are surfaced
// as a parallel array so downstream can still render them.
type nonStreamResponse struct {
	RunID     string          `json:"run_id"`
	FinalText string          `json:"final_text"`
	ToolCalls []toolCallEntry `json:"tool_calls,omitempty"`
	Usage     json.RawMessage `json:"usage,omitempty"`
	Error     *runErrorBody   `json:"error,omitempty"`
}

type toolCallEntry struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type runErrorBody struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// streamNonStreaming drains the events channel and produces one
// JSON body. Blocks until run.done / run.error / channel close.
// If the caller's ctx cancels, we return an error body rather than
// leaving the connection hanging.
func streamNonStreaming(ctx context.Context, w http.ResponseWriter, runID string, events <-chan sdk.RunStreamMsg) {
	var (
		textBuf   []byte
		toolCalls []toolCallEntry
		usage     json.RawMessage
		errBody   *runErrorBody
	)

	// Type of one SDK event, minimally destructured for the fields
	// we accumulate. Anything else (thinking, task, status) is
	// dropped silently for the non-streaming shape — clients that
	// need it should use stream=true.
	type sdkEvent struct {
		Type  string          `json:"type"`
		Delta string          `json:"delta"`
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}

	for {
		select {
		case msg, ok := <-events:
			if !ok {
				goto finish
			}
			switch {
			case msg.Event != nil:
				var ev sdkEvent
				if err := json.Unmarshal(msg.Event.Event, &ev); err != nil {
					// Unknown event shape — skip; not fatal.
					continue
				}
				switch ev.Type {
				case "assistant":
					textBuf = append(textBuf, []byte(ev.Delta)...)
				case "tool_call":
					toolCalls = append(toolCalls, toolCallEntry{
						Name:  ev.Name,
						Input: ev.Input,
					})
				}
			case msg.Done != nil:
				if msg.Done.FinalText != "" {
					// FinalText, when present, replaces the accumulated
					// deltas — the SDK sometimes provides a canonical
					// full-text at end_turn.
					textBuf = []byte(msg.Done.FinalText)
				}
				usage = msg.Done.Usage
				goto finish
			case msg.Error != nil:
				errBody = &runErrorBody{
					Code:    msg.Error.Code,
					Message: msg.Error.Message,
				}
				goto finish
			}
		case <-ctx.Done():
			errBody = &runErrorBody{
				Code:    -1,
				Message: fmt.Sprintf("client disconnected: %v", context.Cause(ctx)),
			}
			goto finish
		}
	}

finish:
	resp := nonStreamResponse{
		RunID:     runID,
		FinalText: string(textBuf),
		ToolCalls: toolCalls,
		Usage:     usage,
		Error:     errBody,
	}
	w.Header().Set("content-type", "application/json")
	status := http.StatusOK
	if errBody != nil {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(resp)
}

// -------- (unused import guard) --------

// errors import is used indirectly through sdk.RPCError type assertions
// in agents_handler.go; keep it referenced here so goimports doesn't
// prune it if a refactor tightens the handler.
var _ = errors.New
