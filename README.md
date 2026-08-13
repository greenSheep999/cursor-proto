# cursor-proto

A Go client + reverse-engineered protocol library for Cursor IDE, plus an
OpenAI- and Anthropic-compatible HTTP proxy so any existing OpenAI/Anthropic
client can talk to Cursor's backend.

Built by rebuilding Cursor's private wire protocol from the shipped
`workbench.desktop.main.js` (~40 MB) — proto schemas, checksum algorithm,
machine-id derivation, session identifiers, and header set.

**Currently impersonates Cursor 3.15.19** on `main`. The 3.10.20 and 3.11.19 lines are
still maintained on `release/cursor-3.10` and `release/cursor-3.11`. See [docs/versioning.md](docs/versioning.md)
for the release contract and how downstream consumers should pin.

## Versioning

Every release targets one specific Cursor IDE version. Tags encode both
axes: `cursor<major>.<minor>/v<semver>` (e.g. `cursor3.10/v0.1.1`,
`cursor3.11/v0.2.0`). Older Cursor lines stay on long-lived
`release/cursor-<X.Y>` branches so they can receive patches after `main`
has moved on.

Any running binary can be introspected — no auth required:

```bash
cursor-proxy -version                          # CLI, prints JSON and exits
curl -s http://127.0.0.1:8317/v1/proxy-info    # HTTP, same JSON
```

Returns `cursor_line`, `impersonated_version`, `impersonated_commit`,
`release_hash`, and `proto_version` (the git tag CI built from, or
`"dev"` for local builds).

See [docs/versioning.md](docs/versioning.md) for the full release
contract — tag scheme, artifact names, Docker image tags, the
line-vs-impersonated distinction, and how downstream consumers (e.g.
`cursor2api`) should pin and verify.

## Two modes: wire + agent

Since `cursor3.11/v0.3.0`, cursor-proxy runs two independent HTTP
surfaces on the same port:

- **Wire mode** (`/v1/messages`, `/v1/chat/completions`, `/v1/responses`,
  `/v1beta/models/*`) — the OpenAI/Anthropic/Gemini-compatible surface
  we've always shipped. Authenticates upstream with the IDE's
  `accessToken` (auto-loaded from `state.vscdb`).
- **Agent mode** (`/v1/agents/*`) — new. Backed by the official
  `@cursor/sdk` in a Node child process. Authenticates upstream with
  a dashboard-issued `CURSOR_API_KEY` (`crsr_...`). Gives you Cursor's
  full agent runtime — codebase indexing, MCP servers, skills, hooks,
  subagents, cloud VMs.

Both modes share the `-api-keys` gate and coexist on the same port.
Agent mode is **optional** — leave `CURSOR_API_KEY` unset and only
wire mode runs (no Node process, no extra memory).

See [docs/agents.md](docs/agents.md) for the operator guide and
[docs/sdk-integration.md](docs/sdk-integration.md) for the architecture.

## Architecture

### High-level: how a request flows

```
┌────────────────────┐    OpenAI / Anthropic JSON    ┌────────────────────────┐
│  Any OpenAI or     │  ───────────────────────────▶ │      cursor-proxy      │
│  Anthropic client  │                                │  (this project)        │
│  (Cline, Claude    │ ◀────── SSE / JSON ─────────  │                        │
│   Code, Cursor CLI,│                                │  ┌──────────────────┐  │
│   custom scripts)  │                                │  │ auth middleware  │  │
└────────────────────┘                                │  │ (API keys)       │  │
                                                      │  └──────────────────┘  │
                                                      │  ┌──────────────────┐  │
                                                      │  │ translator       │  │
                                                      │  │ (in/out shape)   │  │
                                                      │  └──────────────────┘  │
                                                      │  ┌──────────────────┐  │
                                                      │  │ executor         │  │
                                                      │  │ (Cursor protocol)│  │
                                                      │  └──────────────────┘  │
                                                      └───────────┬────────────┘
                                                                  │
                                                                  ▼
                                          ┌──────────────────────────────────────────┐
                                          │        api2.cursor.sh (private)          │
                                          │  aiserver.v1.AiService/AvailableModels   │
                                          │  aiserver.v1.BidiService/BidiAppend      │
                                          │  aiserver.v1.DashboardService/GetMe …    │
                                          │  agent.v1.AgentService/RunSSE            │
                                          └──────────────────────────────────────────┘
```

