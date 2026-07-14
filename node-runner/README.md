# cursor-proxy-node-runner

Node.js child process that runs `@cursor/sdk` on behalf of the
Go `cursor-proxy` binary. See `docs/sdk-integration.md` in the
repo root for the full protocol contract and architectural
rationale.

## What this is

- A **single Node process per cursor-proxy process** (not one
  process per agent).
- A **line-delimited JSON-RPC server** over stdin/stdout.
- A wrapper around `@cursor/sdk`'s `Agent` and `Run` primitives —
  we don't add semantics beyond what the SDK gives us.

## What this is not

- Not a standalone daemon. It has no HTTP surface, no port bind,
  no listen loop. It only speaks the private stdio protocol to
  its parent Go process.
- Not persistent. On parent exit the process dies with it; on
  crash the parent restarts a fresh copy.
- Not authenticated on the wire. It trusts stdin because stdin
  can only come from the parent process. The `CURSOR_API_KEY`
  it uses arrives via env, never via stdin.

## Protocol

Every message is one JSON object, one line, `\n`-terminated.

**Request** (parent → child):
```json
{"jsonrpc":"2.0","id":42,"method":"agent.create","params":{...}}
```

**Response** (child → parent, correlated by `id`):
```json
{"jsonrpc":"2.0","id":42,"result":{...}}
```
or
```json
{"jsonrpc":"2.0","id":42,"error":{"code":-32603,"message":"..."}}
```

**Server-initiated notifications** (child → parent, no `id`):
```json
{"jsonrpc":"2.0","method":"run.event","params":{"run_id":"...","event":{...}}}
```

Methods are documented in `src/protocol.ts`.

## Auth

The Node process reads `CURSOR_API_KEY` from its environment at
startup and uses it as the `apiKey` for every `Agent.create()`
call. The parent process is responsible for setting this env
before spawning the child; the child never receives the key
over stdin. If unset, `agent.create` requests return an error.

## Local development

```bash
cd node-runner
npm install
npm run build

# Drive it manually — type JSON, press enter, watch responses.
CURSOR_API_KEY=crsr_xxx node dist/index.js
> {"jsonrpc":"2.0","id":1,"method":"ping"}
{"jsonrpc":"2.0","id":1,"result":{"pong":true,"sdk_version":"1.0.23"}}
```
