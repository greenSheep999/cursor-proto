# Observability endpoints

**Introduced**: `cursor3.11/v0.2.6` and `cursor3.10/v0.1.5`.

`cursor-proxy` exposes three read-only endpoints for downstream
consumers (cursor2api's UI, ops dashboards, health probes) to see
what the proxy supports and what recently flowed through it. All
three bypass `-api-keys` authentication — there are no secrets in
the responses, and cursor2api's sidecar supervisor needs to probe
them before it has an API key wired.

## `GET /v1/capabilities`

Static description of what protocol features this build supports.
The values here are compile-time facts — they change when the
codebase changes, never per-request. Fetch once at sidecar spawn.

```json
{
  "streaming": true,
  "tool_use_json_input": true,
  "multi_turn_tool_loop": true,
  "thinking": true,
  "prompt_caching": {
    "read_tokens_reported": true,
    "write_tokens_reported": true,
    "local_simulator": true,
    "cache_control_honored": false
  },
  "server_tools": false,
  "mcp_tools": true,
  "effort_mapping": true,
  "anthropic_model_aliases": true,
  "http_version_options": ["auto", "http1.1", "http1.0"]
}
```

Field meanings:

- **streaming** — `/v1/messages` and `/v1/chat/completions` support SSE.
- **tool_use_json_input** — `tool_use.input` is delivered as valid JSON.
  False before v0.2.3 (protobuf bytes leaked); permanently true now.
- **multi_turn_tool_loop** — assistant `tool_use` + user `tool_result`
  history threads coherently. True since v0.2.3.
- **thinking** — Extended Thinking blocks emitted for reasoning models.
- **prompt_caching.read_tokens_reported** —
  `usage.cache_read_input_tokens` populated on responses.
- **prompt_caching.write_tokens_reported** —
  `usage.cache_creation_input_tokens` populated.
- **prompt_caching.local_simulator** — the in-proxy simcache is on
  (matches `-simulate-cache` at boot). If true, cache_read counters
  blend real-upstream with local-estimate.
- **prompt_caching.cache_control_honored** — request-side
  `cache_control` markers are forwarded upstream. False today —
  Cursor's caching is server-side opaque, honoring the marker would
  be misleading.
- **server_tools** — Anthropic server-side tools (`web_search_20250305`
  etc.) accepted. Always false; proxy returns 400 to fail-fast.
- **mcp_tools** — `mcp__server__tool` names flow through unchanged.
- **effort_mapping** — `output_config.effort` and
  `thinking.budget_tokens` map onto Cursor's tier suffixes
  (`-low` / `-medium` / `-high`).
- **anthropic_model_aliases** — canonical bare names like
  `claude-sonnet-4-5-20250929` get rewritten to Cursor's tier form.
- **http_version_options** — values the operator can pass to
  `-http-version` / `CURSOR_PROXY_HTTP_VERSION`.

## `GET /v1/introspect/recent-tools?since=<duration>`

Aggregated view of tools that the client(s) declared in recent
requests. Backed by an in-memory ring buffer (4096 observations,
about 5 minutes at 10 tools/request × 1 request/second). Not
persisted across restarts.

Query parameters:

- **since** — window size. Accepts Go duration syntax (`60s`,
  `5m`, `1h`) or a bare integer of seconds. Default 60s, garbage
  values fall back to the default rather than erroring.

Response:

```json
{
  "since_seconds": 60,
  "sample_size": 9,
  "unique_tools": [
    {"name": "Bash", "requests": 3, "kind": "custom"},
    {"name": "mcp__filesystem__read_file", "requests": 3, "kind": "mcp", "server": "filesystem"},
    {"name": "mcp__github__create_issue", "requests": 1, "kind": "mcp", "server": "github"}
  ],
  "oldest_seconds": 59.9
}
```

- **sample_size** — total tool observations in the window (a
  request declaring 3 tools contributes 3). Compare against
  `sample_size` on `/v1/introspect/recent-mcp-servers` to derive
  the MCP-vs-custom ratio.
- **unique_tools** — deduplicated by name, sorted by `requests`
  descending. `kind` is either `"custom"` or `"mcp"`; `server` is
  populated only when kind is mcp.
- **oldest_seconds** — age of the oldest observation returned.
  When smaller than the requested window, you're seeing the whole
  ring (older data has aged out or the proxy hasn't been running
  long enough).

Recording rules:

- Server-side Anthropic tools (`web_search_20250305` etc.) are
  **not** recorded — they get 400-rejected upstream, and recording
  them would fake activity that never actually happened.
- Empty tool arrays and blank names are ignored.
- MCP name detection matches both `mcp__server__tool` (canonical
  Claude Code) and the looser `mcp_server_tool` form some aider
  builds emit.

## `GET /v1/introspect/recent-mcp-servers?since=<duration>`

Same ring buffer, projected onto the MCP server dimension. Useful
for cursor2api's Dashboard to show "your Claude Code is using
these MCP servers" without reaching into the client's own settings.

Response:

```json
{
  "since_seconds": 60,
  "sample_size": 9,
  "servers": [
    {
      "server": "filesystem",
      "requests": 3,
      "tool_names": ["mcp__filesystem__read_file"]
    },
    {
      "server": "github",
      "requests": 2,
      "tool_names": ["mcp__github__create_issue", "mcp__github__search_repos"]
    }
  ]
}
```

- **sample_size** — same window total as `/recent-tools` (all
  observations, including non-MCP), so downstream can compute
  "9 total observations, 5 of them MCP → 55% MCP traffic".
- **servers** — sorted by `requests` descending; `tool_names` per
  server is deduplicated and alphabetically sorted.

## What this is NOT

- **Not configuration.** These endpoints observe what the client
  did; they don't let you change tool policy or add MCP servers.
  Managing MCP / hooks / skills is out of scope for the current
  cursor-proxy design (see [claude-code-compat.md](claude-code-compat.md)
  for why, and the "Future SDK integration" discussion for what
  would be required to add management).
- **Not per-user.** cursor-proxy is single-tenant — one process,
  one Cursor account, one shared ring.
- **Not audit-grade.** The ring buffer is best-effort in-memory
  storage; a proxy restart or high-QPS traffic can age data out
  before you query. Use it for UI hints and diagnostics, not
  for compliance.
- **Not secret-bearing.** No prompts, message bodies, tool inputs,
  or account identifiers appear in any response. Only tool NAMES
  the client declared.

## Consumer contract

cursor2api can wire these into its UI without needing an API key:

```rust
// Rust sidecar supervisor
async fn probe_proxy(port: u16) -> anyhow::Result<()> {
    let caps: Capabilities = reqwest::get(
        format!("http://127.0.0.1:{port}/v1/capabilities")
    ).await?.json().await?;
    // Show / hide feature toggles based on caps.
    let recent = reqwest::get(
        format!("http://127.0.0.1:{port}/v1/introspect/recent-mcp-servers?since=5m")
    ).await?.json::<RecentMcpServers>().await?;
    // Populate "MCP servers used recently" card.
    Ok(())
}
```

Poll cadence: `/v1/capabilities` once at sidecar spawn, cached for
the process lifetime; `/v1/introspect/*` every UI refresh
(15–30s typical).
