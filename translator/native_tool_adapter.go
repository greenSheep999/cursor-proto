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
var nativeToolClientName = map[string]string{
	"shell":  "Bash",
	"read":   "Read",
	"edit":   "Write",
	"grep":   "Grep",
	"ls":     "LS",
	"glob":   "Glob",
	"delete": "Bash", // no dedicated Delete in Claude Code; map to Bash rm
	"fetch":  "WebFetch",
	// ask_question is a Cursor-internal user-facing prompt; it has no
	// direct Claude Code analogue. Leave the raw name so clients that
	// know Cursor natively (cursor2api harness) can handle it.
	"ask_question": "AskQuestion",
}

// clientNameForCursorTool returns the client-visible tool name for
// a Cursor internal exec type. Falls back to the raw name if the
// type isn't in the mapping — better a technically-wrong name than
// an empty string, which the client would reject as Invalid tool ''.
func clientNameForCursorTool(cursorType string) string {
	if mapped, ok := nativeToolClientName[cursorType]; ok {
		return mapped
	}
	return cursorType
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
// `'\''` sequence.
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
