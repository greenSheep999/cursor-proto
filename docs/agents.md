# Agent mode operator guide

**Introduced**: `cursor3.11/v0.3.0` (not back-ported to the 3.10 line).

Agent mode adds a `/v1/agents/*` HTTP surface backed by the official
`@cursor/sdk` Node package. It runs alongside the existing wire mode
(`/v1/messages` etc.) — both surfaces share the same process, the
same `-api-keys` gate, and the same upstream account, but they use
different Cursor authentication mechanisms.

If you don't need `/v1/agents/*`, ignore this document. Wire mode
keeps working exactly as it did in `v0.2.x`.

## Two modes, one process

|                 | Wire mode                               | Agent mode                                             |
|-----------------|-----------------------------------------|--------------------------------------------------------|
| Endpoints       | `/v1/messages`, `/v1/chat/completions`… | `/v1/agents/*`                                         |
| Cursor auth     | IDE `accessToken` from `state.vscdb`    | Dashboard-issued `CURSOR_API_KEY` (`crsr_...`)         |
| Backend path    | Go executor → private protobuf          | `@cursor/sdk` in a Node child process                  |
| Node.js needed? | No                                      | Yes (via `-node-runner` flag or the bundled binary)    |
| Capabilities    | Chat, tool-loop, streaming              | + codebase indexing / MCP servers / skills / hooks     |

Both modes go through `-api-keys` authentication.

## Enabling agent mode

Three ways, ordered by how much work you do:

### 1. Docker (`ghcr.io/greensheep999/cursor-proxy:cursor3.11-*`)

```bash
docker run -p 127.0.0.1:8317:8317 \
  -e CURSOR_PROXY_API_KEYS=sk-cp-... \
  -e CURSOR_API_KEY=crsr_... \
  ghcr.io/greensheep999/cursor-proxy:cursor3.11-latest
```

The image ships the Node runner at `/opt/cursor-node-runner/dist/index.js`
and `CURSOR_PROXY_NODE_RUNNER` is pre-set in the image environment, so
just providing `CURSOR_API_KEY` is enough.

### 2. Combined tarball (`cursor-proxy-cursor3.11-<platform>.tar.gz`)

Download the tarball from the GitHub release, extract, and:

```bash
tar -xzf cursor-proxy-cursor3.11-linux-amd64.tar.gz
cd cursor-proxy-cursor3.11-linux-amd64

CURSOR_API_KEY=crsr_... ./cursor-proxy \
  -addr 127.0.0.1:8317 \
  -node-runner ./cursor-node-runner/dist/index.js
```

