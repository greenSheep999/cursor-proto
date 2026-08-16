# Phase 7f — CLIProxyAPI (CPA) Integration

This phase adds the pieces required to plug the cursor-proto client
into `router-for-me/CLIProxyAPI` (CPA), the operator's multi-provider
proxy. Two deliverables are covered:

- **D1**: `cursor-to-cpa`, a CLI tool that converts the account JSON
  written by `cmd/cursor-login` into the JSON shape CPA expects under
  its `auths/` directory.
- **D2**: `plugin/cursor`, a Go plugin compiled as a C-shared library
  that exposes cursor-proto through CPA's plugin ABI.

## CPA JSON shape

CPA's file synthesizer (`internal/watcher/synthesizer/file.go`) reads
each JSON file under the configured auth directory as a **flat**
object, keyed off the top-level `type` field. Other providers such as
Claude, Codex, Kimi, XAI and Gemini all use the same layout: token
material plus `type`, `email`, `proxy_url`, `prefix`, `priority`,
`note`, `disabled`, `disable_cooling`, `request_retry`,
`excluded_models` at the top level.

Cursor accounts may also persist an optional, internally consistent IDE
platform profile:

```json
{
  "client_os": "win32",
  "client_os_version": "10.0.22631",
  "client_arch": "x64",
  "client_shell": "C:\\Windows\\System32\\WindowsPowerShell\\v1.0\\powershell.exe",
  "workspace_path": "C:\\Users\\Public\\Cursor"
}
```

Supported `client_os` values are `darwin`, `win32`, and `linux`. The fields
round-trip through `cursor-to-cpa` and CPA auth refreshes. When omitted, the
executor uses a coherent profile for its local runtime rather than mixing
headers and Agent environment fields from different operating systems.

We reuse that layout, giving Cursor its own provider key (`"type":
"cursor"`) and adding a small provider-specific block for the fields
the runtime executor needs:

```json
{
  "type": "cursor",
  "access_token": "…",
  "refresh_token": "…",
  "email": "you@example.com",
  "user_id": "user_01KX…",
  "auth_id": "auth0|user_01KX…",
  "auth_kind": "Auth_0",
  "machine_id": "abc…",
  "mac_machine_id": "aab…",
  "issued_at": "2025-06-01T00:00:00Z",
  "last_refresh": "2025-06-01T00:00:00Z",
  "expired": "2026-06-01T00:00:00Z",
  "prefix": "team-a",
  "priority": 5,
  "note": "primary cursor account"
}
```

Notes:
- `expired` (not `expires_at`) matches CPA's convention — `Auth.ExpirationTime`
  understands it directly.
- `issued_at` and `last_refresh` are RFC3339 UTC strings.
- The provider block mirrors `auth.Account` in this repo minus the
  session-scoped fields (`session_id`, `config_version`, `client_key`,
  `checksum_session`), which the plugin regenerates at load time via
  `Account.FillSessionDefaults`.

The shared struct definitions live in `sdk/cpaformat/`, imported by
both the converter CLI and the CPA plugin so there is one source of
truth for the layout.

## Running the converter

```
CGO_ENABLED=1 go build -o cursor-to-cpa ./cmd/cursor-to-cpa

# Simplest form: derive the filename from the account email and drop
# it in $HOME/.cli-proxy-api/ (CPA's default auth directory).
./cursor-to-cpa -in ./account.json

# Or write to an explicit path.
./cursor-to-cpa -in ./account.json -out /tmp/cpa.json

# Or target a specific CPA install.
./cursor-to-cpa -in ./account.json -dir /etc/cliproxyapi/auths

# Optional operator knobs (all mirror the top-level JSON fields):
./cursor-to-cpa -in ./account.json \
  -prefix team-a \
  -proxy-url http://proxy.local:8080 \
  -priority 5 \
  -note "primary cursor account" \
  -disable-cooling \
  -request-retry 3 \
  -excluded-models composer-1,cursor-small
```

A dry-run mode (`-dry-run`) prints the resulting JSON to stdout without
touching disk. `-stdout` writes to disk and also echoes the result for
piping.

## Loading the plugin in CPA

Build the plugin as a c-shared library:

```
CGO_ENABLED=1 go build -buildmode=c-shared \
    -o plugin/cursor/cursor.dylib ./plugin/cursor      # macOS
CGO_ENABLED=1 go build -buildmode=c-shared \
    -o plugin/cursor/cursor.so ./plugin/cursor         # linux
```

Copy the resulting shared object into CPA's plugin directory and enable
it via `config.yaml`. A drop-in snippet lives in
`plugin/cursor/plugin.example.yaml`:

```yaml
plugins:
  enabled: true
  dir: "plugins"
  configs:
    cursor:
      enabled: true
      priority: 5
```

On startup CPA calls `cliproxy_plugin_init` from the shared object,
followed by `plugin.register` to learn the capabilities. The plugin
declares:

- `auth_provider: true` — CPA hands every unrecognised JSON file with
  `type: cursor` to the plugin's `auth.parse`.
- `executor: true`, `executor_model_scope: oauth`,
  `executor_input_formats: ["openai", "claude"]`,
  `executor_output_formats: ["openai", "claude"]` — advertises the
  cursor executor, though `execute` / `execute_stream` are still
  stubbed (see below).
- `model_provider: true` — `model.static` returns the hand-maintained
  list of Cursor models.

## What ships in this pass

- `sdk/cpaformat/`: shared struct + convert/marshal/validate helpers,
  fully tested.
- `cmd/cursor-to-cpa`: CLI converter, exercised end-to-end against a
  local fixture.
- `plugin/cursor/`: C-shared library that answers `plugin.register`,
  `plugin.reconfigure`, `plugin.shutdown`, `auth.identifier`,
  `auth.parse`, `auth.refresh` (passthrough), `executor.identifier`,
  and `model.static` / `model.for_auth`.
- `plugin/cursor/plugin.example.yaml`: CPA config snippet.

## What is deferred

`executor.execute_stream`, `executor.execute`, and
`executor.count_tokens` currently return an `not_implemented` envelope.
The exact data path we need to close — protocol translation, Cursor
RunSSE invocation, host stream chunk emission — is captured in
`docs/phase-7f-plugin-plan.md`. That doc also describes the follow-up
work needed to make `auth.refresh` call Cursor's refresh endpoint
rather than acting as a passthrough.

Even in the current state, the plugin builds cleanly, exports all four
ABI symbols, and can be loaded by CPA to advertise the Cursor provider
alongside converted account JSON files. Users who need chat immediately
can keep running `cursor-proxy` in front of CPA.
