// native_tool_adapter.go — translates Cursor's native tool calls
// (shell / read / edit / grep / ls / glob / delete / fetch) into the
// tool names and argument shapes that OpenAI-compat and
// Anthropic-compat clients (Claude Code, Cline, aider, cursor2api's
// harness) expect.
//
// Why this exists
// ---------------
//
// Cursor's model is trained to prefer native tool APIs even when the
// caller registered its own MCP tool set. When the caller declared
// `bash` / `write_file` / `glob`, the model may still emit a
// ShellToolCall / EditToolCall / GlobToolCall on the wire. If we
// pass those internal names verbatim (`extractToolName` did until
// v0.3.2), the client rejects them:
//
//   Claude Code:  Invalid tool 'shell'
//   Cline:        Unknown tool 'edit'
//   Grok variant: Invalid tool ''
//
// The JS reference implementation
// (reference/js-src/sessionManager.js:200-322 + agentClient.js:1032-
// 1056) mediates this by mapping the exec type to a canonical name
// then delegating to a per-client adapter for the client-visible
// name and argument shape. Since we don't have adapters plumbed
// through the wire proxy yet, we ship the same Claude-Code-friendly
// mapping the JS legacy path uses — the 80 % that unblocks Claude
// Code / Cline / cursor2api immediately.
//
// Wire contract
// -------------
//
// `mapNativeToolName` returns the client-visible tool name for a
// Cursor native ToolCall variant. Callers thread the result into
// EventInfo.ToolName (translator/events.go extractToolName).
//
// `mapNativeToolArgs` takes the Cursor-side native args submessage
// and returns a JSON object matching the corresponding client
// tool's schema. Callers substitute this for the protojson.Marshal
// output previously returned from extractToolArgsFromStart.

package translator

