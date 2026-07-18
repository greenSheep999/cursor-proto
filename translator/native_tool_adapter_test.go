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
		{"shell", "bash"},
		{"read", "read"},
		{"edit", "write"},
		{"grep", "grep"},
		{"ls", "ls"},
		{"glob", "glob"},
		{"delete", "bash"},
		{"fetch", "web_fetch"},
		{"ask_question", "ask_question"},
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
//
//	{"globPattern":"**/*.go","targetDirectory":"/foo"}
//
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
		{"shell", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_ShellToolCall{ShellToolCall: &cursorpb.AgentV1_ShellToolCall{}}}, "bash"},
		{"read", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_ReadToolCall{ReadToolCall: &cursorpb.AgentV1_ReadToolCall{}}}, "read"},
		{"edit", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_EditToolCall{EditToolCall: &cursorpb.AgentV1_EditToolCall{}}}, "write"},
		{"grep", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_GrepToolCall{GrepToolCall: &cursorpb.AgentV1_GrepToolCall{}}}, "grep"},
		{"ls", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_LsToolCall{LsToolCall: &cursorpb.AgentV1_LsToolCall{}}}, "ls"},
		{"glob", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_GlobToolCall{GlobToolCall: &cursorpb.AgentV1_GlobToolCall{}}}, "glob"},
		{"delete", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_DeleteToolCall{DeleteToolCall: &cursorpb.AgentV1_DeleteToolCall{}}}, "bash"},
		{"fetch", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_FetchToolCall{FetchToolCall: &cursorpb.AgentV1_FetchToolCall{}}}, "web_fetch"},
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

// TestClientNameForCursorTool_PiFamily locks in the Pi* mapping.
// This is the fix for the downstream 2026-07-18 report — Composer
// emits PiWriteToolCall for a client-declared write_file, and
// without these entries the name came out as "".
func TestClientNameForCursorTool_PiFamily(t *testing.T) {
	cases := []struct {
		cursorType string
		want       string
	}{
		{"pi_write", "write"},
		{"pi_bash", "bash"},
		{"pi_edit", "write"},
		{"pi_read", "read"},
		{"pi_find", "glob"},
		{"pi_grep", "grep"},
		{"pi_ls", "ls"},
	}
	for _, tc := range cases {
		if got := clientNameForCursorTool(tc.cursorType); got != tc.want {
			t.Errorf("clientNameForCursorTool(%q) = %q, want %q", tc.cursorType, got, tc.want)
		}
	}
}

// TestMapNativeToolArgsJSON_PiWrite is the exact downstream
// regression: Composer's PiWriteToolCall carries { path, content }
// and the client's write_file expects { file_path, content }. The
// pre-fix build emitted "{}" here; this test guards that we
// project both fields into the Claude Code Write schema.
func TestMapNativeToolArgsJSON_PiWrite(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_PiWriteToolCall{
			PiWriteToolCall: &cursorpb.AgentV1_PiWriteToolCall{
				Args: &cursorpb.AgentV1_PiWriteToolArgs{
					Path:    "/tmp/note.txt",
					Content: "4271",
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v (raw=%s)", err, got)
	}
	if parsed["file_path"] != "/tmp/note.txt" {
		t.Errorf("file_path = %v, want /tmp/note.txt", parsed["file_path"])
	}
	if parsed["content"] != "4271" {
		t.Errorf("content = %v, want 4271", parsed["content"])
	}
	if _, leaked := parsed["path"]; leaked {
		t.Error("Cursor internal `path` leaked into Write shape (should be file_path)")
	}
}

// TestMapNativeToolArgsJSON_PiBash covers Composer's PiBashToolCall
// path — { command, timeout? } wants { command, timeout_ms? }
// (Claude Code Bash uses milliseconds; Cursor uses seconds float).
func TestMapNativeToolArgsJSON_PiBash(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_PiBashToolCall{
			PiBashToolCall: &cursorpb.AgentV1_PiBashToolCall{
				Args: &cursorpb.AgentV1_PiBashToolArgs{
					Command: "ls -la",
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if parsed["command"] != "ls -la" {
		t.Errorf("command = %v", parsed["command"])
	}
}

// TestExtractToolName_PiFamily is the end-to-end for the extractor
// side of the fix — every Pi* branch must produce a non-empty
// client-visible name.
func TestExtractToolName_PiFamily(t *testing.T) {
	cases := []struct {
		name string
		tc   *cursorpb.AgentV1_ToolCall
		want string
	}{
		{"pi_write", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_PiWriteToolCall{PiWriteToolCall: &cursorpb.AgentV1_PiWriteToolCall{}}}, "write"},
		{"pi_bash", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_PiBashToolCall{PiBashToolCall: &cursorpb.AgentV1_PiBashToolCall{}}}, "bash"},
		{"pi_edit", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_PiEditToolCall{PiEditToolCall: &cursorpb.AgentV1_PiEditToolCall{}}}, "write"},
		{"pi_read", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_PiReadToolCall{PiReadToolCall: &cursorpb.AgentV1_PiReadToolCall{}}}, "read"},
		{"pi_find", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_PiFindToolCall{PiFindToolCall: &cursorpb.AgentV1_PiFindToolCall{}}}, "glob"},
		{"pi_grep", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_PiGrepToolCall{PiGrepToolCall: &cursorpb.AgentV1_PiGrepToolCall{}}}, "grep"},
		{"pi_ls", &cursorpb.AgentV1_ToolCall{Tool: &cursorpb.AgentV1_ToolCall_PiLsToolCall{PiLsToolCall: &cursorpb.AgentV1_PiLsToolCall{}}}, "ls"},
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

// TestExtractToolName_UnknownReturnsBranchName guards the raw-name
// fallback. When Cursor emits a tool variant we don't enumerate
// (CreatePlan, Task, SemSearch, etc.), extractToolName MUST return
// the branch name — not "" — so the client at least logs
// something. This is the second downstream ask from the
// 2026-07-18 report.
func TestExtractToolName_UnknownReturnsBranchName(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_CreatePlanToolCall{
			CreatePlanToolCall: &cursorpb.AgentV1_CreatePlanToolCall{},
		},
	}
	got := extractToolName(tc)
	if got == "" {
		t.Fatalf("extractToolName(CreatePlan) = %q, want non-empty branch name — client rejects empty tool names outright", got)
	}
	// Exact spelling is implementation detail; just require the
	// branch label is present.
	if !strings.Contains(strings.ToLower(got), "createplan") {
		t.Errorf("extractToolName(CreatePlan) = %q, want to contain 'createplan'", got)
	}
}

