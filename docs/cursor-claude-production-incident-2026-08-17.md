# Cursor Claude production incident — 2026-08-17

## Scope

This note records the production failure modes found while routing Claude Code
through TokenSheep/New API channel 16, CPA's Cursor plugin, and the Chromium
transport sidecar. Credentials, API keys, proxy URLs, account IDs, and request
content are intentionally omitted.

## Request path

```text
Claude Code
  -> api.tokensheep.fun (New API / TokenSheep)
  -> CPA channel 16
  -> cursor.so
  -> Chromium sidecar on 127.0.0.1:18901
  -> Cursor upstream
```

## Shared-network-namespace failure

The sidecar uses Docker's `network_mode: container:<cpa-container>` and binds
only to `127.0.0.1:18901`. Restarting CPA alone recreates its network namespace,
while the already-running sidecar can remain attached to the old namespace.
This creates a misleading split-brain state:

- `/healthz` succeeds when called inside the sidecar container;
- CPA receives `dial tcp 127.0.0.1:18901: connect: connection refused`.

Recovery order:

1. restart CPA;
2. restart the Chromium sidecar so it joins CPA's current namespace;
3. wait for `http://127.0.0.1:18901/healthz`;
4. reload Cursor auth files/live model catalogs;
5. run one real Claude request.

The deployment workflow should encode this ordering instead of relying on an
operator to remember it.

## HTTP 200 malformed/empty Claude Code responses

The exact client-side reproduction was:

```text
API returned an empty or malformed response (HTTP 200)
```

Claude Code's debug log showed a stream that received `message_start` but no
completed content block. A wire-shape probe confirmed that TokenSheep sometimes
returned two Anthropic streams concatenated together:

```text
message_start
ping
message_start
ping
content_block_start
...
message_stop
```

Direct CPA requests contained only one `message_start`. The first Cursor auth
had returned an empty response quickly, but the plugin had already committed
`message_start + ping` from an upstream heartbeat. A downstream retry then
appended a second response to the already-started HTTP 200 stream. Anthropic
clients correctly reject that sequence.

The plugin fix has two parts:

1. delay a heartbeat-only Anthropic preamble for a short grace period so fast
   empty responses remain retryable before any client bytes are committed;
2. if a preamble has already been committed, terminate a later empty response
   in-band with one legal Anthropic terminal sequence instead of closing the
   host bridge with a retryable error.

Regression coverage asserts that a fast heartbeat-plus-empty response emits no
client bytes and closes retryably, while an already-started heartbeat-only
stream terminates cleanly without a duplicate `message_start`.

## Cursor usage counter variants

Cursor currently exposes at least two `TurnEnded.output_tokens` shapes:

- independent output counter, often alongside cache counters larger than the
  generated output;
- cumulative counter where output includes cache-read and cache-write tokens.

One observed cumulative shape was:

```text
raw output = 1487
cache read = 294
cache write = 915
generated output = 1487 - 294 - 915 = 278
```

A blind subtraction is unsafe because valid independent-counter responses can
have cache-write counts in the tens of thousands. The plugin therefore records
a local estimate from the response text and subtracts cache counters only when:

- cache is a material portion of the raw output;
- the difference is compatible with the observed response size;
- the difference is substantially more plausible than the raw value.

The normalized count is shared by Anthropic, OpenAI Chat Completions, OpenAI
Responses, and Gemini usage renderers.

## Acceptance checklist

- sidecar health succeeds from the same network namespace CPA uses;
- live account catalog loads after sidecar readiness;
- Claude streaming has exactly one `message_start` and one `message_stop`;
- non-streaming returns a non-empty content block or an explicit non-200 error;
- tool calls contain a complete `tool_use` block and legal `stop_reason`;
- WebSearch contains matching server-tool-use and result blocks;
- thinking signatures are forwarded only when Cursor supplied a real signature;
- usage counters are mathematically consistent with response size and cache
  counters;
- TokenSheep and direct CPA both pass the same request matrix.

## v0.8.7 follow-up: dynamic tools, latest WebSearch, and long prefill

The post-v0.8.6 CCTest report still failed tool calling, WebSearch, and
protocol compliance. A real Claude Code coding turn also produced:

```text
No such tool available: glob
```

while the Claude Code dispatcher had registered `Glob`. A separate long-context
turn ended at approximately the plugin's 60-second first-output deadline with:

```text
upstream produced no content before first-output timeout
```

The follow-up fixes are:

1. resolve translated Cursor tool events through a request-scoped client tool
   contract, preserving the exact name/casing declared by the caller;
2. keep CLI-specific fallback spellings behind a single tool-contract module,
   rather than scattering Claude/OpenAI conditionals across protocol writers;
3. recognize Anthropic's current `web_search_20260318` server-tool version in
   addition to `web_search_20250305` and `web_search_20260209`;
4. treat Cursor heartbeat activity as renewal of the first-semantic-output idle
   timer, so long prompt prefill is not killed by an absolute 60-second timer;
5. emit Anthropic's standard `event:error` after a committed stream fails,
   instead of the invalid `stop_reason:"error"` or a synthetic success ending.

The primary-source protocol and CLI comparison is recorded in
`docs/cli-tool-and-anthropic-stream-contracts-2026-08-17.md`.

## v0.8.8 follow-up: preserve per-account egress through Chromium

The first v0.8.7 production checks succeeded until the Chromium sidecar was
restarted. After a clean restart, `AvailableModels` still worked but Claude
turns from the two eligible production accounts returned an empty upstream
response in about one second. CPA's scheduler log showed that both accounts
were configured with the same authenticated SOCKS5 route, while the sidecar
launched Chromium without any proxy configuration.

This exposed a transport-boundary bug: `ChromiumSidecarOption` correctly kept
Go's loopback request away from the account proxy, but discarded the proxy
instead of transferring it to Chromium. Cursor therefore saw the VPS egress
for chat even though CPA reported the account as using SOCKS5. Catalog access
alone was not a sufficient health signal because Cursor can return a catalog
and still silently suppress Claude generation on the wrong network identity.

Playwright/Chromium cannot authenticate directly to a username/password
SOCKS5 proxy (`Browser does not support socks5 proxy authentication`). v0.8.8
therefore uses this request-scoped route:

```text
CPA account proxy URL
  -> loopback-only sidecar header
  -> per-proxy local unauthenticated SOCKS5 bridge
  -> authenticated upstream SOCKS5
  -> per-request Chromium BrowserContext
  -> Cursor upstream
```

The routing header is removed before the Cursor request is issued. Proxy
bridges listen only on `127.0.0.1`, are reused by exact proxy URL within the
sidecar process, and are closed during sidecar shutdown. Accounts without a
proxy continue to use the default Chromium context. This keeps proxy choice an
account-level transport concern and avoids hard-coding one production proxy in
the sidecar or plugin.
