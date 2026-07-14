// Package sdk supervises a Node.js child process that runs
// @cursor/sdk on our behalf, and speaks the line-delimited JSON-RPC
// protocol defined in node-runner/src/protocol.ts.
//
// One Supervisor manages exactly one Node child. Agents are addressed
// by opaque agentId; runs by opaque runId. Both come from the child.
// Callers get typed Go structs — no JSON at the API surface — via
// the Supervisor methods.
//
// Design notes (see docs/sdk-integration.md § Architecture):
//
//   1. One Node process, N agents. Not one process per agent. Node
//      startup is ~200-500ms; per-agent spawn kills UX. @cursor/sdk
//      is designed for multi-agent use inside a single Node runtime.
//
//   2. Line-delimited JSON. Not Content-Length framing. Simpler on
//      both sides, plays well with Go's bufio.Scanner.
//
//   3. Requests correlate by numeric id. The Supervisor owns a
//      monotonic counter and a map[id]chan Response. Handlers of
//      the reader goroutine deliver responses onto those channels
//      and clean them up.
//
//   4. Notifications (Node → Go, no id) carry stream events. The
//      Supervisor demuxes them onto per-run event channels; the
//      HTTP layer in cmd/cursor-proxy subscribes to these channels
//      and re-emits as SSE.
//
//   5. Auth. CURSOR_API_KEY is set on the child's env at spawn —
//      never sent over stdin. The Supervisor never sees the key
//      value after spawn (it lives inside the Node process's env
//      table).
//
//   6. Crash recovery. If the child exits abnormally, all pending
//      requests get an error and all agent/run maps are cleared.
//      The Supervisor does NOT auto-restart — that's the caller's
//      choice; auto-restart with in-flight agents is a data-loss
//      trap (Cursor cloud agents survive, local agents don't).
//
// Not concurrency-safe against configuration mutation. Start() and
// Close() must be called once; concurrent Send / Cancel / Create /
// Close *of individual agents* are safe.
package sdk
