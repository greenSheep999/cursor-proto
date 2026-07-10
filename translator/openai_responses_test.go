package translator

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// splitFrames parses raw SSE bytes into an ordered slice of (event, data)
// pairs so tests can assert on the wire order without regex fragility.
type frame struct {
	Event string
	Data  map[string]any
}

func splitFrames(t *testing.T, raw []byte) []frame {
	t.Helper()
	var out []frame
	// Each frame is `event: X\ndata: {json}\n\n`
	blocks := bytes.Split(bytes.TrimSpace(raw), []byte("\n\n"))
	for _, blk := range blocks {
		if len(blk) == 0 {
			continue
		}
		lines := bytes.SplitN(blk, []byte("\n"), 2)
		if len(lines) != 2 {
			t.Fatalf("malformed frame: %q", string(blk))
		}
		if !bytes.HasPrefix(lines[0], []byte("event: ")) {
			t.Fatalf("expected event: prefix, got %q", string(lines[0]))
		}
		if !bytes.HasPrefix(lines[1], []byte("data: ")) {
			t.Fatalf("expected data: prefix, got %q", string(lines[1]))
		}
		event := string(bytes.TrimPrefix(lines[0], []byte("event: ")))
		data := map[string]any{}
		if err := json.Unmarshal(bytes.TrimPrefix(lines[1], []byte("data: ")), &data); err != nil {
			t.Fatalf("decode data: %v (frame=%q)", err, string(blk))
		}
		out = append(out, frame{Event: event, Data: data})
	}
	return out
}

func TestOpenAIResponsesStreamWriter_TextOnly(t *testing.T) {
	w := NewOpenAIResponsesStreamWriter("composer-2.5")
	var buf bytes.Buffer
	buf.Write(w.InitialFrames())
	buf.Write(w.Encode(&Event{Kind: EventTextDelta, Text: "Hello"}))
	buf.Write(w.Encode(&Event{Kind: EventTextDelta, Text: " world"}))
	buf.Write(w.Encode(&Event{Kind: EventTurnEnded, Usage: &Usage{
		InputTokens: 12, OutputTokens: 5, CacheReadTokens: 3,
	}}))

	frames := splitFrames(t, buf.Bytes())
	wantOrder := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",  // opens message item on first delta
		"response.content_part.added", // opens output_text part
		"response.output_text.delta",  // "Hello"
		"response.output_text.delta",  // " world"
		"response.output_text.done",   // finalises text on turn_ended
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if len(frames) != len(wantOrder) {
		t.Fatalf("frame count: got %d, want %d\nframes:\n%s", len(frames), len(wantOrder), string(buf.Bytes()))
	}
	for i, f := range frames {
		if f.Event != wantOrder[i] {
			t.Fatalf("frame[%d].Event = %q, want %q\nfull stream:\n%s", i, f.Event, wantOrder[i], string(buf.Bytes()))
		}
	}

	// sequence_number is monotonic and starts at 0.
	for i, f := range frames {
		seq, ok := f.Data["sequence_number"].(float64)
		if !ok {
			t.Fatalf("frame[%d] missing sequence_number: %+v", i, f.Data)
		}
		if int(seq) != i {
			t.Fatalf("frame[%d].sequence_number = %d, want %d", i, int(seq), i)
		}
	}

	// Deltas carry the expected text.
	delta1 := frames[4].Data["delta"].(string)
	delta2 := frames[5].Data["delta"].(string)
	if delta1 != "Hello" || delta2 != " world" {
		t.Fatalf("deltas: got %q + %q, want %q + %q", delta1, delta2, "Hello", " world")
	}

	// output_text.done and content_part.done both report full text.
	if got := frames[6].Data["text"].(string); got != "Hello world" {
		t.Fatalf("output_text.done text = %q, want %q", got, "Hello world")
	}

	// Completed frame includes usage.
	completed := frames[9].Data
	resp := completed["response"].(map[string]any)
	if resp["status"].(string) != "completed" {
		t.Fatalf("response.status = %v, want %q", resp["status"], "completed")
	}
	usage := resp["usage"].(map[string]any)
	if int(usage["input_tokens"].(float64)) != 12 {
		t.Fatalf("usage.input_tokens = %v, want 12", usage["input_tokens"])
	}
	if int(usage["output_tokens"].(float64)) != 5 {
		t.Fatalf("usage.output_tokens = %v, want 5", usage["output_tokens"])
	}
	inputDetails := usage["input_tokens_details"].(map[string]any)
	if int(inputDetails["cached_tokens"].(float64)) != 3 {
		t.Fatalf("usage.input_tokens_details.cached_tokens = %v, want 3", inputDetails["cached_tokens"])
	}

	// output[] contains the message with joined text.
	out := resp["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len = %d, want 1", len(out))
	}
	msg := out[0].(map[string]any)
	if msg["type"].(string) != "message" || msg["role"].(string) != "assistant" {
		t.Fatalf("output[0] wrong shape: %+v", msg)
	}
	content := msg["content"].([]any)
	if got := content[0].(map[string]any)["text"].(string); got != "Hello world" {
		t.Fatalf("output[0].content[0].text = %q, want %q", got, "Hello world")
	}
}

