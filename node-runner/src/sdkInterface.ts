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
 * One in-flight run. Callers consume the async iterator to receive
 * SDK events (assistant deltas, tool calls, thinking, task status,
 * etc.). The iterator ends when the run reaches end_turn / cancel /
 * error.
 */
export interface SdkRun {
  readonly runId: string;

  /** Ordered stream of SDKMessage-like objects. */
  stream(): AsyncIterable<unknown>;

  /** Best-effort cancel. Idempotent. */
  cancel(): Promise<void>;

  /** Optional: usage snapshot after end_turn. May be undefined. */
  usage?(): Promise<unknown | undefined>;
}

/**
 * Factory the runner uses to create agents. `realSdk.ts` provides the
 * @cursor/sdk-backed implementation; tests provide fakes.
 */
export interface SdkFactory {
  create(opts: SdkAgentOptions): Promise<SdkAgent>;
}