### Chat: OpenAI request → Cursor RunSSE + BidiAppend

```
Client                cursor-proxy              executor                api2.cursor.sh
  │                       │                        │                          │
  │  POST /v1/chat/       │                        │                          │
  │  completions          │                        │                          │
  ├──────────────────────▶│                        │                          │
  │                       │ RequireAPIKeys         │                          │
  │                       │ (401 if wrong key)     │                          │
  │                       │                        │                          │
  │                       │ ChatRequest{model,     │                          │
  │                       │   messages, tools,     │                          │
  │                       │   system, history}     │                          │
  │                       ├───────────────────────▶│                          │
  │                       │                        │ build AgentRunRequest    │
  │                       │                        │ (proto), splice system   │
  │                       │                        │ prompt + history         │
  │                       │                        │                          │
  │                       │                        │ POST RunSSE ─────────────▶ (open SSE)
  │                       │                        │ POST BidiAppend ─────────▶ (send msg)
  │                       │                        │                          │
  │                       │                        │ ◀── heartbeat            │
  │                       │                        │ ◀── text_delta (sparse)  │
  │                       │                        │ ◀── KV assistant blob    │
  │                       │                        │ ◀── turn_ended + usage   │
  │                       │                        │                          │
  │                       │ ◀──── ChatEvent chan ──│                          │
  │  SSE:                 │                        │                          │
  │  data: {delta:role}   │                        │                          │
  │  data: {delta:content}│                        │                          │
  │  data: {finish:stop}  │                        │                          │
  │  data: [DONE]         │                        │                          │
  │◀──────────────────────│                        │                          │
```

### Tools: tools[] in → tool_calls out (MCP)

```
tools:[{name:get_weather}]              McpTools + RequestContext.Tools
      │                                    │
      ▼                                    ▼
OpenAI/Anthropic JSON  ──▶  AgentRunRequest.mcp_tools = [
                                   { name, description,
                                     input_schema: <Value>,  ← wrapped, NOT Struct
                                     provider_identifier: "cursor-tools" } ]
                                                │
                                                ▼
                                    Cursor server routes tool call
                                                │
                                                ▼
                             ExecServerMessage.mcp_args {
                                 tool_call_id, tool_name, args
                             }
                                                │
                                                ▼
                            translator emits OpenAI tool_calls
                            or Anthropic content_block[type=tool_use]
```

### Auth / checksum construction

```
Cursor OAuth (loginDeepControl)
        │
        ▼
    accessToken (JWT, ~60d)  ─────────────────────────────┐
        │                                                 │
        ├─────▶ savedAccount.json (also has refresh_token,│
        │       machine_id, mac_machine_id)               │
        │                                                 ▼
        ▼                                          Authorization: Bearer <JWT>
Every outbound request:                            x-cursor-checksum: <derived>

    session_start (time.Now())                       │
        │                                            │
        ▼                                            │
    E = unix_ms / 1e6                                │
    raw = 6 bytes packed by JS 32-bit shift rules    │
    obf = tVg(raw)  ← xor+add mixer, seed=165        │
    b64 = base64(obf)                                │
                                                     │
    machineID    = SHA-256(IOPlatformUUID)   ─────┐  │
    macMachineID = SHA-256(first MAC address) ────┼──┤
                                                  │  │
    checksum = <b64><machineID>/<macMachineID>  ──┴──┘
```

## Status

