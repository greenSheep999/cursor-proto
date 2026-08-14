package sdk

// Protocol types mirror node-runner/src/protocol.ts. Keep these in
// sync when the Node side gains new methods; a mismatch surfaces as
// -32601 (method not found) or a JSON unmarshal error at runtime.

import (
	"encoding/json"
)

// JSON-RPC error codes we recognize. Duplicated from the Node side
// because Go tests can't import TypeScript. The comment on each
// constant states the Node source line that defines it, so drift is
// easy to spot in a diff review.
const (
	ErrParseError     = -32700 // protocol.ts ERR_PARSE_ERROR
	ErrInvalidRequest = -32600 // protocol.ts ERR_INVALID_REQUEST
	ErrMethodNotFound = -32601 // protocol.ts ERR_METHOD_NOT_FOUND
	ErrInvalidParams  = -32602 // protocol.ts ERR_INVALID_PARAMS
	ErrInternal       = -32603 // protocol.ts ERR_INTERNAL
	ErrNoAPIKey       = -32001 // protocol.ts ERR_NO_API_KEY
	ErrAgentNotFound  = -32002 // protocol.ts ERR_AGENT_NOT_FOUND
	ErrRunNotFound    = -32003 // protocol.ts ERR_RUN_NOT_FOUND
	ErrSDKFailure     = -32004 // protocol.ts ERR_SDK_FAILURE
	// customTools reverse-RPC error codes. Sent by the Go supervisor
	// (via tool.result notifications) or the HTTP layer when a tool
	// call can't complete normally. See docs/sdk-integration.md
	// §"Custom tools" and protocol.ts for the same constants.
	ErrToolClientDisconnected = -32005 // protocol.ts ERR_TOOL_CLIENT_DISCONNECTED
	ErrToolCallTimeout        = -32006 // protocol.ts ERR_TOOL_CALL_TIMEOUT
	ErrToolCallCancelled      = -32007 // protocol.ts ERR_TOOL_CALL_CANCELLED
	ErrToolCallExpired        = -32008 // protocol.ts ERR_TOOL_CALL_EXPIRED (HTTP layer only)
)

// rpcRequest is what we send to the Node child. Fields are exported
// so encoding/json can see them; the JSON keys stay lower-case to
// match the protocol.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"` // always "2.0"
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// rpcResponse is one line off the child's stdout. Exactly one of
// Result or Error is populated on a well-formed reply. Notifications
// (Method != "" and ID == 0) are separately routed.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method,omitempty"` // for notifications
	Params  json.RawMessage `json:"params,omitempty"` // for notifications
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is the typed shape of an error response. Exposed publicly
// so callers can type-assert against ErrKind and act on the code
// (e.g. distinguish ErrNoAPIKey from ErrSDKFailure for retry logic).
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return "<nil rpc error>"
	}
	return e.Message
}

// -------- request/response payloads --------

// PingResult mirrors PingResult in protocol.ts.
type PingResult struct {
	Pong         bool   `json:"pong"`
	SDKVersion   string `json:"sdk_version"`
	NodeVersion  string `json:"node_version"`
	ActiveAgents int    `json:"active_agents"`
	ActiveRuns   int    `json:"active_runs"`
}

// ModelSelection is the shape the Node runner expects for
// AgentCreateParams.model. Params is optional (per-model tuning like
// reasoning effort); leave it nil when not needed.
type ModelSelection struct {
	ID     string       `json:"id"`
	Params []ModelParam `json:"params,omitempty"`
}

type ModelParam struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// AgentCreateParams mirrors AgentCreateParams in protocol.ts.
// Runtime is "local" or "cloud"; the mutually-exclusive local/cloud
// fields make invalid states unrepresentable in the request struct.
type AgentCreateParams struct {
	Runtime string         `json:"runtime"`
	Model   ModelSelection `json:"model"`

	// local-only
	CWD string `json:"cwd,omitempty"`

	// cloud-only
	Repos        []CloudRepo `json:"repos,omitempty"`
	AutoCreatePR bool        `json:"autoCreatePR,omitempty"`

	// shared
	EnvVars map[string]string `json:"envVars,omitempty"`
}

type CloudRepo struct {
	URL         string `json:"url"`
	StartingRef string `json:"startingRef,omitempty"`
}

type AgentCreateResult struct {
	AgentID   string `json:"agentId"`
	CreatedAt string `json:"createdAt"` // ISO 8601
}

type AgentSummary struct {
	AgentID      string   `json:"agentId"`
	Runtime      string   `json:"runtime"`
	Model        string   `json:"model,omitempty"`
	CreatedAt    string   `json:"createdAt"`
	ActiveRunIDs []string `json:"activeRunIds"`
}

type AgentListResult struct {
	Agents []AgentSummary `json:"agents"`
}