Requires `node` on PATH (Node 22.13+; use `nvm install 22` or
Homebrew's `node@22`).

### 3. From source

```bash
# Build cursor-proxy
go build -o bin/cursor-proxy ./cmd/cursor-proxy

# Build node runner
cd node-runner
npm ci --production
npm run build
cd ..

CURSOR_API_KEY=crsr_... ./bin/cursor-proxy \
  -node-runner ./node-runner/dist/index.js
```

## Verifying it's up

`GET /v1/proxy-info` (unauthenticated) reports both modes:

```json
{
  "wire_mode":  {"available": true, "account_email": "..."},
  "agent_mode": {
    "available":    true,
    "sdk_version":  "1.0.23",
    "node_version": "v22.13.0",
    "runtimes":     ["local", "cloud"],
    "active_agents": 0,
    "active_runs":   0
  }
}
```

If `agent_mode.available == false`, the startup log will explain why:

- `[proxy] agent mode: CURSOR_API_KEY is set but -node-runner is empty; agent mode disabled`
- `[proxy] agent mode: -node-runner set but no CURSOR_API_KEY; agent mode disabled`
- `[proxy] agent mode: node runner failed to start (…); agent mode disabled`

## HTTP surface

All endpoints require the `-api-keys` gate (same as `/v1/messages`).

### Create an agent

```bash
curl -X POST -H "x-api-key: $KEY" -H "Content-Type: application/json" \
  http://127.0.0.1:8317/v1/agents \
  -d '{
    "runtime": "local",
    "cwd": "/path/to/repo",
    "model": {"id": "composer-2.5"}
  }'
```

Returns `{"agentId": "agent-<uuid>", "createdAt": "..."}`.

For a cloud runtime:

```json
{
  "runtime": "cloud",
  "model": {"id": "composer-2.5"},
  "repos": [{"url": "https://github.com/your-org/your-repo"}],
  "auto_create_pr": true
}
```

### List / status / close

```bash
curl -H "x-api-key: $KEY" http://127.0.0.1:8317/v1/agents
curl -H "x-api-key: $KEY" http://127.0.0.1:8317/v1/agents/agent-abc
curl -X DELETE -H "x-api-key: $KEY" http://127.0.0.1:8317/v1/agents/agent-abc
```

DELETE is idempotent: closing an already-gone agent returns
`{"ok": true, "already_gone": true}`.

### Run a prompt (non-streaming)

```bash
curl -X POST -H "x-api-key: $KEY" -H "Content-Type: application/json" \
  http://127.0.0.1:8317/v1/agents/agent-abc/runs \
  -d '{"prompt": "Summarize this repo", "stream": false}'
```

Returns:

```json
{
  "run_id":     "run-<uuid>",
  "final_text": "...",
  "tool_calls": [...],
  "usage":      {...}
}
```

### Run a prompt (streaming SSE)

```bash
curl -N -X POST -H "x-api-key: $KEY" -H "Content-Type: application/json" \
  http://127.0.0.1:8317/v1/agents/agent-abc/runs/stream \
  -d '{"prompt": "Fix the failing test in main_test.go"}'
```

SSE frames come out as:

```
event: run.started
data: {"run_id": "run-abc"}

event: run.event
data: {"type": "assistant", "delta": "Let me look..."}

event: run.event
data: {"type": "tool_call", "name": "Bash", "input": {...}}

event: run.done
data: {"runId": "run-abc", "usage": {...}}
```

### Cancel a run

```bash
curl -X POST -H "x-api-key: $KEY" \
  http://127.0.0.1:8317/v1/agents/agent-abc/runs/run-xyz/cancel
```

Idempotent. Returns 202 whether or not the run had already ended.

## Known limits (MVP)

- `GET /v1/agents/{id}/runs/{run_id}/stream` returns 501 today.
  Use the combined `POST /v1/agents/{id}/runs/stream` endpoint
  instead. Standalone stream-reconnect with a per-event
  `Last-Event-Id` buffer is queued for a follow-on patch.
- Agent-mode configuration (MCP servers, skills, hooks) lives on
  the operator's filesystem via the SDK's own conventions today;
  a `/v1/agent-config` HTTP surface is on the roadmap.
- Cloud runtime agents survive process restarts on Cursor's side;
  we don't currently expose "resume" (call `Agent.resume()` in a
  future patch). Local runtime agents die with the proxy process.

## Deployment sizes

|                     | Binary          | Docker image     |
|---------------------|-----------------|------------------|
| Wire only           | ~20 MB          | ~30 MB           |
| Wire + agent mode   | ~20 MB + ~15 MB | ~110 MB          |

The size delta is Node + `@cursor/sdk` + its runtime deps. If you
run wire-only, use the raw `cursor-proxy-<line>-<platform>` binary
(uploaded to the release alongside the tarball) — no Node needed.

## Troubleshooting

- **`node runner failed to start: exec: "node": executable file not found`** —
  Node isn't on the proxy's PATH. Install it (`nvm install 22` or
  the OS package), or pass `-node-binary /path/to/node`.
- **`agent.create failed: apiKey invalid or missing`** — the
  `CURSOR_API_KEY` is wrong. Dashboard-issued keys start with
  `crsr_`; the IDE's `accessToken` (from `state.vscdb`) is NOT
  interchangeable with this.
- **agent.send returns 502 sdk_upstream_error** — Cursor's own
  backend rejected the request. The error message from the SDK is
  in the response body; typically a model that's not enabled on
  your account, a region gate, or a rate limit.

## Related

- `docs/sdk-integration.md` — architecture + rationale for the two-mode design.
- `docs/versioning.md` — release tag scheme (`cursor3.11/v0.3.0` and later
  include agent mode).
- `docs/observability.md` — `/v1/capabilities` reports `agents.supported`
  so downstream can toggle UI without probing `/v1/agents` first.