// TestInternalPlanningToolAsText — Composer's first move on every
// prompt is a createPlan tool call. No client harness (opencode,
// claude-code, codex) declares createPlan, so they reject the tool
// use and loop until timeout. Downstream reported this as the
// remaining blocker on 2026-07-18. The fix: intercept the internal
// planning tools at translator layer and synthesize an assistant
// text delta instead — the plan surfaces as narration, the model
// self-continues into the actual Pi execution on the next turn.
func TestInternalPlanningToolAsText_CreatePlan(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_CreatePlanToolCall{
			CreatePlanToolCall: &cursorpb.AgentV1_CreatePlanToolCall{
				Args: &cursorpb.AgentV1_CreatePlanArgs{
					Name:     "Write note",
					Overview: "Create note.txt with '4271'.",
					Todos: []*cursorpb.AgentV1_TodoItem{
						{Content: "check dir", Status: cursorpb.AgentV1_TodoStatus(1)},
						{Content: "write file", Status: cursorpb.AgentV1_TodoStatus(1)},
					},
				},
			},
		},
	}
	text, isInternal := internalPlanningToolAsText(tc)
	if !isInternal {
		t.Fatal("CreatePlan should be flagged as internal planning tool")
	}
	if !strings.Contains(text, "Write note") {
		t.Errorf("rendered text missing plan name: %q", text)
	}
	if !strings.Contains(text, "Create note.txt") {
		t.Errorf("rendered text missing overview: %q", text)
	}
	if !strings.Contains(text, "check dir") {
		t.Errorf("rendered text missing todo: %q", text)
	}
	if !strings.Contains(text, "write file") {
		t.Errorf("rendered text missing todo: %q", text)
	}
}

// TestInternalPlanningToolAsText_NonPlanning verifies user-facing
// tools (bash / write / etc.) are NOT flagged as internal, so they
// keep flowing through extractToolName / mapNativeToolArgsJSON.
func TestInternalPlanningToolAsText_NonPlanning(t *testing.T) {
	for _, tc := range []*cursorpb.AgentV1_ToolCall{
		{Tool: &cursorpb.AgentV1_ToolCall_PiBashToolCall{PiBashToolCall: &cursorpb.AgentV1_PiBashToolCall{}}},
		{Tool: &cursorpb.AgentV1_ToolCall_ShellToolCall{ShellToolCall: &cursorpb.AgentV1_ShellToolCall{}}},
		{Tool: &cursorpb.AgentV1_ToolCall_EditToolCall{EditToolCall: &cursorpb.AgentV1_EditToolCall{}}},
	} {
		_, isInternal := internalPlanningToolAsText(tc)
		if isInternal {
			t.Errorf("user-facing tool %T incorrectly flagged as internal planning", tc.GetTool())
		}
	}
}

// TestInternalPlanningToolAsText_EmptyArgs — an empty CreatePlan
// still counts as internal (must be swallowed) but yields an empty
// text so the writer can drop the event entirely without emitting
// an empty content_block_start.
func TestInternalPlanningToolAsText_EmptyArgs(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_CreatePlanToolCall{
			CreatePlanToolCall: &cursorpb.AgentV1_CreatePlanToolCall{},
		},
	}
	text, isInternal := internalPlanningToolAsText(tc)
	if !isInternal {
		t.Error("empty CreatePlan should still be flagged as internal")
	}
	if text != "" {
		t.Errorf("empty args should yield empty text (caller drops event), got %q", text)
	}
}

// stringPtr is the pointer-taking helper protobuf oneof fields want
// for optional strings. Inlined here so the test file doesn't need
// its own utility import.
func stringPtr(s string) *string {
	return &s
}
