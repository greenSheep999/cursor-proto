// Protocol types shared between the Go supervisor and this Node runner.
// One JSON object per line on stdin/stdout. See ../README.md for the
// framing rules and docs/sdk-integration.md in the repo root for the
// architectural rationale.
//
// Method inventory:
//
//   Request/response methods (Go → Node → Go):
//     ping                                                  → PingResult
//     agent.create(runtime, cwd, model, envVars?)           → { agentId }
//     agent.list()                                          → { agents: AgentSummary[] }
//     agent.status(agentId)                                 → AgentSummary
//     agent.close(agentId)                                  → { ok: true }
//     agent.send(agentId, prompt)                           → { runId }
//     run.cancel(runId)                                     → { ok: true }
//
//   Notifications (Node → Go, no id):
//     run.event  — one per SDK stream event
//     run.done   — end marker with final text + usage
//     run.error  — SDK / runtime error; caller aborts the stream

/**
 * JSON-RPC 2.0 envelope. We use `id: number` (auto-incremented on the
 * Go side); JSON-RPC also permits string/null but we don't.
 */
export interface RpcRequest<M extends string = string, P = unknown> {
  jsonrpc: "2.0";
  id: number;
  method: M;
  params?: P;
}

export interface RpcResponseOk<R = unknown> {
  jsonrpc: "2.0";
  id: number;
  result: R;
}

export interface RpcResponseErr {
  jsonrpc: "2.0";
  id: number;
  error: RpcError;
}

export type RpcResponse<R = unknown> = RpcResponseOk<R> | RpcResponseErr;

export interface RpcNotification<M extends string = string, P = unknown> {
  jsonrpc: "2.0";
  method: M;
  params: P;
}

export interface RpcError {
  code: number;
  message: string;
  data?: unknown;
}

// Standard JSON-RPC error codes we might emit.
export const ERR_PARSE_ERROR = -32700;
export const ERR_INVALID_REQUEST = -32600;
export const ERR_METHOD_NOT_FOUND = -32601;
export const ERR_INVALID_PARAMS = -32602;
export const ERR_INTERNAL = -32603;
// Application-specific range (-32000 to -32099 per spec).
export const ERR_NO_API_KEY = -32001;
export const ERR_AGENT_NOT_FOUND = -32002;
export const ERR_RUN_NOT_FOUND = -32003;
export const ERR_SDK_FAILURE = -32004;

// -------- payload shapes --------

export interface PingResult {
  pong: true;
  sdk_version: string;
  node_version: string;
  active_agents: number;
  active_runs: number;
}

/**
 * Parameters for agent.create. Mirror @cursor/sdk's Agent.create()
 * option shape (see https://cursor.com/cn/docs/sdk/typescript) but
 * flattened for JSON serialization.
 *
 * runtime = "local"  → runs the agent loop inline in this Node
 *                       process; cwd required.
 * runtime = "cloud"  → Cursor-hosted VM; cwd ignored; SDK clones
 *                       repos[] into the VM.
 */
export interface AgentCreateParams {
  runtime: "local" | "cloud";
  model: { id: string; params?: Array<{ id: string; value: string }> };

  // local-only
  cwd?: string;

  // cloud-only
  repos?: Array<{ url: string; startingRef?: string }>;
  autoCreatePR?: boolean;

  // shared
  envVars?: Record<string, string>;
}

export interface AgentCreateResult {
  agentId: string;
  createdAt: string; // ISO 8601
}

export interface AgentSummary {
  agentId: string;
  runtime: "local" | "cloud";
  model?: string;
  createdAt: string;
  activeRunIds: string[];
}

export interface AgentListResult {
  agents: AgentSummary[];
}

export interface AgentStatusParams {
  agentId: string;
}

export interface AgentCloseParams {
  agentId: string;
}

export interface AgentCloseResult {
  ok: true;
}

export interface AgentSendParams {
  agentId: string;
  // The prompt string. We accept a plain string here even though the
  // SDK also allows SDKUserMessage objects — the Go supervisor
  // constructs prompts from HTTP request bodies which are always
  // strings.
  prompt: string;
}

export interface AgentSendResult {
  runId: string;
}

export interface RunCancelParams {
  runId: string;
}

export interface RunCancelResult {
  ok: true;
}

// -------- notification payloads --------

/**
 * One SDK stream event, wrapped so the Go supervisor can multiplex
 * events from multiple concurrent runs onto its per-run channels.
 *
 * `event` is the raw SDKMessage from the SDK — its shape varies by
 * event.type ("assistant", "user", "tool_call", "thinking", "status",
 * "request", "task"). We pass it through opaque; the Go side and the
 * downstream HTTP consumer decide how to render it.
 */
export interface RunEventParams {
  runId: string;
  event: unknown;
}

/**
 * TokenUsage mirrors @cursor/sdk's TokenUsage type verbatim so the
 * Go side can pass this straight through without a translation
 * table. See https://cursor.com/cn/docs/sdk/typescript#token-usage.
 */
export interface TokenUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  totalTokens: number;
  reasoningTokens?: number;
}

/**
 * One tool call collected during a run. We keep only the fields
 * downstream needs — the SDK's ToolCall type is a large disjoint
 * union whose payloads the SDK docs explicitly mark as
 * "unstable, parse defensively." We surface name + input (which
 * IS stable) and drop the result blob into the SSE stream but not
 * this summary — clients that need per-tool results should read
 * the stream.
 */
export interface ToolCallSummary {
  callId: string;
  name: string;
  input?: unknown;
}

export interface RunDoneParams {
  runId: string;
  /**
   * Final assistant text from RunResult.result. Empty string when the
   * SDK didn't produce one (e.g. the run only made tool calls without
   * a wrap-up text).
   */
  finalText: string;
  /** RunResult.status verbatim: "finished" | "error" | "cancelled". */
  status: "finished" | "error" | "cancelled";
  /** Cumulative TokenUsage. Absent when the runtime didn't report any. */
  usage?: TokenUsage;
  /** Wall-clock duration of the run, from RunResult.durationMs. */
  durationMs?: number;
  /** Deduped tool_call summary observed during the stream. */
  toolCalls: ToolCallSummary[];
}

export interface RunErrorParams {
  runId: string;
  message: string;
  code?: number;
}
