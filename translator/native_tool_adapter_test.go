package translator

import (
	"encoding/json"
	"strings"
	"testing"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// TestClientNameForCursorTool locks in the tool-name mapping the
// Claude Code / Cline / cursor2api harness expects. Any change to
// this table is a wire contract change — bump the downstream
// harness in lockstep.
func TestClientNameForCursorTool(t *testing.T) {
	cases := []struct {
		cursorType string
		want       string
	}{
		{"shell", "Bash"},
		{"read", "Read"},
		{"edit", "Write"},
		{"grep", "Grep"},
		{"ls", "LS"},
		{"glob", "Glob"},
		{"delete", "Bash"},
		{"fetch", "WebFetch"},
		{"ask_question", "AskQuestion"},
		// Unknown types fall back to the raw cursorType — Grok
		// variants land here so the client sees at least a name
		// to log, not "".
		{"custom-experimental-tool", "custom-experimental-tool"},
	}
	for _, tc := range cases {
		got := clientNameForCursorTool(tc.cursorType)
		if got != tc.want {
			t.Errorf("clientNameForCursorTool(%q) = %q, want %q", tc.cursorType, got, tc.want)
		}
	}
}

// TestMapNativeToolArgsJSON_Shell verifies the working_directory ->
// description convention. Claude Code Bash doesn't have a cwd
// field, and the JS reference (sessionManager.js:246-250) folds
// cwd into the description so the client at least surfaces it.
func TestMapNativeToolArgsJSON_Shell(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_ShellToolCall{
			ShellToolCall: &cursorpb.AgentV1_ShellToolCall{
				Args: &cursorpb.AgentV1_ShellArgs{
					Command:          "ls -la",
					WorkingDirectory: "/tmp",
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v (raw=%s)", err, got)
	}
	if parsed["command"] != "ls -la" {
		t.Errorf("command = %v, want ls -la", parsed["command"])
	}
	if got, ok := parsed["description"].(string); !ok || !strings.Contains(got, "/tmp") {
		t.Errorf("description = %v, want to mention /tmp", parsed["description"])
	}
	// Cursor-internal fields (parsing_result, sandbox_policy,
	// simple_commands, timeout_behavior…) must be dropped — they'd
	// confuse the client's Bash schema.
	for _, junk := range []string{"working_directory", "workingDirectory", "parsing_result", "simple_commands", "timeout_behavior", "sandbox_policy"} {
		if _, present := parsed[junk]; present {
			t.Errorf("unexpected internal field %q leaked into client shape", junk)
		}
	}
}

// TestMapNativeToolArgsJSON_Glob is the exact downstream bug report:
// Cursor's field is `glob_pattern`, client's Claude Code Glob schema
// wants `pattern`. Without this rename the client sees:
//   {"globPattern":"**/*.go","targetDirectory":"/foo"}
// and rejects the tool_use for missing `pattern`.
func TestMapNativeToolArgsJSON_Glob(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_GlobToolCall{
			GlobToolCall: &cursorpb.AgentV1_GlobToolCall{
				Args: &cursorpb.AgentV1_GlobToolArgs{
					GlobPattern:     "**/*.go",
					TargetDirectory: stringPtr("/foo"),
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v (raw=%s)", err, got)
	}
	if parsed["pattern"] != "**/*.go" {
		t.Errorf("pattern = %v, want **/*.go (glob_pattern must be renamed)", parsed["pattern"])
	}
	if parsed["path"] != "/foo" {
		t.Errorf("path = %v, want /foo (target_directory must be renamed)", parsed["path"])
	}
	if _, leaked := parsed["glob_pattern"]; leaked {
		t.Error("Cursor internal glob_pattern leaked into client shape")
	}
	if _, leaked := parsed["globPattern"]; leaked {
		t.Error("Cursor internal globPattern (camelCase) leaked into client shape")
	}
}

// TestMapNativeToolArgsJSON_Edit swaps Cursor's { path, stream_content }
// to Claude's { file_path, content }. Client rejects tool_use if
// file_path is missing.
func TestMapNativeToolArgsJSON_Edit(t *testing.T) {
	body := "console.log('hi')\n"
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_EditToolCall{
			EditToolCall: &cursorpb.AgentV1_EditToolCall{
				Args: &cursorpb.AgentV1_EditArgs{
					Path:          "/tmp/x.js",
					StreamContent: &body,
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v (raw=%s)", err, got)
	}
	if parsed["file_path"] != "/tmp/x.js" {
		t.Errorf("file_path = %v, want /tmp/x.js", parsed["file_path"])
	}
	if parsed["content"] != body {
		t.Errorf("content mismatch: %v", parsed["content"])
	}
	for _, leaked := range []string{"path", "stream_content", "streamContent"} {
		if _, ok := parsed[leaked]; ok {
			t.Errorf("Cursor internal field %q leaked into Write shape", leaked)
		}
	}
}

// TestMapNativeToolArgsJSON_Grep — Cursor field names are already
// compatible with Claude Code Grep (pattern / path / glob), so this
// mostly verifies we don't drop them. Optional fields stay absent
// when not populated.
func TestMapNativeToolArgsJSON_Grep(t *testing.T) {
	path := "/src"
	glob := "*.go"
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_GrepToolCall{
			GrepToolCall: &cursorpb.AgentV1_GrepToolCall{
				Args: &cursorpb.AgentV1_GrepArgs{
					Pattern: "TODO",
					Path:    &path,
					Glob:    &glob,
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if parsed["pattern"] != "TODO" {
		t.Errorf("pattern = %v", parsed["pattern"])
	}
	if parsed["path"] != "/src" {
		t.Errorf("path = %v", parsed["path"])
	}
	if parsed["glob"] != "*.go" {
		t.Errorf("glob = %v", parsed["glob"])
	}
}

// TestProbeUnknownToolName is the Grok variant safety net. When
// Cursor's ToolCall carries a tool type this build doesn't
// enumerate in extractToolName, we should surface *something* the
// client can log instead of "" (which every harness rejects).
//
// The current proto's ToolCall doesn't carry a top-level name
// string field, so this test just documents the fallback behaviour
// — if a future proto revision adds `name`/`tool_name`/`type` on
// the envelope, probeUnknownToolName will find it. Today the
// function returns "" for an empty ToolCall, which is what the
// caller uses as its "give up" signal.
func TestProbeUnknownToolName_EmptyEnvelope(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{}
	if got := probeUnknownToolName(tc); got != "" {
		t.Errorf("probeUnknownToolName(empty) = %q, want empty", got)
	}
	if got := probeUnknownToolName(nil); got != "" {
		t.Errorf("probeUnknownToolName(nil) = %q, want empty", got)
	}
}

// TestExtractToolName_NativeReturnsClientName is the higher-level
// end-to-end for the whole switch — every native branch must
// produce a client-visible name, never the Cursor internal one.
func TestExtractToolName_NativeReturnsClientName(t *testing.T) {
	cases := []struct {
		name string
		tc   *cursorpb.AgentV1_ToolCall
		want string
	}{
		{"shell", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_ShellToolCall{ShellToolCall: &cursorpb.AgentV1_ShellToolCall{}}}, "Bash"},
		{"read", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_ReadToolCall{ReadToolCall: &cursorpb.AgentV1_ReadToolCall{}}}, "Read"},
		{"edit", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_EditToolCall{EditToolCall: &cursorpb.AgentV1_EditToolCall{}}}, "Write"},
		{"grep", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_GrepToolCall{GrepToolCall: &cursorpb.AgentV1_GrepToolCall{}}}, "Grep"},
		{"ls", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_LsToolCall{LsToolCall: &cursorpb.AgentV1_LsToolCall{}}}, "LS"},
		{"glob", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_GlobToolCall{GlobToolCall: &cursorpb.AgentV1_GlobToolCall{}}}, "Glob"},
		{"delete", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_DeleteToolCall{DeleteToolCall: &cursorpb.AgentV1_DeleteToolCall{}}}, "Bash"},
		{"fetch", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_FetchToolCall{FetchToolCall: &cursorpb.AgentV1_FetchToolCall{}}}, "WebFetch"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := extractToolName(tc.tc)
			if got != tc.want {
				t.Errorf("extractToolName(%s) = %q, want %q", tc.name, got, tc.want)
			}
		})
	}
}

// stringPtr is the pointer-taking helper protobuf oneof fields want
// for optional strings. Inlined here so the test file doesn't need
// its own utility import.
func stringPtr(s string) *string {
	return &s
}
