# Official `@cursor/sdk` integration — design & roadmap

**Status**: proposed for cursor-proto `cursor3.11/v0.3.0`. Multi-phase.
**Filed**: this is the on-repo record of the "should we integrate the
official SDK?" thread that closed with "yes, complete integration,
consider fusion with the reverse proxy."

---

## Why (recap)

Cursor released `@cursor/sdk` (TypeScript, public beta since 2026-04)
that gives programmatic access to Cursor's agent runtime — codebase
indexing, MCP servers, skills, hooks, subagents, cloud VMs, and
artifact downloads. Those are the capabilities Claude Code / aider /
cline **can't** reach through our current wire proxy (see
`docs/claude-code-compat.md`).

The proxy will keep working for what it does today (OpenAI /
Anthropic / Gemini HTTP compatibility over the IDE's account token);
the SDK adds a **second mode** on top of the same binary.

Three user personas after this integration ships:

1. **Existing users**: keep hitting `/v1/messages` etc. — no change.
2. **SDK-curious users**: same account, additionally get
   `/v1/agents/*` endpoints backed by @cursor/sdk. Can now use MCP
   servers configured in the sidecar, codebase indexing on a repo
   the sidecar has access to, and hooks/skills without needing a
   Node harness of their own.
3. **New "agent-first" users**: cursor2api's UI can offer a
   zero-client-install flow — no Claude Code needed. Menu-bar app
   spawns cursor-proxy, cursor-proxy runs an SDK agent, user gets
   a chat window in the tray.

---

## Two-axis mode model

The proxy will operate as **two coexisting HTTP surfaces on the same
port**, each with its own auth model:

| Mode | Endpoints | Backend | Auth |
|---|---|---|---|
| **Wire proxy** (existing) | `/v1/messages`, `/v1/chat/completions`, `/v1/responses`, `/v1beta/models/*`, `/v1/models`, `/v1/usage`, `/v1/proxy-info`, `/v1/capabilities`, `/v1/introspect/*` | Go executor talking Cursor's private protobuf directly | IDE `accessToken` from `state.vscdb` snapshot |
| **Agent** (new) | `/v1/agents/*`, `/v1/agent-config/*` | `@cursor/sdk` in a Node child process | `CURSOR_API_KEY` from Cursor dashboard |

**Both modes share** — `-api-keys` gate, `-http-version`, upstream
proxy, /v1/proxy-info introspection, observation ring buffer,
Prometheus metrics.

**Neither mode requires the other** — a build with no
`CURSOR_API_KEY` disables `/v1/agents/*` at the mux level, returning
`503 Service Unavailable` with a helpful message. A build with no
IDE state.vscdb (headless, `-token-file` off, no key either)
disables wire mode symmetrically. The most useful deployment has
both, but neither is a hard dependency.

---

## Architecture

```
┌───────────────────────────────────────────────────────────────┐
│                       cursor-proxy (Go)                       │
│                                                               │
│  ┌────────────────┐        ┌────────────────────────────┐     │
│  │  HTTP mux      │        │  Node runner (child proc)  │     │
│  │                │        │                            │     │
│  │  Wire endpoints├─────▶  │  runs @cursor/sdk          │     │
│  │  Agent endpoints├───┐   │  stdio JSON-RPC protocol   │     │
│  │                │   │   │                            │     │
│  └────────────────┘   │   │  1 process = N agents      │     │
│         │             │   │  spawned lazily on first   │     │
│         │             │   │  /v1/agents request        │     │
│         ▼             ▼   │                            │     │
│  ┌────────────┐    ┌──────┴────────────────────────┐    │     │
│  │ executor/  │    │   sdk/client (Go supervisor)  │    │     │
│  │ (proto RPC)│    │   - spawn / supervise         │    │     │
│  │            │    │   - JSON-RPC send/recv        │    │     │
│  │  api2.cursor.sh│   │   - stream event pump         │    │     │
│  │            │    │   - health check              │    │     │
│  └────────────┘    └───────────────────────────────┘    │     │
│                                                         │     │
└─────────┼───────────────────────────────────────────────┼─────┘
          │                                               │
          ▼                                               ▼
      Cursor backend                                Cursor backend
      (private proto)                              (SDK — same server,
                                                    different auth)
```

**Key decision: one Node process, N agents.**

Not one process per agent. Reasoning:
- Node startup is ~200-500 ms; spawning per agent kills UX.
- `@cursor/sdk` accepts multiple `Agent.create()` calls in one
  process; that's what it's designed for.
- One process to supervise = simpler restart / crash recovery.
- Memory: SDK's local execution mode buffers workspace state
  per-agent; N agents in one process is what Cursor's own SDK
  examples do.

Downside: a crash in the Node process takes down all agents. We
mitigate with auto-restart on abnormal exit + a max-agents cap.

---

## IPC protocol: line-delimited JSON-RPC over stdio

Both directions communicate via **one JSON message per line** on
the child's stdin (Go → Node) and stdout (Node → Go). stderr stays
unstructured for panic traces and Node warnings — captured to our
existing log stream with a `[node]` prefix.

**Request** (Go → Node):
```json
{"jsonrpc":"2.0","id":42,"method":"agent.create","params":{
  "runtime":"local","cwd":"/path","model":"composer-2.5",
  "cursor_api_key":"crsr_..."}}
```

**Response** (Node → Go):
```json
{"jsonrpc":"2.0","id":42,"result":{"agent_id":"agent-uuid"}}
```

**Server-initiated events** (Node → Go, no id):
```json
{"jsonrpc":"2.0","method":"run.event","params":{
  "run_id":"run-uuid","event":{"type":"assistant","delta":"..."}}}
```

**Methods** (initial set):
- `agent.create(runtime, cwd, model, envVars?)` → `agent_id`
- `agent.list()` → `[agent_id]`
- `agent.close(agent_id)` → ok
- `agent.send(agent_id, message)` → `run_id`
- `run.cancel(run_id)` → ok
- (event) `run.event` — one per SDK stream event
- (event) `run.completed(run_id, final_text, usage)` — end marker

Correlation: every Go-initiated request has a monotonic `id`; Node
must echo it on the response. A pending map on the Go side keys `id
→ chan Response`. Timeout: 60s for `agent.create` (SDK does a
handshake with Cursor), 5s for everything else, unbounded for
`agent.send` (streams).

---

## HTTP surface (agent mode)

Modeled after the SDK's own `Agent` / `Run` split. All endpoints
require the `-api-keys` gate (same as `/v1/messages`) — SDK mode
doesn't get free access.

### `POST /v1/agents`
Create an agent. Body mirrors `Agent.create()`:
```json
{
  "runtime": "local",
  "cwd": "/path/to/repo",
  "model": {"id": "composer-2.5", "params": [{"id":"fast","value":"true"}]},
  "env_vars": {"STAGING_TOKEN": "..."}
}
```
Returns `{"agent_id": "agent-uuid", "created_at": "..."}`.

### `GET /v1/agents`
List agents this proxy currently supervises. Not persisted across
restarts — the SDK's cloud runtime is what persists agents; local
runtime is process-scoped.

### `GET /v1/agents/{id}`
Agent status: model, cwd, active runs count, created_at.

### `POST /v1/agents/{id}/runs`
Start a run. Body:
```json
{"prompt": "...", "stream": true}
```
When `stream=false`: blocks until `end_turn`, returns full run.
When `stream=true`: returns `{"run_id":"..."}` immediately, client
connects to `/stream` for events.

### `GET /v1/agents/{id}/runs/{run_id}/stream`
SSE stream of SDK events. Format:
```
event: assistant
data: {"delta":"Hello"}

event: tool_call
data: {"name":"Bash","input":{"command":"ls"}}

event: task
data: {"status":"completed"}
```
Reconnect via `Last-Event-Id` header (backed by an SDK-level buffer).

### `POST /v1/agents/{id}/runs/{run_id}/cancel`
Best-effort cancel. Response 202 whether or not the run had already
finished — cancel is idempotent.

### `DELETE /v1/agents/{id}`
Close and release. All in-flight runs are cancelled.

---

## Configuration surface (agent mode)

Wire-proxy mode is stateless. Agent mode needs *some* state (MCP
servers, skills, hooks). Two options considered:

### Option A: HTTP config API
`POST /v1/agent-config/mcp {name, cmd/url}`, `GET /v1/agent-config/mcp`,
`DELETE /v1/agent-config/mcp/{name}`. Persistent, edited via HTTP.

### Option B: File-based config, hot-reloaded
`~/.config/cursor-proxy/agent-config.yaml` — the file IS the source
of truth; HTTP endpoint just `GET`s it.

**Chosen: B.** Reasoning: cursor2api owns the UI and has its own
persistence (Rust Tauri Store); one source of truth avoids drift.
Editing MCP config via HTTP would need auth we don't have. File
approach matches how Claude Code / Cursor IDE do it, so users can
share config across clients.

Config shape:
```yaml
# ~/.config/cursor-proxy/agent-config.yaml
mcp_servers:
  filesystem:
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/repos"]
  github:
    url: "https://mcp.githubmcp.io"
    headers: {Authorization: "Bearer ${GITHUB_TOKEN}"}

skills_dir: ~/.config/cursor-proxy/skills/
hooks:
  preToolUse: ~/.config/cursor-proxy/hooks/pre-tool.sh
```

`GET /v1/agent-config` echoes the parsed config so cursor2api's UI
can display what's active without re-parsing the file.

---

## Authentication fusion

Two secrets. Two purposes.

| Secret | Source | Powers | Rotates on |
|---|---|---|---|
| IDE `accessToken` | `state.vscdb` snapshot | Wire mode (`/v1/messages` etc.) | IDE re-login |
| `CURSOR_API_KEY` | Dashboard-issued (`crsr_...`) | SDK mode (`/v1/agents/*`) | User rotates |

Flag / env plumbing:

```
-cursor-api-key <key>          new flag
CURSOR_API_KEY                 env fallback
```

Priority: flag > env > (nothing → SDK mode disabled).

The key is passed to Node at process startup as env, never on stdin
(so it doesn't sit in a JSON message that could get logged). Node
uses it as `apiKey` in every `Agent.create()`.

**Rotation**: setting `CURSOR_API_KEY` env, then `SIGHUP` (or POST
`/v1/agent-config/reload`) tells the runner to swap it. Existing
agents keep their current key until they close.

---

## `/v1/proxy-info` extensions

```json
{
  "cursor_line": "3.11",
  "impersonated_version": "3.11.19",
  "impersonated_commit": "...",
  "release_hash": "...",
  "proto_version": "cursor3.11/v0.3.0",
  "http_version": "auto",

  "wire_mode": {"available": true, "account_email": "..."},
  "agent_mode": {
    "available": true,
    "sdk_version": "@cursor/sdk@1.0.23",
    "node_version": "v22.13.0",
    "runtime_supported": ["local", "cloud"],
    "active_agents": 2
  }
}
```

The `agent_mode.available` flag lets cursor2api's UI decide whether
to render agent-related controls without probing individual
endpoints.

---

## `/v1/capabilities` extensions

New fields:
```json
{
  "agents": {
    "supported": true,
    "runtimes": ["local", "cloud"],
    "mcp_management": true,
    "skills": true,
    "hooks": true,
    "subagents": true,
    "artifacts": true
  }
}
```

When `agent_mode.available == false` (no Node, or no
`CURSOR_API_KEY`), `agents.supported` is `false` and all sub-flags
`false` — one field to check.

---

## Docker & deployment

Current `Dockerfile` is Go-only, ~20 MB image. Adding Node.js:

```dockerfile
FROM node:22-alpine AS node-base
# @cursor/sdk pulls in native subprocess bindings — need build deps
RUN apk add --no-cache python3 make g++
WORKDIR /app
COPY sdk/node-runner/package.json sdk/node-runner/package-lock.json ./
RUN npm ci --production
COPY sdk/node-runner/ ./

FROM golang:1.24-alpine AS go-build
# ... existing Go build ...

FROM alpine:3.20
RUN apk add --no-cache ca-certificates sqlite-libs tzdata nodejs npm
COPY --from=go-build /out/cursor-proxy /usr/local/bin/
COPY --from=node-base /app /opt/cursor-node-runner
ENTRYPOINT ["cursor-proxy", "-node-runner=/opt/cursor-node-runner/index.js"]
```

Image size grows ~80 MB (Node runtime + @cursor/sdk transitive
deps). Documented cost.

Binary release (macOS / Linux tarballs) picks up an adjacent
`cursor-node-runner/` directory; if it's missing, agent mode
disables cleanly. So a stripped binary release stays useful for
wire-only deployments.

---

## Phased delivery

### Phase 0 — this document
Design lock-in. **← we are here**

### Phase 1 — Node runner MVP (~3-4 days)
- `sdk/node-runner/` TypeScript project
- Implement `agent.create/list/close/send`, `run.cancel`
- stdio JSON-RPC framing
- Real @cursor/sdk calls; no HTTP surface yet
- Unit tests: mock the SDK, verify JSON-RPC framing

### Phase 2 — Go supervisor (~2-3 days)
- `executor/sdk/` package
- Spawn / restart / health-check Node subprocess
- JSON-RPC framing + request/response correlation
- Stream event pump into Go channels
- Unit tests: fake Node runner via test harness

### Phase 3 — HTTP endpoints (~3-4 days)
- `/v1/agents/*` handler tree
- SSE streaming with `Last-Event-Id` reconnect
- All endpoints wired to the Go supervisor
- Route registration in `main.go`
- Integration tests: real Node runner via httptest.Server

### Phase 4 — Config + auth fusion (~2 days)
- `-cursor-api-key` flag + env
- `~/.config/cursor-proxy/agent-config.yaml` loader
- `/v1/agent-config` read endpoint
- `/v1/proxy-info` and `/v1/capabilities` extensions
- Reload on SIGHUP

### Phase 5 — Docker + release (~1-2 days)
- Multi-stage Dockerfile with Node
- Release workflow: tarball includes `cursor-node-runner/`
- GHCR image tags land as normal

### Phase 6 — End-to-end + docs + tag (~2 days)
- E2E test against real Cursor backend
- README + docs/agents.md
- Ship as `cursor3.11/v0.3.0` (minor bump: new endpoint tree, new
  dependency)
- Decide 3.10 back-port: **probably not** — 3.10 line is on
  security-patch-only, and SDK integration is a feature. cursor2api
  can require 3.11 for agent features.

**Total budget: ~14 working days = 3 calendar weeks with buffer.**

---

## Risks & mitigations

- **SDK is beta**: shape may change. Mitigation: pin
  `@cursor/sdk@1.0.23` in `package.json`, upgrade deliberately.
- **Node process leak**: dying badly leaves zombies. Mitigation:
  Go supervisor sends `SIGTERM` on parent exit, `procctl` on Linux
  to die-with-parent, `SIGHUP` handler in Node to graceful close.
- **Auth split confuses users**: two secrets, two purposes.
  Mitigation: `/v1/proxy-info` clearly separates `wire_mode` and
  `agent_mode` availability; docs use consistent naming.
- **Docker image size**: 20 MB → 100 MB. Mitigation: document, and
  keep binary release path Node-free for lean deployments.
- **Cursor deprecates SDK or adds attestation**: SDK stops working.
  Mitigation: agent mode disables cleanly (proxy-info reports
  false), wire mode unaffected. We keep both alive so a hostile
  SDK deprecation doesn't take us down.

---

## Non-goals (explicitly out of scope)

- **Windows support** — Node runner has Windows-specific quirks
  around subprocess management; ship Linux + macOS first.
- **Per-request runtime selection** — SDK mode is process-wide.
  If you need both modes on one host, run two proxies.
- **Auto-fallback from agent to wire mode** — if `/v1/agents` fails,
  clients get an error; they must retry against `/v1/messages`
  themselves. Silent fallback would mask real problems.
- **cursor2api-specific config injection** — this repo doesn't
  encode cursor2api's UI model. Agent config is a plain YAML file
  cursor2api can write to; how cursor2api's Tauri Store maps onto
  it is cursor2api's problem.

---

## Open questions (to answer during implementation)

1. **Skills directory format** — does @cursor/sdk expect a
   filesystem layout (per Claude Code), or does it want JSON
   descriptions? To be verified against the SDK docs during
   Phase 4.
2. **Hooks execution model** — SDK hooks are async callbacks in
   Node; do we shell them out to arbitrary user scripts, or
   require they be Node modules? Suspect the former; TBD.
3. **Cloud runtime state** — `Agent.create({cloud: {...}})`
   agents outlive our process. Do we expose "resume" for agents
   spawned in a previous session? Deferred; ship local-only in v1.
4. **Rate limiting between the two modes** — Cursor's own rate
   limits are per-account; running wire + agent modes on the
   same account will share the quota. Do we need proxy-side
   fairness? Probably not for v1 — document the shared quota
   and let users self-throttle.

---

## Green-lights required before Phase 1

- [ ] User accepts the Node.js dependency in Docker
- [ ] User accepts the auth-split (two secrets, two modes)
- [ ] User accepts the phased tag naming (`v0.3.0` minor bump, no
      3.10 back-port)
- [ ] User confirms cursor2api roadmap actually wants /v1/agents
      (vs. observation-only, which is already shipped in v0.2.6)

Once green, phase 1 starts immediately.