type AgentSendParams struct {
	AgentID string `json:"agentId"`
	Prompt  string `json:"prompt"`
	// CustomTools declares HTTP-client-hosted tools the Node runner
	// should expose to the SDK for this run. Each entry carries only
	// schema — the actual execute() callback synthesized in Node
	// bounces back over stdio (tool.execute request) so the HTTP
	// client can service it via SSE + POST /tool_results. Empty means
	// no custom tools; SDK's own built-ins still apply. Local
	// runtime only (SDK rejects it on cloud agents).
	CustomTools []CustomToolDef `json:"customTools,omitempty"`
}

// CustomToolDef mirrors the CustomToolDef in protocol.ts. Passed
// through Node → SDK.LocalSendOptions.customTools[name].
// TimeoutMs is enforced Go-side inside handleInboundRequest;
// zero uses the supervisor's default (300 000 ms).
type CustomToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
	TimeoutMs   int64           `json:"timeoutMs,omitempty"`
}

type AgentSendResult struct {
	RunID string `json:"runId"`
}

// -------- notification payloads (child → parent) --------

// RunEvent wraps one SDK stream event so the Go supervisor can route
// it to the run's per-run channel. Event is left as RawMessage so
// consumers can decide how to render it (SSE, JSON, whatever) without
// this package having to model every SDK event shape.
type RunEvent struct {
	RunID string          `json:"runId"`
	Event json.RawMessage `json:"event"`
}

// RunDone is the terminal notification for a run. Payload mirrors
// what the Node runner assembles from the SDK's RunResult (per
// https://cursor.com/cn/docs/sdk/typescript#waiting-in-non-streaming-mode)
// plus a dedup'd list of tool_call summaries collected during the
// stream. Downstream should treat this as authoritative and not
// try to reconstruct final_text / usage from raw run.event
// notifications.
type RunDone struct {
	RunID      string            `json:"runId"`
	FinalText  string            `json:"finalText"`
	Status     string            `json:"status"` // "finished" | "error" | "cancelled"
	Usage      *TokenUsage       `json:"usage,omitempty"`
	DurationMs int64             `json:"durationMs,omitempty"`
	ToolCalls  []ToolCallSummary `json:"toolCalls,omitempty"`
}

// TokenUsage mirrors @cursor/sdk's TokenUsage. See
// https://cursor.com/cn/docs/sdk/typescript#token-usage.
type TokenUsage struct {
	InputTokens      int64 `json:"inputTokens"`
	OutputTokens     int64 `json:"outputTokens"`
	CacheReadTokens  int64 `json:"cacheReadTokens"`
	CacheWriteTokens int64 `json:"cacheWriteTokens"`
	TotalTokens      int64 `json:"totalTokens"`
	ReasoningTokens  int64 `json:"reasoningTokens,omitempty"`
}

// ToolCallSummary is one deduped tool call observed during a run.
// The SDK docs mark args/result payloads as unstable so we only
// keep name + input (which IS stable) and callId for correlation.
type ToolCallSummary struct {
	CallID string          `json:"callId"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input,omitempty"`
}

type RunError struct {
	RunID   string `json:"runId"`
	Message string `json:"message"`
	Code    int    `json:"code,omitempty"`
}

// -------- customTools reverse-RPC (stage 2/3) --------

// ToolExecuteRequest is the inbound request Node emits when the SDK
// invokes an HTTP-registered custom tool. Response is a
// ToolExecuteResult (Result field) or an RPCError.
type ToolExecuteRequest struct {
	RunID  string          `json:"runId"`
	CallID string          `json:"callId"`
	Name   string          `json:"name"`
	Args   json.RawMessage `json:"args"`
}

// ToolExecuteResult mirrors SDKCustomToolResult in options.d.ts, but
// serialized as JSON. The Node runner unpacks this back into the
// SDK's execute() promise value. Callers should populate at most one
// of Content / Text / StructuredContent — string or content array
// are the SDK's two documented result shapes. IsError=true asks the
// SDK to treat the result as a tool error (surfaced to the model as
// an error message instead of a normal tool_result content block).
type ToolExecuteResult struct {
	Content           []ToolExecuteContent `json:"content,omitempty"`
	Text              string               `json:"text,omitempty"`
	IsError           bool                 `json:"isError,omitempty"`
	StructuredContent json.RawMessage      `json:"structuredContent,omitempty"`
}

// ToolExecuteContent mirrors the SDKCustomToolContent union in
// options.d.ts. Image payloads carry base64-encoded bytes in Data.
type ToolExecuteContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// ToolResultNotify is the Go → Node notification used when the Go
// side needs to force-resolve a pending tool.execute WITHOUT waiting
// for the client's POST /tool_results — typically because the HTTP
// client disconnected, the per-call timeout fired, or the run was
// cancelled. Node's toolBridge rejects the pending execute() promise
// with the given error code. If both a stdio response and a
// tool.result notification arrive for the same callId (a race),
// Node treats the first as authoritative and drops the second.
type ToolResultNotify struct {
	CallID string    `json:"callId"`
	Error  *RPCError `json:"error,omitempty"`
	// Result is deliberately omitted: tool.result is only used for
	// error paths. Success paths always ride the tool.execute
	// rpcResponse the HTTP handler synthesizes.
}
