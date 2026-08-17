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

## v0.8.9 follow-up: credential-safe route observability

After v0.8.8 deployment, a real authenticated SOCKS bridge probe succeeded,
but CPA Claude chat still returned an empty upstream response. Catalog success
does not prove that an individual chat request selected the same account route,
and logging the routing header would disclose proxy credentials.

The sidecar health response therefore exposes only aggregate, non-identifying
route state:

```json
{
  "direct_requests": 0,
  "proxied_requests": 1,
  "last_route": "socks-bridge",
  "proxy_bridge_count": 1
}
```

No proxy URL, host, port, username, password, account identifier, or derived
hash is included. Counters advance only after the request-scoped Chromium route
has been resolved. A plugin-level HTTP boundary test also verifies that an
account proxy survives `StorageJSON -> auth.Account -> executor.Client` and is
carried to the loopback sidecar request while Go's loopback transport remains
direct.

Production v0.8.9 evidence ruled out a missing proxy handoff. Before the manual
chat probe, health already showed only proxied traffic and an active SOCKS
bridge. One simple Claude request remained empty while increasing the proxied
counter for every retry and paired RPC.

## v0.8.10 follow-up: RPC-stage transport observability

To locate the empty response inside Cursor's paired chat protocol, health also
reports an `upstream_rpc` object keyed only by allowlisted RPC class:

- `available_models`;
- `run_sse`;
- `bidi_append`;
- `other` for any future method not yet classified.

Each entry contains request and failure counts plus the last HTTP status,
response byte count, and duration. It intentionally excludes response bodies,
headers, request IDs, URLs, account identity, and proxy details. This separates
a RunSSE stream that ends without semantic events from a BidiAppend seed that
Cursor acknowledges with HTTP 200 but ignores.

## v0.8.11 follow-up: preserve transport metadata across response close

The first production RPC counters reported `status=0` because Node emits the
downstream response `close` event after a normal `response.end()`. The existing
cancellation hook closed the page before `page.evaluate()` returned, causing
an already-forwarded response to be classified as a local failure.

The sidecar now records status when Chromium receives upstream headers and
increments bytes as chunks arrive, so those values survive a later page error.
It also treats a close after `writableFinished` as normal completion instead of
canceling the page. This is an observability/cancellation correction; it does
not synthesize or alter Cursor response bytes.

Production v0.8.11 then isolated the protocol symptom for a simple Claude turn:

```text
AvailableModels: HTTP 200, 205760 bytes
BidiAppend:      HTTP 200, 26 bytes
RunSSE:          HTTP 200, 389 bytes, no semantic output
```

## v0.8.12 follow-up: restore dual BidiAppend payload fields

The Cursor 3.16 schema still contains both `BidiAppendRequest.data` (field 1,
hex string) and `data_binary` (field 4, bytes). Before the 3.16 refresh this
client sent both fields with identical payloads. The upgrade removed `data`
without production evidence that the gateway had stopped consuming it.

Cursor can return a successful 26-byte BidiAppend acknowledgement while
silently ignoring a seed that contains only `data_binary`; the paired RunSSE
then closes with control frames but no content, tool call, or usage. v0.8.12
restores the dual-field wire shape: `data` is the lowercase hex encoding of the
same bytes carried in `data_binary`. A regression test decodes the real framed
HTTP request and asserts both representations.

The production differential did not change the response shape, so the missing
legacy field was not the cause of this incident. Keeping both fields restores
the previously verified compatibility shape, but account/trailer state remained
the deciding factor.

## v0.8.13 root cause: Cursor account billing/session state

A credential-safe event-summary probe decoded the 389-byte RunSSE body as two
frames:

```text
InteractionUpdate.heartbeat
gRPC trailer: status 8 (RESOURCE_EXHAUSTED)
```

The parsed Cursor error was `ERROR_RATE_LIMITED` with an explicit unpaid-team-
invoice message. An anonymous audit of all nine production Cursor auth records
found:

- six accounts blocked by an unpaid team invoice;
- two expired/not-logged-in sessions;
- one Free account restricted to Auto and unable to use a named Claude model.

Therefore the Chromium route, authenticated SOCKS bridge, entitlement catalog,
BidiAppend HTTP call, and RunSSE transport were all operating. The production
pool simply contained no account eligible to execute a named Claude request.

The plugin also had a separate error-propagation bug: all collectors skipped
events with `Server == nil` before inspecting trailer status. A parsed Cursor
error was discarded and later replaced by the generic empty-response error.
v0.8.13 checks non-OK trailers first in Anthropic/OpenAI, stream/non-stream
collectors. Uncommitted streams close with the actual retryable Cursor error;
committed Anthropic streams emit a legal `event:error`.

Restoring named Claude generation now requires either paying the affected team
invoice or importing a valid Pro/Team account whose billing and session are
current. Transport or translator changes cannot override that upstream account
policy.

### v0.8.13 production acceptance

The released plugin and sidecar were deployed to the shared CPA namespace and
verified through both public surfaces:

```text
direct CPA non-stream -> HTTP 500 with parsed unpaid-invoice trailer
direct CPA stream     -> HTTP 500 with parsed unpaid-invoice trailer
TokenSheep non-stream -> HTTP 500 with the same parsed trailer
TokenSheep stream     -> HTTP 500 with the same parsed trailer
```

TokenSheep's New API logs confirmed both public probes selected channel 16.
Neither request timed out, returned a malformed HTTP 200, or collapsed to the
generic empty-upstream message. Once a billing-current Claude-eligible account
is supplied, the same matrix must be rerun for successful content, tools, long
context, WebSearch, thinking signatures, and usage before model scoring begins.