func TestOpenAIResponsesStreamWriter_ToolCall(t *testing.T) {
	w := NewOpenAIResponsesStreamWriter("composer-2.5")
	var buf bytes.Buffer
	buf.Write(w.InitialFrames())
	buf.Write(w.Encode(&Event{
		Kind:          EventToolCallStarted,
		ToolCallID:    "call_abc",
		ToolName:      "get_weather",
		ToolArgsDelta: `{"city":"SF"}`,
	}))
	// Simulate the SSE ending mid-tool-call (AutoStopOnToolCall). The
	// handler synthesises TurnEnded with no usage.
	buf.Write(w.Encode(&Event{Kind: EventTurnEnded}))

	frames := splitFrames(t, buf.Bytes())
	wantPrefix := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",             // opens function_call item
		"response.function_call_arguments.delta", // args on start
	}
	for i, want := range wantPrefix {
		if frames[i].Event != want {
			t.Fatalf("frame[%d].Event = %q, want %q", i, frames[i].Event, want)
		}
	}

	// Must eventually see arguments.done and output_item.done for the tool,
	// followed by response.completed.
	sawArgsDone := false
	sawItemDone := false
	for _, f := range frames {
		if f.Event == "response.function_call_arguments.done" {
			sawArgsDone = true
			if got := f.Data["arguments"].(string); got != `{"city":"SF"}` {
				t.Fatalf("arguments.done arguments = %q, want %q", got, `{"city":"SF"}`)
			}
		}
		if f.Event == "response.output_item.done" {
			item := f.Data["item"].(map[string]any)
			if item["type"].(string) == "function_call" {
				sawItemDone = true
				if item["status"].(string) != "completed" {
					t.Fatalf("function_call item status = %v, want completed", item["status"])
				}
				if item["call_id"].(string) != "call_abc" {
					t.Fatalf("function_call call_id = %v, want call_abc", item["call_id"])
				}
			}
		}
	}
	if !sawArgsDone {
		t.Fatalf("missing response.function_call_arguments.done frame\nstream:\n%s", string(buf.Bytes()))
	}
	if !sawItemDone {
		t.Fatalf("missing function_call response.output_item.done frame\nstream:\n%s", string(buf.Bytes()))
	}

	// Last frame is response.completed with the tool in output[].
	last := frames[len(frames)-1]
	if last.Event != "response.completed" {
		t.Fatalf("last frame event = %q, want response.completed", last.Event)
	}
	out := last.Data["response"].(map[string]any)["output"].([]any)
	if len(out) != 1 {
		t.Fatalf("output len = %d, want 1", len(out))
	}
	item := out[0].(map[string]any)
	if item["type"].(string) != "function_call" || item["name"].(string) != "get_weather" {
		t.Fatalf("output[0] wrong shape: %+v", item)
	}
}

