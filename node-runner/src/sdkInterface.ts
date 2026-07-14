// Minimal shape of @cursor/sdk we depend on. Kept as a hand-rolled
// interface (not `import type { Agent } from "@cursor/sdk"`) so that
//
//   1. Unit tests can inject a mock without a real @cursor/sdk install.
//   2. If the SDK's public types shift between beta releases, our
//      protocol stays fixed and we only touch the adapter in
//      src/realSdk.ts.
//
// See https://cursor.com/cn/docs/sdk/typescript for the SDK's own
// docs; the mapping to this interface is straightforward.

export interface SdkAgentOptions {
  apiKey: string;
  model: { id: string; params?: Array<{ id: string; value: string }> };
  local?: { cwd: string };
  cloud?: {
    repos?: Array<{ url: string; startingRef?: string }>;
    autoCreatePR?: boolean;
    envVars?: Record<string, string>;
  };
}

/**
 * One agent handle. `agentId` is stable for the agent's lifetime and
 * matches what `Agent.create()` in @cursor/sdk returns (`agent-<uuid>`
 * for local, `bc-<uuid>` for cloud).
 */
export interface SdkAgent {
  readonly agentId: string;

  /** Start a run. The returned handle produces the event stream. */
  send(prompt: string): Promise<SdkRun>;

  /** Free resources. Cancels in-flight runs. */
  close(): Promise<void>;
}

/**
 * @cursor/sdk's TokenUsage. See
 * https://cursor.com/cn/docs/sdk/typescript#token-usage.
 */
export interface SdkTokenUsage {
  inputTokens: number;
  outputTokens: number;
  cacheReadTokens: number;
  cacheWriteTokens: number;
  totalTokens: number;
  reasoningTokens?: number;
}

/**
 * @cursor/sdk's RunResult, minus fields we don't surface. The SDK's
 * own docs (SDK docs § "waiting in non-streaming mode") say
 * `result.result` is the authoritative final assistant text — no
 * more fishing through message/content/text ourselves.
 */
export interface SdkRunResult {
  status: "finished" | "error" | "cancelled";
  result?: string;
  usage?: SdkTokenUsage;
  durationMs?: number;
  error?: { message: string; code?: string };
}

/**
 * One in-flight run. Callers consume the async iterator to receive
 * SDK events (assistant deltas, tool calls, thinking, task status,
 * etc.). The iterator ends when the run reaches end_turn / cancel /
 * error. wait() then gives you the structured RunResult with the
 * final text + usage.
 */
export interface SdkRun {
  readonly runId: string;

  /** Ordered stream of SDKMessage-like objects. */
  stream(): AsyncIterable<unknown>;

  /** Wait for the run to reach a terminal state and return the result. */
  wait(): Promise<SdkRunResult>;

  /** Best-effort cancel. Idempotent. */
  cancel(): Promise<void>;
}

/**
 * Factory the runner uses to create agents. `realSdk.ts` provides the
 * @cursor/sdk-backed implementation; tests provide fakes.
 */
export interface SdkFactory {
  create(opts: SdkAgentOptions): Promise<SdkAgent>;
}
