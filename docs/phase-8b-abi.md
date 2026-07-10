# Phase 8b — CPA plugin executor ABI

This is the outcome of an archaeology dig into a local checkout of
[`router-for-me/CLIProxyAPI`](https://github.com/router-for-me/CLIProxyAPI)
(v7). It records what the plugin must marshal and unmarshal for the
three executor methods so future readers do not repeat the trip.

## Envelope (all methods)

Every plugin call returns a JSON envelope. From CPA's
`sdk/pluginabi/types.go`:

```go
type Envelope struct {
    OK     bool            `json:"ok"`
    Result json.RawMessage `json:"result,omitempty"`
    Error  *Error          `json:"error,omitempty"`
}

type Error struct {
    Code      string `json:"code"`
    Message   string `json:"message"`
    Retryable bool   `json:"retryable,omitempty"`
}
```

The plugin should always return `rc == 0` from `cliproxyPluginCall`
regardless of business-logic success or failure. The dynamic loader
(`internal/pluginhost/loader_unix.go`) treats a non-zero `rc` as an
unstructured plugin failure and discards the envelope — losing the
structured `Error.Code`. Structured errors are conveyed via `OK: false`
with a filled `Error`.

## `executor.execute` and `executor.execute_stream` — request

The host encodes an `rpcExecutorRequest` (private to
`internal/pluginhost/rpc_schema.go`). It embeds `pluginapi.ExecutorRequest`
and adds two thin RPC fields, so the wire JSON has fields from both:

Embedded fields from `pluginapi.ExecutorRequest`:
- `AuthID` — stable host auth identifier for the picked credential.
- `AuthProvider` — the auth provider key (`cursor` for us).
- `Model` — the requested model identifier.
- `Format` — the exit protocol the plugin negotiated (matches one of
  the strings in `executor_output_formats` from `plugin.register`).
- `Stream` — `true` for `executor.execute_stream`.
- `Alt` — an alternate route or mode suffix. Optional.
- `Headers` — HTTP-shaped headers passed to the executor
  (`map[string][]string`).
- `Query` — HTTP query parameters (`map[string][]string`).
- `OriginalRequest` — the raw client request bytes (source-format
  payload as posted).
- `SourceFormat` — the format the client originally used
  (`"openai"` / `"claude"` / etc.).
- `Payload` — the payload already translated into `Format`. This is
  what we parse.
- `Metadata` — extension bag for host↔plugin data. Sanitized to
  JSON-only types before being sent (`sanitizePluginMetadata`).
- `StorageJSON` — provider-owned auth blob (our `AuthFile`).
- `AuthMetadata` / `AuthAttributes` — host-managed metadata /
  routing attributes for the picked auth.
- `HTTPClient` — struck out on the wire (`json:"-"`). Plugins that
  need host HTTP go through `host.http.do` callbacks.

RPC-only fields (private, added by CPA host at send time):
- `stream_id` — bridge id the plugin should stream into for
  `executor.execute_stream`. Absent for `executor.execute`.
- `host_callback_id` — opaque handle scoping host callbacks
  (log, http.do, model.execute) to this call. Currently unused by
  the cursor plugin.

## `executor.execute` — response

The host expects `pluginapi.ExecutorResponse`:

```go
type ExecutorResponse struct {
    Payload  []byte
    Headers  http.Header
    Metadata map[string]any
}
```

`Payload` is a `[]byte` field so Go's `encoding/json` marshalls it as
a base64 string. That means our plugin can write the full response
body (an OpenAI chat.completion JSON blob or an Anthropic
`type: message` blob) directly as raw bytes. Content-Type is set via
`Headers`.

## `executor.execute_stream` — response

Two shapes are supported by
`internal/pluginhost/rpc_client.go:ExecuteStream`:

1. **Inline chunks (synchronous).** If the plugin returns
   `chunks: [...]` in the envelope, the host wraps them in a buffered
   channel and delivers them immediately, then closes the stream.
   Simple to implement but forces the plugin to buffer the entire
   stream before responding.
2. **Streaming via `stream_id` (asynchronous).** The plugin returns
   an empty `chunks` array (or omits it entirely) and, from a
   background goroutine, calls the host back with
   `host.stream.emit` for each chunk, then `host.stream.close` for
   the terminator. The synchronous return is only an acknowledgement
   containing the negotiated headers.

The wire types:

```go
// From internal/pluginhost/rpc_schema.go
type rpcExecutorStreamResponse struct {
    Headers http.Header                     `json:"headers,omitempty"`
    Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

type ExecutorStreamChunk struct {
    Payload []byte
    Err     error
}

// From internal/pluginhost/stream_bridge.go
type rpcStreamEmitRequest struct {
    StreamID string `json:"stream_id"`
    Payload  []byte `json:"payload,omitempty"`
    Error    string `json:"error,omitempty"`
}

type rpcStreamCloseRequest struct {
    StreamID string `json:"stream_id"`
    Error    string `json:"error,omitempty"`
}
```

The plugin uses the asynchronous shape. It returns synchronously with
only headers set, keeps `Chunks` empty, and immediately spawns a
goroutine that iterates `RunChat` events, encodes each as an SSE
frame in the negotiated `Format`, and calls `host.stream.emit`. On
turn_ended, the goroutine emits the final chunk (with usage) then
calls `host.stream.close`. On context cancel, it calls
`host.stream.close` with a non-empty error message so the host
propagates the failure.

## `executor.count_tokens` — request/response

Same request shape as `executor.execute` (embedded `ExecutorRequest`,
non-streaming). The response is `pluginapi.ExecutorResponse` again;
the `Payload` bytes should be a JSON body the host can pass through
unchanged. Convention across other CPA executors is either:

- OpenAI-flavoured: `{"object":"chat.completion","usage":{...}}`
- Anthropic-flavoured: `{"input_tokens":N}`
- Provider-native: `{"total_tokens":N}`

Cursor does not expose an isolated token-count endpoint, so this
plugin uses a local heuristic (`len(ascii)/4 + len(cjk)/1.5`, same
formula that cpa-context-guard uses) and returns the count in
OpenAI's non-streaming `usage.prompt_tokens` shape. The heuristic is
cheap and never issues a Cursor request.

## Host callbacks used by the plugin

The plugin only calls two host methods, both from
`sdk/pluginabi/types.go`:

- `host.stream.emit` — deliver one chunk. Payload is opaque bytes
  (typically one SSE frame).
- `host.stream.close` — terminator. An empty `error` field means
  clean end-of-stream; a non-empty string surfaces as an error on
  the host's `ExecutorStreamChunk.Err`.

Both are dispatched to the stored host C API via the standard
`cliproxy_host_call_fn` pointer captured in `cliproxy_plugin_init`.

## Concurrency / stream ID lifecycle

- One stream ID per `executor.execute_stream` call.
- Multiple `executor.execute_stream` calls run concurrently — the
  plugin's client cache is keyed by `AuthID` and the goroutines
  emit into disjoint stream IDs, so no cross-request state is
  shared beyond the `*executor.Client` (which is safe to reuse for
  parallel RunChat calls — Cursor derives per-request identifiers
  from `auth.GenerateRequestID`).
- If the plugin's goroutine exits without calling
  `host.stream.close`, the host waits on the channel until its own
  context expires. Always emit close on every exit path.