func TestOpenAIResponsesStreamWriter_SequenceNumbersMonotonic(t *testing.T) {
	// Interleave text + a tool call in a longer stream and verify
	// sequence_number strictly increases by 1 per frame.
	w := NewOpenAIResponsesStreamWriter("m")
	var buf bytes.Buffer
	buf.Write(w.InitialFrames())
	buf.Write(w.Encode(&Event{Kind: EventTextDelta, Text: "a"}))
	buf.Write(w.Encode(&Event{Kind: EventTextDelta, Text: "b"}))
	buf.Write(w.Encode(&Event{
		Kind:       EventToolCallStarted,
		ToolCallID: "c1",
		ToolName:   "t",
	}))
	buf.Write(w.Encode(&Event{
		Kind:          EventToolCallDelta,
		ToolCallID:    "c1",
		ToolArgsDelta: `{"k":`,
	}))
	buf.Write(w.Encode(&Event{
		Kind:          EventToolCallDelta,
		ToolCallID:    "c1",
		ToolArgsDelta: `1}`,
	}))
	buf.Write(w.Encode(&Event{Kind: EventToolCallCompleted, ToolCallID: "c1"}))
	buf.Write(w.Encode(&Event{Kind: EventTurnEnded}))

	frames := splitFrames(t, buf.Bytes())
	for i, f := range frames {
		got := int(f.Data["sequence_number"].(float64))
		if got != i {
			t.Fatalf("frame[%d] (event=%s) sequence_number = %d, want %d", i, f.Event, got, i)
		}
	}
	// Sanity: FinalCompletedFrame after Encode has already emitted the
	// completed event must return nil.
	if extra := w.FinalCompletedFrame(); extra != nil {
		t.Fatalf("FinalCompletedFrame after Encode should be nil, got %q", string(extra))
	}
}

func TestResponsesNonStreamingAccumulator(t *testing.T) {
	acc := &ResponsesNonStreamingAccumulator{Model: "m"}
	acc.Consume(&Event{Kind: EventTextDelta, Text: "Hello, "})
	acc.Consume(&Event{Kind: EventTextDelta, Text: "world"})
	acc.Consume(&Event{
		Kind:          EventToolCallStarted,
		ToolCallID:    "call_1",
		ToolName:      "search",
		ToolArgsDelta: `{"q":"go"}`,
	})
	acc.Consume(&Event{Kind: EventTurnEnded, Usage: &Usage{InputTokens: 10, OutputTokens: 4}})

	body := acc.Response("resp_test")
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode response: %v\n%s", err, string(body))
	}
	if got["object"].(string) != "response" {
		t.Fatalf("object = %v, want response", got["object"])
	}
	if got["status"].(string) != "completed" {
		t.Fatalf("status = %v, want completed", got["status"])
	}
	if !strings.HasPrefix(got["id"].(string), "resp_") {
		t.Fatalf("id = %v, want resp_ prefix", got["id"])
	}
	out := got["output"].([]any)
	if len(out) != 2 {
		t.Fatalf("output len = %d, want 2\nbody: %s", len(out), string(body))
	}
	msg := out[0].(map[string]any)
	if msg["type"].(string) != "message" {
		t.Fatalf("output[0].type = %v, want message", msg["type"])
	}
	if got := msg["content"].([]any)[0].(map[string]any)["text"].(string); got != "Hello, world" {
		t.Fatalf("assistant text = %q, want %q", got, "Hello, world")
	}
	tool := out[1].(map[string]any)
	if tool["type"].(string) != "function_call" {
		t.Fatalf("output[1].type = %v, want function_call", tool["type"])
	}
	if tool["arguments"].(string) != `{"q":"go"}` {
		t.Fatalf("function_call arguments = %q, want %q", tool["arguments"], `{"q":"go"}`)
	}
	if tool["name"].(string) != "search" {
		t.Fatalf("function_call name = %v, want search", tool["name"])
	}
	usage := got["usage"].(map[string]any)
	if int(usage["input_tokens"].(float64)) != 10 || int(usage["output_tokens"].(float64)) != 4 {
		t.Fatalf("usage wrong: %+v", usage)
	}
}
