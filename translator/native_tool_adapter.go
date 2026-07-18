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

// internalPlanningToolAsText detects Cursor's internal orchestration
// tools (createPlan, updateTodos, task, etc.) and renders them as
// a plain-text summary so downstream writers emit them as an
// assistant text delta instead of a tool_use block.
//
// Rationale (from cursor2api's 2026-07-18 report): Composer's
// *first* tool call on any prompt is a createPlan — before any Pi*
// execution tool. No harness (opencode / claude-code / codex)
// declares createPlan, so they all reject the call and loop until
// timeout. Since createPlan is really the model "narrating its
// plan", the correct wire-level presentation is assistant text,
// not a tool_use block.
//
// Returns (text, true) when tc is an internal planning tool and
// the caller should emit a text delta with that string (empty text
// means "drop the event entirely"). Returns ("", false) when tc is
// a normal user-facing tool that should flow through the regular
// extractToolName path.
func internalPlanningToolAsText(tc *cursorpb.AgentV1_ToolCall) (string, bool) {
	if tc == nil {
		return "", false
	}
	if cp := tc.GetCreatePlanToolCall(); cp != nil {
		a := cp.GetArgs()
		if a == nil {
			return "", true
		}
		return renderCreatePlan(a), true
	}
	if ut := tc.GetUpdateTodosToolCall(); ut != nil {
		a := ut.GetArgs()
		if a == nil {
			return "", true
		}
		return renderTodos(a.GetTodos()), true
	}
	if rt := tc.GetReadTodosToolCall(); rt != nil {
		// ReadTodos is a "let me check my todo list" no-op from the
		// user's perspective; drop entirely.
		_ = rt
		return "", true
	}
	if tk := tc.GetTaskToolCall(); tk != nil {
		// Task is Composer's subagent-spawn primitive. Its args are
		// { description, prompt } — surface as a brief text so the
		// user sees what subtask the agent is about to run.
		_ = tk
		return "", true // v1: swallow; upgrade later if downstream asks
	}
	return "", false
}

// renderCreatePlan turns a CreatePlan args payload into a
// human-readable markdown-ish summary. The client sees this as
// assistant text and the model self-continues into the actual
// execution tools on the next turn.
func renderCreatePlan(a *cursorpb.AgentV1_CreatePlanArgs) string {
	var b []byte
	if name := a.GetName(); name != "" {
		b = append(b, "**Plan: "...)
		b = append(b, name...)
		b = append(b, "**\n\n"...)
	}
	if overview := a.GetOverview(); overview != "" {
		b = append(b, overview...)
		b = append(b, "\n\n"...)
	}
	if plan := a.GetPlan(); plan != "" {
		b = append(b, plan...)
		b = append(b, "\n\n"...)
	}
	if todos := a.GetTodos(); len(todos) > 0 {
		b = append(b, renderTodos(todos)...)
	}
	if phases := a.GetPhases(); len(phases) > 0 {
		for _, p := range phases {
			b = append(b, "### "...)
			b = append(b, p.GetName()...)
			b = append(b, "\n"...)
			b = append(b, renderTodos(p.GetTodos())...)
			b = append(b, "\n"...)
		}
	}
	return string(b)
}

// renderTodos writes each todo as a checkbox line. Status:
//
//	0 (unspecified) / 1 (pending)      -> [ ]
//	2 (in_progress)                    -> [~]
//	3 (completed)                      -> [x]
//
// We don't rely on the exact enum spelling because the proto's
// TodoStatus enum may reorder — the numeric ordering ("higher =
// further along") is stable.
func renderTodos(todos []*cursorpb.AgentV1_TodoItem) string {
	if len(todos) == 0 {
		return ""
	}
	var b []byte
	for _, t := range todos {
		var mark string
		switch int32(t.GetStatus()) {
		case 3:
			mark = "[x] "
		case 2:
			mark = "[~] "
		default:
			mark = "[ ] "
		}
		b = append(b, "- "...)
		b = append(b, mark...)
		b = append(b, t.GetContent()...)
		b = append(b, '\n')
	}
	return string(b)
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
	}
	return ""
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
