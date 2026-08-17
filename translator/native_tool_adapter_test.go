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
// (SemSearch, ListMcpResources, and 40+ others), extractToolName
// MUST return the oneof branch name — not "" — so the client at
// least logs something. Second downstream ask from the 2026-07-18
// report.
func TestExtractToolName_UnknownReturnsBranchName(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_SemSearchToolCall{
			SemSearchToolCall: &cursorpb.AgentV1_SemSearchToolCall{},
		},
	}
	got := extractToolName(tc)
	if got == "" {
		t.Fatalf("extractToolName(SemSearch) = %q, want non-empty branch name — client rejects empty tool names outright", got)
	}
	if !strings.Contains(strings.ToLower(got), "semsearch") {
		t.Errorf("extractToolName(SemSearch) = %q, want to contain 'semsearch'", got)
	}
}

// TestExtractToolName_PlanningToolsSnakeCase — Composer's planning
// tools (create_plan / update_todos / read_todos / task) must NOT
// be rendered as assistant text (older behaviour) and must NOT be
// left to fall through to the CamelCase oneof-name fallback.
// Composer waits for a *_response tool_result before continuing, so
// clients need the exact snake_case name to dispatch. Downstream
// cursor2api's 2026-07-19 report identified the markdown intercept
// as the reason all 4 CLI harnesses stalled after the plan.
func TestExtractToolName_PlanningToolsSnakeCase(t *testing.T) {
	cases := []struct {
		name string
		tc   *cursorpb.AgentV1_ToolCall
		want string
	}{
		{"create_plan", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_CreatePlanToolCall{
				CreatePlanToolCall: &cursorpb.AgentV1_CreatePlanToolCall{},
			},
		}, "create_plan"},
		{"update_todos", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_UpdateTodosToolCall{
				UpdateTodosToolCall: &cursorpb.AgentV1_UpdateTodosToolCall{},
			},
		}, "update_todos"},
		{"read_todos", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_ReadTodosToolCall{
				ReadTodosToolCall: &cursorpb.AgentV1_ReadTodosToolCall{},
			},
		}, "read_todos"},
		{"task", &cursorpb.AgentV1_ToolCall{
			Tool: &cursorpb.AgentV1_ToolCall_TaskToolCall{
				TaskToolCall: &cursorpb.AgentV1_TaskToolCall{},
			},
		}, "task"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := extractToolName(c.tc)
			if got != c.want {
				t.Errorf("extractToolName(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
}

// TestMapNativeToolArgs_CreatePlan — args JSON must preserve the
// full plan schema (name / overview / plan / todos / phases / is_project).
// A client that declared create_plan expects this shape from
// Cursor's official client; without it there's nothing to ack with
// a create_plan_response, and Composer stays stuck.
func TestMapNativeToolArgs_CreatePlan(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_CreatePlanToolCall{
			CreatePlanToolCall: &cursorpb.AgentV1_CreatePlanToolCall{
				Args: &cursorpb.AgentV1_CreatePlanArgs{
					Name:     "Write note",
					Overview: "Create note.txt with '4271'.",
					Todos: []*cursorpb.AgentV1_TodoItem{
						{Id: "t1", Content: "check dir", Status: cursorpb.AgentV1_TodoStatus(1)},
						{Id: "t2", Content: "write file", Status: cursorpb.AgentV1_TodoStatus(2)},
					},
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	for _, want := range []string{
		`"name":"Write note"`,
		`"overview":"Create note.txt with '4271'."`,
		`"todos"`,
		`"content":"check dir"`,
		`"content":"write file"`,
		`"status":"pending"`,
		`"status":"in_progress"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mapNativeToolArgsJSON(CreatePlan) missing %q; got %q", want, got)
		}
	}
}

// TestMapNativeToolArgs_UpdateTodos — todo status must surface as
// snake_case string, and merge:true flows through when set.
func TestMapNativeToolArgs_UpdateTodos(t *testing.T) {
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_UpdateTodosToolCall{
			UpdateTodosToolCall: &cursorpb.AgentV1_UpdateTodosToolCall{
				Args: &cursorpb.AgentV1_UpdateTodosArgs{
					Todos: []*cursorpb.AgentV1_TodoItem{
						{Id: "t1", Content: "step 1", Status: cursorpb.AgentV1_TodoStatus(3)},
					},
					Merge: true,
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	for _, want := range []string{
		`"todos"`,
		`"content":"step 1"`,
		`"status":"completed"`,
		`"merge":true`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("mapNativeToolArgsJSON(UpdateTodos) missing %q; got %q", want, got)
		}
	}
}

// TestMapNativeToolArgs_Task — description + prompt (the two
// required TaskArgs fields) must survive the JSON round-trip.
func TestMapNativeToolArgs_Task(t *testing.T) {
	desc, prompt := "run a subagent", "explore repo"
	tc := &cursorpb.AgentV1_ToolCall{
		Tool: &cursorpb.AgentV1_ToolCall_TaskToolCall{
			TaskToolCall: &cursorpb.AgentV1_TaskToolCall{
				Args: &cursorpb.AgentV1_TaskArgs{
					Description: desc,
					Prompt:      prompt,
				},
			},
		},
	}
	got := mapNativeToolArgsJSON(tc)
	if !strings.Contains(got, `"description":"run a subagent"`) {
		t.Errorf("mapNativeToolArgsJSON(Task) missing description; got %q", got)
	}
	if !strings.Contains(got, `"prompt":"explore repo"`) {
		t.Errorf("mapNativeToolArgsJSON(Task) missing prompt; got %q", got)
	}
}

// stringPtr is the pointer-taking helper protobuf oneof fields want
// for optional strings. Inlined here so the test file doesn't need
// its own utility import.
func stringPtr(s string) *string {
	return &s
}

// TestApplyClientToolAlias — client codex declares `shell`, we
// default `bash`, alias resolution should pick `shell` because
// they're in the same family (Cursor's downstream 2026-07-19
// asked for exactly this).
func TestApplyClientToolAlias_CodexShell(t *testing.T) {
	ev := &Event{ToolName: "bash"}
	ApplyClientToolAlias(ev, []string{"shell", "read", "write"})
	if ev.ToolName != "shell" {
		t.Errorf("ApplyClientToolAlias: bash + client{shell} = %q, want shell", ev.ToolName)
	}
}

func TestApplyClientToolContract_CodexExactDialect(t *testing.T) {
	ev := &Event{ToolName: "bash"}
	ApplyClientToolContract(ev, []string{"exec_command", "apply_patch"}, ToolNameDialectDeclaredOnly)
	if ev.ToolName != "exec_command" {
		t.Fatalf("Codex shell tool = %q, want request-declared exec_command", ev.ToolName)
	}
}

func TestApplyClientToolContract_ClaudeCodeFallback(t *testing.T) {
	ev := &Event{ToolName: "glob"}
	ApplyClientToolContract(ev, nil, ToolNameDialectClaudeCode)
	if ev.ToolName != "Glob" {
		t.Fatalf("Claude Code lazy glob = %q, want canonical Glob", ev.ToolName)
	}
}

func TestApplyClientToolContract_PreservesMCPExactCase(t *testing.T) {
	ev := &Event{ToolName: "MyServer.CustomTool"}
	ApplyClientToolContract(ev, []string{"MyServer.CustomTool"}, ToolNameDialectClaudeCode)
	if ev.ToolName != "MyServer.CustomTool" {
		t.Fatalf("MCP exact tool name changed to %q", ev.ToolName)
	}
}

func TestApplyClientToolAlias_AmbiguousCasePreservesExactIdentity(t *testing.T) {
	ev := &Event{ToolName: "glob"}
	ApplyClientToolAlias(ev, []string{"Glob", "glob"})
	if ev.ToolName != "glob" {
		t.Fatalf("exact lowercase identity changed to %q", ev.ToolName)
	}
}

func TestApplyClientToolAlias_AmbiguousAliasesAreNotGuessed(t *testing.T) {
	ev := &Event{ToolName: "bash"}
	ApplyClientToolAlias(ev, []string{"shell", "exec_command"})
	if ev.ToolName != "bash" {
		t.Fatalf("ambiguous shell aliases guessed %q, want semantic bash unchanged", ev.ToolName)
	}
}

// TestApplyClientToolAlias_ExactMatch — Grok emits `add` (MCP-side
// exact match). Client declared `add`. Should stay as declared.
func TestApplyClientToolAlias_ExactMatch(t *testing.T) {
	ev := &Event{ToolName: "add"}
	ApplyClientToolAlias(ev, []string{"add", "subtract"})
	if ev.ToolName != "add" {
		t.Errorf("exact-match alias returned %q, want add", ev.ToolName)
	}
}

// TestApplyClientToolAlias_CaseInsensitiveMatch — Composer emits
// `bash`, client declared `Bash` (Claude Code capitalisation). We
// should return the client's exact spelling.
func TestApplyClientToolAlias_CaseInsensitiveMatch(t *testing.T) {
	ev := &Event{ToolName: "bash"}
	ApplyClientToolAlias(ev, []string{"Bash", "Read", "Write"})
	if ev.ToolName != "Bash" {
		t.Errorf("case-insensitive match returned %q, want Bash", ev.ToolName)
	}
}

// TestApplyClientToolAlias_NoMatch — client declared entirely
// unrelated tools. Leave the ToolName alone so at least our
// lowercase default is visible.
func TestApplyClientToolAlias_NoMatch(t *testing.T) {
	ev := &Event{ToolName: "bash"}
	ApplyClientToolAlias(ev, []string{"query_database", "send_email"})
	if ev.ToolName != "bash" {
		t.Errorf("no-match alias mutated ToolName to %q; want to keep 'bash' default", ev.ToolName)
	}
}

// TestApplyClientToolAlias_NoClientTools — no tools declared,
// leave name alone.
func TestApplyClientToolAlias_NoClientTools(t *testing.T) {
	ev := &Event{ToolName: "bash"}
	ApplyClientToolAlias(ev, nil)
	if ev.ToolName != "bash" {
		t.Errorf("nil client tools mutated ToolName to %q", ev.ToolName)
	}
	ApplyClientToolAlias(ev, []string{})
	if ev.ToolName != "bash" {
		t.Errorf("empty client tools mutated ToolName to %q", ev.ToolName)
	}
}

// TestApplyClientToolAlias_NilEventSafe — defensive
func TestApplyClientToolAlias_NilEventSafe(t *testing.T) {
	ApplyClientToolAlias(nil, []string{"shell"})
	// no panic = pass
}