import (
	"encoding/json"
	"strings"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// nativeToolMapping records the Cursor exec type -> client-visible
// name for every native tool the wire proxy is likely to see. Names
// match Claude Code's canonical tool set (Bash / Read / Write / Grep
// / Glob / LS / WebFetch). The JS reference uses these same names in
// the legacy fallback (sessionManager.js:283-322), so a Claude Code
// harness that already handles Cursor via that path sees a
// consistent name space here.
//
// TODO(phase-B): thread the caller's declared tool list through
// ChatRequest → TranslatorConfig so we can prefer the client's
// spelling (e.g. lowercase `bash`) over the hardcoded `Bash` when
// the client declared a compatible tool.
// Name choice: lowercase, matching what codex / opencode and most
// OpenAI/Anthropic harnesses declare in their tools[]. Claude Code
// officially uses `Bash` / `Grep` / `Read` capitalized, but the
// runtime tool-lookup on Claude Code is case-INsensitive, so
// lowercase serves both. cursor2api's 2026-07-18 report explicitly
// asked for this so codex stops erroring with `unsupported call:
// Bash` (case-sensitive match failure). See "Path C" in the report.
var nativeToolClientName = map[string]string{
	"shell":  "bash",
	"read":   "read",
	"edit":   "write",
	"grep":   "grep",
	"ls":     "ls",
	"glob":   "glob",
	"delete": "bash", // no dedicated Delete in Claude Code; map to bash rm
	"fetch":  "web_fetch",
	// Pi* family — Cursor's Composer-2.5 planning intelligence tools.
	// Composer prefers these over the shell/edit/read tools even when
	// the caller registered its own MCP set. Field shapes are already
	// close to Claude Code's — pi_write.{path,content} maps to
	// write.{file_path,content}, pi_bash.command maps to bash.command,
	// etc. Coverage of these was the specific gap downstream reported
	// on 2026-07-18 (sse-tool-use-postfix-report.md): write_file
	// calls yielded name:"" and args:{} because Composer emitted
	// PiWriteToolCall and our switch didn't enumerate it.
	"pi_write": "write",
	"pi_bash":  "bash",
	"pi_edit":  "write", // string-replace edits collapse to a write w/ new content
	"pi_read":  "read",
	"pi_find":  "glob",
	"pi_grep":  "grep",
	"pi_ls":    "ls",
	// ask_question is a Cursor-internal user-facing prompt; it has no
	// direct Claude Code analogue. Leave the raw name so clients that
	// know Cursor natively (cursor2api harness) can handle it.
	"ask_question": "ask_question",
	// Composer's planning-intelligence tools. Cursor's official client
	// declares these in tools[] and ACKs them with a *_response tool
	// result; downstream cursor2api (opencode / codex / claude-code)
	// mirrors that schema. Prior versions rendered CreatePlan as
	// assistant markdown to hide it from harnesses that didn't declare
	// the tool — but Composer waits for create_plan_response before
	// emitting pi_write, so a text render meant the follow-up tool
	// never fired (cursor2api 2026-07-19 report). Passing through as
	// snake_case tool_call is the correct wire shape: clients that
	// declared create_plan handle it normally; clients that didn't
	// can still ack via a synthetic tool_result and unblock Composer.
	"create_plan":  "create_plan",
	"update_todos": "update_todos",
	"read_todos":   "read_todos",
	"task":         "task",
}

// clientNameForCursorTool returns the client-visible tool name for
// a Cursor internal exec type. Falls back to the raw name if the
// type isn't in the mapping — better a technically-wrong name than
// an empty string, which the client would reject as Invalid tool ”.
func clientNameForCursorTool(cursorType string) string {
	if mapped, ok := nativeToolClientName[cursorType]; ok {
		return mapped
	}
	return cursorType
}

// toolNameAliases groups common client-declared names that map to
// the same functional tool. Used by ApplyClientToolAlias to pick
// the client's spelling instead of our hardcoded default.
//
// Grouped by concept:
//   - shell: bash / shell / run_terminal_command / run_shell_command /
//     run_command / execute
//   - write: write / write_file / create_file / create
//   - read:  read / read_file / open_file / view
//   - grep:  grep / ripgrep_search / grep_search / search_files
//   - glob:  glob / file_search / find_files
//   - ls:    ls / list_dir / list_directory
//   - fetch: web_fetch / webfetch / fetch_url / http_get
//
// A single Cursor default like "bash" from Path C above matches any
// alias in the "shell" group. When the client declared any of those
// spellings we return the exact one they declared; otherwise we
// keep our default. See cursor2api's 2026-07-19 codex report — codex
// declares `shell`, we returned `bash`, hence 6× "unsupported call".
var toolNameAliases = [][]string{
	// shell family
	{"bash", "shell", "exec_command", "shell_command", "run_terminal_command", "run_terminal_cmd", "run_shell_command", "run_command", "execute", "sh"},
	// write family
	{"write", "write_file", "create_file", "create", "str_replace_editor"},
	// read family
	{"read", "read_file", "open_file", "view"},
	// grep family
	{"grep", "ripgrep_search", "grep_search", "search_files"},
	// glob family
	{"glob", "file_search", "find_files"},
	// ls family
	{"ls", "list_dir", "list_directory", "list_files"},
	// fetch family
	{"web_fetch", "webfetch", "fetch_url", "http_get", "web_search"},
}

// ToolNameDialect selects the fallback spelling used when a translated
// native tool cannot be matched to a name declared in the current request.
// Declared names always win; dialects exist only for lazy/deferred built-ins.
type ToolNameDialect uint8

const (
	ToolNameDialectDeclaredOnly ToolNameDialect = iota
	ToolNameDialectClaudeCode
)

var claudeCodeFallbackToolNames = map[string]string{
	"bash":      "Bash",
	"read":      "Read",
	"write":     "Write",
	"grep":      "Grep",
	"glob":      "Glob",
	"ls":        "LS",
	"web_fetch": "WebFetch",
}

// ApplyClientToolContract resolves a tool event through one deep interface:
// first use the caller's dynamic tools[] contract, then apply a CLI-specific
// fallback only when no declared tool matched. Protocol writers do not need to
// know Cursor's native names or maintain their own alias tables.
func ApplyClientToolContract(ev *Event, clientToolNames []string, dialect ToolNameDialect) {
	if ev == nil || ev.ToolName == "" {
		return
	}
	ApplyClientToolAlias(ev, clientToolNames)
	for _, name := range clientToolNames {
		if strings.EqualFold(strings.TrimSpace(name), ev.ToolName) {
			return
		}
	}
	if dialect != ToolNameDialectClaudeCode {
		return
	}
	if canonical, ok := claudeCodeFallbackToolNames[strings.ToLower(ev.ToolName)]; ok {
		ev.ToolName = canonical
	}
}

// ApplyClientToolAlias rewrites ev.ToolName to whatever the client
// declared in its tools[] list, when we can identify a compatible
// alias. When there's no match, leaves the name untouched — a
// client that doesn't declare shell/bash still sees `bash` (our
// Path C default), which is at least a valid identifier.
//
// clientToolNames comes from OpenAI req.Tools[].function.name or
// Anthropic req.Tools[].name; case is preserved as-declared so the
// returned name matches exactly what the client's dispatch table
// expects.
//
// A nil / empty clientToolNames list is a no-op — clients that
// don't declare tools shouldn't get aliased.
func ApplyClientToolAlias(ev *Event, clientToolNames []string) {
	if ev == nil || ev.ToolName == "" || len(clientToolNames) == 0 {
		return
	}
	// Exact, case-sensitive identity is authoritative, including when a
	// catalog intentionally declares both `Glob` and `glob`.
	for _, name := range clientToolNames {
		if strings.TrimSpace(name) == ev.ToolName {
			return
		}
	}

	// Build a lowercase index, but retain only unique case-folded names.
	// Ambiguous casing must not be resolved by map insertion order.
	byLower := make(map[string]string, len(clientToolNames))
	ambiguousLower := make(map[string]bool)
	for _, name := range clientToolNames {
		n := strings.TrimSpace(name)
		if n == "" {
			continue
		}
		lower := strings.ToLower(n)
		if existing, ok := byLower[lower]; ok && existing != n {
			ambiguousLower[lower] = true
			continue
		}
		byLower[lower] = n
	}
	// If the current ToolName already matches (case-insensitively)
	// something the client declared, return the client's exact
	// spelling — this covers the Grok case where the model calls
	// an MCP tool by the caller's own name.
	currentLower := strings.ToLower(ev.ToolName)
	if exact, ok := byLower[currentLower]; ok && !ambiguousLower[currentLower] {
		ev.ToolName = exact
		return
	}
	// Otherwise find the alias group our default lives in, then
	// check whether the client declared any other alias in the
	// same group. If yes, prefer the client's spelling.
	group := aliasGroupOf(ev.ToolName)
	if group == nil {
		return
	}
	var candidate string
	for _, alias := range group {
		lower := strings.ToLower(alias)
		if ambiguousLower[lower] {
			return
		}
		if exact, ok := byLower[lower]; ok {
			if candidate != "" && candidate != exact {
				return
			}
			candidate = exact
		}
	}
	if candidate != "" {
		ev.ToolName = candidate
	}
	// No client alias matched. Leave the current name.
}

// aliasGroupOf returns the alias list that contains name (case-
// insensitive lookup), or nil if name isn't in any group.
func aliasGroupOf(name string) []string {
	lower := strings.ToLower(name)
	for _, group := range toolNameAliases {
		for _, a := range group {
			if a == lower {
				return group
			}
		}
	}
	return nil
}

// mapNativeToolArgsJSON returns the client-schema JSON for a Cursor
// native tool call. Fields are renamed and pruned to match what
// Claude Code / Cline harnesses expect. Anything not on the client
// side is dropped — it's Cursor-internal detail (parsing_result,
// simple_commands, timeout_behavior, sandbox_policy, etc.) that
// would only confuse a harness.
//
// Returns "" if the tool call carries no native args (caller should
// fall back to extractToolArgsFromStart's MCP path or "{}").
func mapNativeToolArgsJSON(tc *cursorpb.AgentV1_ToolCall) string {
	if tc == nil {
		return ""
	}
	switch {
	case tc.GetShellToolCall() != nil:
		a := tc.GetShellToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// Claude Code Bash: { command, description? }.
		// working_directory becomes description because Cursor uses cwd
		// as a per-invocation label and Bash has no cwd field.
		out := map[string]any{"command": a.GetCommand()}
		if cwd := a.GetWorkingDirectory(); cwd != "" {
			out["description"] = "Run in " + cwd
		}
		return marshalJSON(out)
	case tc.GetReadToolCall() != nil:
		a := tc.GetReadToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// Claude Code Read: { file_path, offset?, limit? }.
		out := map[string]any{"file_path": a.GetPath()}
		if a.GetOffset() != 0 {
			out["offset"] = a.GetOffset()
		}
		if a.GetLimit() != 0 {
			out["limit"] = a.GetLimit()
		}
		return marshalJSON(out)
	case tc.GetEditToolCall() != nil:
		a := tc.GetEditToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// Claude Code Write: { file_path, content }.
		// Cursor EditArgs.stream_content carries the full replacement
		// body for the streaming edit path; treat as `content`.
		return marshalJSON(map[string]any{
			"file_path": a.GetPath(),
			"content":   a.GetStreamContent(),
		})
	case tc.GetGrepToolCall() != nil:
		a := tc.GetGrepToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// Claude Code Grep: { pattern, path?, glob?, output_mode? }.
		out := map[string]any{"pattern": a.GetPattern()}
		if path := a.GetPath(); path != "" {
			out["path"] = path
		}
		if glob := a.GetGlob(); glob != "" {
			out["glob"] = glob
		}
		if mode := a.GetOutputMode(); mode != "" {
			out["output_mode"] = mode
		}
		return marshalJSON(out)
	case tc.GetLsToolCall() != nil:
		a := tc.GetLsToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// Claude Code LS: { path, ignore? }.
		out := map[string]any{"path": a.GetPath()}
		if ig := a.GetIgnore(); len(ig) > 0 {
			out["ignore"] = ig
		}
		return marshalJSON(out)
	case tc.GetGlobToolCall() != nil:
		a := tc.GetGlobToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// Claude Code Glob: { pattern, path? }. Cursor's field is
		// glob_pattern; the client expects `pattern` — this rename is
		// the whole point of the mapping (see the "delta merger
		// dropped pattern" downstream bug report).
		out := map[string]any{"pattern": a.GetGlobPattern()}
		if td := a.GetTargetDirectory(); td != "" {
			out["path"] = td
		}
		return marshalJSON(out)
	case tc.GetDeleteToolCall() != nil:
		a := tc.GetDeleteToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// Mapped to Bash — emit `rm -rf <path>` so the client's Bash
		// executor performs the delete. Not perfect (no rollback,
		// destructive) but matches the JS legacy path.
		path := a.GetPath()
		return marshalJSON(map[string]any{
			"command":     "rm -rf " + shellQuote(path),
			"description": "Delete " + path,
		})
	case tc.GetFetchToolCall() != nil:
		a := tc.GetFetchToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// Claude Code WebFetch: { url, prompt? }. Cursor's FetchArgs
		// carries no prompt.
		return marshalJSON(map[string]any{"url": a.GetUrl()})
	case tc.GetAskQuestionToolCall() != nil:
		// AskQuestion has no direct Claude Code analogue. Passthrough
		// the raw args JSON — clients that don't know AskQuestion will
		// error, but that's a per-client policy call, not a data
		// shape one.
		return ""
	// Pi* family — Composer / planning-intelligence tools. Field
	// shapes are already close to Claude Code's; do a minimal rename
	// (path → file_path) so a client's write_file / read_file /
	// glob / grep receives the arguments in the shape it declared.
	case tc.GetPiWriteToolCall() != nil:
		a := tc.GetPiWriteToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		return marshalJSON(map[string]any{
			"file_path": a.GetPath(),
			"content":   a.GetContent(),
		})
	case tc.GetPiBashToolCall() != nil:
		a := tc.GetPiBashToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		out := map[string]any{"command": a.GetCommand()}
		if t := a.GetTimeout(); t > 0 {
			out["timeout_ms"] = int(t * 1000)
		}
		return marshalJSON(out)
	case tc.GetPiEditToolCall() != nil:
		a := tc.GetPiEditToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// PiEdit is a stream of {old_text, new_text} replacements.
		// Claude Code's Write expects one final content blob — we can't
		// materialise that here without the source file. Expose the
		// replacements list under `edits` so clients that DO understand
		// the string-replace shape (Cline, cursor2api native) can use
		// it; clients that only know Write will reject the tool_use.
		// Also expose file_path so at least the target file is known.
		edits := a.GetEdits()
		out := map[string]any{"file_path": a.GetPath()}
		if len(edits) > 0 {
			list := make([]map[string]string, 0, len(edits))
			for _, e := range edits {
				list = append(list, map[string]string{
					"old_string": e.GetOldText(),
					"new_string": e.GetNewText(),
				})
			}
			out["edits"] = list
		}
		return marshalJSON(out)
	case tc.GetPiReadToolCall() != nil:
		a := tc.GetPiReadToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		out := map[string]any{"file_path": a.GetPath()}
		if a.GetOffset() != 0 {
			out["offset"] = a.GetOffset()
		}
		if a.GetLimit() != 0 {
			out["limit"] = a.GetLimit()
		}
		return marshalJSON(out)
	case tc.GetPiFindToolCall() != nil:
		a := tc.GetPiFindToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		// PiFind is fuzzy filename discovery — Claude Code's Glob is
		// the closest analogue.
		out := map[string]any{"pattern": a.GetPattern()}
		if p := a.GetPath(); p != "" {
			out["path"] = p
		}
		return marshalJSON(out)
	case tc.GetPiGrepToolCall() != nil:
		a := tc.GetPiGrepToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		out := map[string]any{"pattern": a.GetPattern()}
		if p := a.GetPath(); p != "" {
			out["path"] = p
		}
		if g := a.GetGlob(); g != "" {
			out["glob"] = g
		}
		return marshalJSON(out)
	case tc.GetPiLsToolCall() != nil:
		a := tc.GetPiLsToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		out := map[string]any{}
		if p := a.GetPath(); p != "" {
			out["path"] = p
		}
		return marshalJSON(out)
	// Composer planning tools — pass args through as a JSON object.
	// Field names mirror the Cursor server schema (name / overview /
	// plan / todos / phases for create_plan; todos / merge for
	// update_todos; status_filter / id_filter for read_todos; the
	// full TaskArgs shape for task). Any client that declared these
	// tools already knows the schema; clients that didn't can either
	// swallow the tool_use or reply with a stubbed result to unblock
	// Composer's wait state.
	case tc.GetCreatePlanToolCall() != nil:
		a := tc.GetCreatePlanToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		out := map[string]any{}
		if n := a.GetName(); n != "" {
			out["name"] = n
		}
		if o := a.GetOverview(); o != "" {
			out["overview"] = o
		}
		if p := a.GetPlan(); p != "" {
			out["plan"] = p
		}
		if a.GetIsProject() {
			out["is_project"] = true
		}
		if td := a.GetTodos(); len(td) > 0 {
			out["todos"] = todosToJSON(td)
		}
		if ph := a.GetPhases(); len(ph) > 0 {
			phases := make([]map[string]any, 0, len(ph))
			for _, p := range ph {
				pj := map[string]any{}
				if n := p.GetName(); n != "" {
					pj["name"] = n
				}
				if td := p.GetTodos(); len(td) > 0 {
					pj["todos"] = todosToJSON(td)
				}
				phases = append(phases, pj)
			}
			out["phases"] = phases
		}
		return marshalJSON(out)
	case tc.GetUpdateTodosToolCall() != nil:
		a := tc.GetUpdateTodosToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		out := map[string]any{}
		if td := a.GetTodos(); len(td) > 0 {
			out["todos"] = todosToJSON(td)
		}
		if a.GetMerge() {
			out["merge"] = true
		}
		return marshalJSON(out)
	case tc.GetReadTodosToolCall() != nil:
		a := tc.GetReadTodosToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		out := map[string]any{}
		if sf := a.GetStatusFilter(); len(sf) > 0 {
			ss := make([]string, 0, len(sf))
			for _, s := range sf {
				ss = append(ss, todoStatusString(int32(s)))
			}
			out["status_filter"] = ss
		}
		if id := a.GetIdFilter(); len(id) > 0 {
			out["id_filter"] = id
		}
		return marshalJSON(out)
	case tc.GetTaskToolCall() != nil:
		a := tc.GetTaskToolCall().GetArgs()
		if a == nil {
			return "{}"
		}
		out := map[string]any{}
		if d := a.GetDescription(); d != "" {
			out["description"] = d
		}
		if p := a.GetPrompt(); p != "" {
			out["prompt"] = p
		}
		if m := a.GetModel(); m != "" {
			out["model"] = m
		}
		if r := a.GetResume(); r != "" {
			out["resume"] = r
		}
		if id := a.GetAgentId(); id != "" {
			out["agent_id"] = id
		}
		if at := a.GetAttachments(); len(at) > 0 {
			out["attachments"] = at
		}
		if ids := a.GetRespondingToMessageIds(); len(ids) > 0 {
			out["responding_to_message_ids"] = ids
		}
		return marshalJSON(out)
	}
	return ""
}

// todosToJSON serialises a []*TodoItem into the JSON shape Cursor's
// server side uses: [{id, content, status, dependencies?}, ...].
// Status is emitted as snake_case string so clients don't need to
// know the enum's integer coding.
func todosToJSON(todos []*cursorpb.AgentV1_TodoItem) []map[string]any {
	out := make([]map[string]any, 0, len(todos))
	for _, t := range todos {
		row := map[string]any{}
		if id := t.GetId(); id != "" {
			row["id"] = id
		}
		if c := t.GetContent(); c != "" {
			row["content"] = c
		}
		row["status"] = todoStatusString(int32(t.GetStatus()))
		if d := t.GetDependencies(); len(d) > 0 {
			row["dependencies"] = d
		}
		out = append(out, row)
	}
	return out
}

// todoStatusString maps Cursor's TodoStatus enum to its wire name.
// The numeric order is stable across proto revisions:
//
//	0 UNSPECIFIED, 1 PENDING, 2 IN_PROGRESS, 3 COMPLETED, 4 CANCELLED
func todoStatusString(s int32) string {
	switch s {
	case 1:
		return "pending"
	case 2:
		return "in_progress"
	case 3:
		return "completed"
	case 4:
		return "cancelled"
	default:
		return "unspecified"
	}
}

// marshalJSON is a defensive helper — every value in a nativeTool
// args map is a string / number / bool / []string. json.Marshal
// cannot fail on those, but we handle the error path anyway so a
// future change (adding a nested struct) doesn't crash the whole
// event stream.
func marshalJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// shellQuote escapes a path for safe insertion into a shell command
// string. Only single-quotes need escaping since we wrap the value
// in single quotes; embedded single-quotes become the standard
// `'\”` sequence.
func shellQuote(s string) string {
	out := "'"
	for _, r := range s {
		if r == '\'' {
			out += `'\''`
			continue
		}
		out += string(r)
	}
	out += "'"
	return out
}