| Phase | Content | Status |
|---|---|---|
| 1 | Cursor 3.10 proto schema extraction | ✅ 823 messages + 52 enums |
| 2 | IDE traffic capture / header baseline | ✅ 75 real requests, 25 required headers |
| 3 | Go proto codegen | ✅ `gen/cursor/cursor.pb.go` (2.5 MB) |
| 4 | Auth module (checksum + machine id + OAuth) | ✅ macMachineID byte-perfect vs IDE |
| 5 | Executor (chat / agent RunSSE + BidiAppend) | ✅ end-to-end AI reply |
| 6 | Translator (OpenAI + Anthropic SSE) | ✅ HTTP proxy live |
| 7-A | MCP tools (tools[] → tool_calls) | ✅ `get_weather(Paris)` round-trips |
| 7-B | SSE streaming | ✅ standard OpenAI/Anthropic SSE; per-token pacing not available (server-side limit — see [Streaming section](#streaming--what-it-is-and-isnt)) |
| 7-C | API key auth middleware | ✅ constant-time compare, env + flag |
| 7-D | Multi-turn conversation | ✅ "Remember 42" test passes |
| 7-E | Docker + CI + release workflow | ✅ Dockerfile + 2 GHA workflows |
| 7-F | CPA integration | ✅ converter + plugin skeleton |
| 7-G | Usage / rate-limit / country snapshot | ✅ `/v1/usage` endpoint |

## Repository layout

```
cursor-proto/
├── auth/            Checksum, machine-id, OAuth device flow, account JSON
├── executor/        HTTP client, RunSSE + BidiAppend, header assembly
├── translator/      Cursor events → OpenAI / Anthropic wire format
├── proto/           cursor.proto (source of gen/*)
├── gen/cursor/      Generated Go protobuf types (cursor package = cursorpb)
├── cmd/
│   ├── cursor-proxy/   OpenAI + Anthropic + Gemini + Responses HTTP endpoints backed by Cursor
│   ├── cursor-login/   Interactive OAuth CLI, writes account JSON
│   ├── cursor-export/  One-shot: reads the signed-in Cursor IDE account and writes a CPA-compatible auth JSON
│   ├── cursor-to-cpa/  Converts a cursor-login account JSON into CPA auth JSON (batch mode too)
│   ├── test-chat/      End-to-end RunSSE dumper
│   ├── test-connect/   Unary AvailableModels verifier
│   ├── test-features/  SystemPrompt / PureMode / AutoStop toggles
│   └── test-*          Various diagnostic tools
├── docs/            Reverse-engineering reports (schema-3.10, checksum, pitfalls)
├── captures/        raw schema JSON, mitmproxy traffic samples
├── reference/       Original JS sources (CursorGateway) used during RE
└── scripts/         Python extractors: extract_schema.py, gen_proto.py
```

## Prerequisites

- Go 1.24+
- Cursor 3.10.20 installed and signed in (the tools read your access token from
  Cursor's SQLite storage: `~/Library/Application Support/Cursor/User/globalStorage/state.vscdb`)
- macOS (Linux + Windows machine-id support is scaffolded but untested)
- CGO enabled (for `mattn/go-sqlite3` — the SQLite reader used by `test-connect`)

## Quick start

```bash
# 1. Build the OpenAI/Anthropic bridge
CGO_ENABLED=1 go build -o cursor-proxy ./cmd/cursor-proxy

# 2. Start it (reads your Cursor IDE token automatically on macOS)
./cursor-proxy -addr 127.0.0.1:8317

# With API key protection
CURSOR_PROXY_API_KEYS=sk-mypersonalkey ./cursor-proxy -addr 127.0.0.1:8317
curl -H "Authorization: Bearer sk-mypersonalkey" http://127.0.0.1:8317/v1/models
# (equivalent flag form: ./cursor-proxy -addr 127.0.0.1:8317 -api-keys sk-mypersonalkey,sk-second)

# 3. Talk to it with any OpenAI client
curl -N http://127.0.0.1:8317/v1/chat/completions \
  -H "content-type: application/json" \
  -d '{
    "model":"composer-2.5",
    "messages":[
      {"role":"system","content":"Reply in pirate speak."},
      {"role":"user","content":"say hello"}
    ],
    "stream":true
  }'

# Or with any Anthropic-compatible client
curl -N http://127.0.0.1:8317/v1/messages \
  -H "content-type: application/json" \
  -d '{
    "model":"composer-2.5",
    "system":"Reply in pirate speak.",
    "messages":[{"role":"user","content":"say hello"}],
    "stream":true,
    "max_tokens":100
  }'
```

Endpoints exposed by `cursor-proxy`:

- `GET  /v1/models` — full Cursor model catalog
- `POST /v1/chat/completions` — OpenAI Chat Completion (streaming + non-streaming)
- `POST /v1/messages` — Anthropic Messages (streaming + non-streaming)

## Which models work?

`GET /v1/models` always returns the full catalog Cursor advertises (156 models
at the time of writing). Whether a *specific* model is actually callable is
**decided per-account**, not per-project or per-IP.

Cursor stores a `country` field on every user account (queryable via
`DashboardService/GetMe`, e.g. our test account shows `country: "CN"`). Some
model providers (notably Anthropic Claude and the Claude Fable family) refuse
to serve accounts whose country code is on a restricted list — **regardless
of the IP the request comes from**. Routing traffic through an out-of-region
SOCKS5 proxy does NOT unlock these models; verified empirically by connecting
from a US residential IP (Charter/Comcast/RCN) and still getting
`"Model not available: This model provider is not supported in your region"`.

What this means:

| Account country | Behaviour |
|---|---|
| US / EU / other permitted | All 156 models callable |
| CN / other restricted | `composer-2.5*`, `grok-4.5*`, `gpt-5.5*`, `default` work; `claude-*` and `claude-fable-*` return the region error |

If a call fails with `"This model provider is not supported in your region"`,
that's an account-scoped restriction the proxy can't work around — you need
an account whose registered country permits the model (typical fix: sign up
with a US billing method and IP). There's no protocol issue on our side;
error responses pass straight through to the client.

## Streaming — what it is and isn't

**Yes, the proxy supports streaming.** Requests with `"stream": true` return
proper Server-Sent Events (`text/event-stream`, `data: {...}\n\n` frames,
terminating `data: [DONE]`), fully compatible with OpenAI Chat Completion
and Anthropic Messages clients out of the box. `stream_options.include_usage`
is honoured on the OpenAI side and produces a final usage-only chunk before
`[DONE]`.

**What is not available is *per-token* streaming** (the "typewriter"
animation). Cursor's own `agent.v1/RunSSE` protocol emits the assistant's
final answer as a **single blob at the end of the turn**, not token by
token — we ship that blob as one `content` delta and close the stream.

Empirically confirmed:

- All legacy `aiserver.v1.ChatService/Stream*` methods have been sunset for
  3.10 clients (they return `Update Required` or 404). See
  `docs/phase-7m-legacy-streaming.md`. There is no alternative endpoint
  that delivers dense per-token deltas.
- The current path emits: 1 role frame → 1 content frame (the full answer)
  → 1 finish frame → `[DONE]`. Total: 3 SSE data frames per turn, all
  arriving within the same ~100ms window at end-of-turn.

**If you want typewriter animation, do it in your client** — receive the
full content chunk, then render one character at a time locally. The proxy
deliberately does not fake per-token pacing (that would only add latency,
not information).

## Prompt caching

Cursor's server auto-caches the model's system prompt (typically
~4675 tokens for `composer-2.5`) and reports the counter in every response.
The proxy forwards it in the standard OpenAI / Anthropic usage shape:

```jsonc
// OpenAI /v1/chat/completions
"usage": {
  "prompt_tokens": 13080,
  "completion_tokens": 215,
  "total_tokens": 13295,
  "prompt_tokens_details":  { "cached_tokens": 4675 },
  "completion_tokens_details": { "reasoning_tokens": 0 }
}

// Anthropic /v1/messages
"usage": {
  "input_tokens": 8405,              // total_input − cache_read
  "output_tokens": 115,
  "cache_read_input_tokens": 4675,
  "cache_creation_input_tokens": 0   // Cursor doesn't accept client-side writes
}
```

### Not supported: client-directed cache writes

Anthropic's official API lets clients declare `"cache_control": {"type": "ephemeral"}`
on a content block to trigger a 5-minute paid cache (90% off on subsequent
reads). **Cursor's server ignores that field entirely.** You cannot use this
proxy to pay Anthropic-style cache rates — Cursor bills you by its own
rules (fast/slow request quotas, not Anthropic's token pricing). The cache
numbers we do report (`cache_read_input_tokens`) come from Cursor's own
server-side cache of its internal system prompt, not from anything the
client asked for.

TL;DR: **the cached_tokens field is informational; sending
`cache_control` doesn't save you money.**

## Non-obvious findings

Documented in `docs/`:

- **checksum-algorithm.md** — JS's 32-bit shift oddities recovered, the
  timestamp is snapshot-once-per-session, `machineID = SHA-256(IOPlatformUUID)`,
  `macMachineID = SHA-256(first MAC address)`.
- **phase-2-report.md** — full list of the 25 required headers, incl. the
  four that CursorGateway missed (`x-client-key`, `x-cursor-config-version`,
  `x-cursor-client-layout: unifiedAgent`, `x-new-onboarding-completed`).
- **phase-5-report.md** — RunSSE lives on api2 not api3; the "AgentClientMessage"
  envelope is a bare `{ field 1: AgentRunRequest }`; `ConversationState.mode`
  is required.
- **phase-6-report.md** — `custom_system_prompt` field is server-rejected
  (regardless of harness); assistant final text is in a KV blob rather than
  `text_delta`; the auto-stop heuristic needs to wait for the assistant blob.

## Tools

| Binary | What it does |
|---|---|
| `cursor-proxy` | The HTTP bridge (OpenAI Chat + Responses + Anthropic + Gemini) |
| `cursor-login` | Runs the OAuth device flow, writes an account JSON file |
| `cursor-export` | Reads the currently signed-in Cursor IDE account from `state.vscdb` and writes a CPA-compatible auth JSON in one step |
| `cursor-to-cpa` | Converts a `cursor-login` account JSON into CPA auth JSON (single-file or `--dir-in` batch) |
| `test-chat` | Streams a single chat and dumps every server event |
| `test-connect` | Sanity-checks `/v1/models` end-to-end |
| `test-features` | Toggle SystemPrompt / PureMode / Harness / AutoStop |
| `test-kv` | Dumps KV blob contents for reverse engineering |
| `test-sniff` | Prints blob head bytes for the auto-stop heuristic |

## Regenerating the proto

If Cursor ships a new release:

```bash
# 1. Point at the new workbench.desktop.main.js
cp "/Applications/Cursor.app/Contents/Resources/app/out/vs/workbench/workbench.desktop.main.js" \
   captures/wb-latest.js

# 2. Extract schema
python3 scripts/extract_schema.py > captures/schema-latest.raw.json

# 3. Regenerate .proto (core mode = chat + agent essentials)
python3 scripts/gen_proto.py --mode core

# 4. protoc → Go
protoc --proto_path=proto --go_out=gen --go_opt=paths=source_relative proto/cursor.proto

# 5. Bump the release hash constant if it changed
grep -oE '[a-f0-9]{64}' /Applications/Cursor.app/Contents/Resources/app/product.json  # or via IDE main.log update URL
# then update auth.KnownReleaseHash_3_10_20
```

## Docker

Prebuilt image workflow — see `Dockerfile` and `docker-compose.yml`.

```bash
# 1. Put an account JSON where compose can see it.
#    Generate one with `cursor-login` (writes cursor-<email>.json), then:
mkdir -p accounts
cp cursor-you@example.com.json accounts/current.json

# 2. Start the proxy (localhost-only bind by default)
docker compose up -d --build

# 3. Smoke-test
curl http://127.0.0.1:8317/v1/models
```

Environment variables:

- `CURSOR_PROXY_API_KEYS` — comma-separated allowlist for the proxy's future
  `-api-keys` gate (empty = unauthenticated; do not leave empty on a public
  network).
- `CURSOR_PROXY_ACCOUNT_FILE` — absolute path inside the container to the
  account JSON. `docker-compose.yml` mounts `./accounts` at `/data/accounts:ro`
  and defaults this to `/data/accounts/current.json`.

The compose file binds the port to `127.0.0.1:8317` by default. To expose it
on all interfaces (only behind a trusted network), change `ports:` to
`"8317:8317"`.

Prebuilt binaries are published from tag pushes (`v*.*.*`) via
`.github/workflows/release.yml`:

- `linux/amd64`, `linux/arm64`, `darwin/arm64`

The release workflow does not push a Docker image yet — that is a future
improvement.

## Disclaimer

This project exists purely for **research, study, and educational purposes** —
protocol analysis, reverse-engineering practice, and interoperability
experiments. It is **not a product**, not affiliated with any commercial
entity, and not intended for production or commercial use.

**No affiliation.** Independent research project. Unrelated to Anysphere Inc.,
Cursor, or any of their subsidiaries, partners, or brands.

**No warranty.** Provided **AS IS**. Use is entirely at your own risk. You
are solely responsible for what you do with the code and for compliance
with all applicable terms of service, laws, and regulations in your
jurisdiction.

## License

Licensed under the **Apache License, Version 2.0** — see [LICENSE](LICENSE).

Apache-2.0 was chosen over MIT for the explicit patent grant. If you find
this project useful, or fork it, keep the [NOTICE](NOTICE) file intact and
credit the upstream reference material.

Third-party attributions:

- `reference/js-src/` — snapshots of
  [CursorGateway](https://github.com/taxue2016/CursorGateway) (MIT), retained
  as read-only cross-reference during reverse engineering. **No Go code in
  this repository is a direct port of CursorGateway.**
- `captures/`, `proto/cursor.proto` — schema derived from Cursor.app's
  public JS bundle. Cursor and Anysphere retain all rights to their APIs
  and brand.

Full attributions in [NOTICE](NOTICE).
