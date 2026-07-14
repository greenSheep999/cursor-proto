// Runner: dispatches JSON-RPC requests to @cursor/sdk operations and
// pumps SDK stream events back out as notifications. All method
// handlers live here; index.ts is just the stdin/stdout wiring.
//
// State model:
//
//   - `agents` keeps every live SdkAgent by agentId.
//   - `runs` keeps every in-flight run by runId, back-referenced to
//     the agentId that owns it (needed for agent.close cascade).
//
// Concurrency: Node is single-threaded so there is no lock. We assume
// requests arrive strictly serialized (one JSON per line, parsed
// sequentially in index.ts). Long-running work (SDK network I/O,
// stream pumps) uses async/await; the runner returns to the event
// loop between messages.

import type {
  AgentCloseParams,
  AgentCloseResult,
  AgentCreateParams,
  AgentCreateResult,
  AgentListResult,
  AgentSendParams,
  AgentSendResult,
  AgentStatusParams,
  AgentSummary,
  PingResult,
  RunCancelParams,
  RunCancelResult,
} from "./protocol.js";
import {
  ERR_AGENT_NOT_FOUND,
  ERR_INVALID_PARAMS,
  ERR_NO_API_KEY,
  ERR_RUN_NOT_FOUND,
  ERR_SDK_FAILURE,
} from "./protocol.js";
import type { SdkAgent, SdkFactory } from "./sdkInterface.js";

/** One line-serialized message emitted for the parent to write out. */
export type Emit = (
  msg:
    | { kind: "response"; id: number; result: unknown }
    | { kind: "error"; id: number; code: number; message: string; data?: unknown }
    | { kind: "notify"; method: string; params: unknown },
) => void;

interface RunRec {
  runId: string;
  agentId: string;
  streamTask: Promise<void>; // resolves when the stream pump finishes
  cancel: () => Promise<void>;
}

/**
 * Thrown by handlers to indicate a protocol-level error we want to
 * surface as a JSON-RPC error response. Non-`ProtocolError` throws
 * become ERR_SDK_FAILURE — the SDK misbehaved, not the caller.
 */
export class ProtocolError extends Error {
  constructor(public code: number, message: string) {
    super(message);
  }
}

export interface RunnerOptions {
  apiKey: string | undefined;
  factory: SdkFactory;
  sdkVersion: string; // reported via ping
  emit: Emit;
}

export class Runner {
  private agents = new Map<string, { agent: SdkAgent; createdAt: string; runtime: "local" | "cloud"; modelId?: string }>();
  private runs = new Map<string, RunRec>();

  constructor(private readonly opts: RunnerOptions) {}

  // ---- ping ----

  ping(): PingResult {
    return {
      pong: true,
      sdk_version: this.opts.sdkVersion,
      node_version: process.versions.node,
      active_agents: this.agents.size,
      active_runs: this.runs.size,
    };
  }

  // ---- agent lifecycle ----

  async agentCreate(p: AgentCreateParams): Promise<AgentCreateResult> {
    if (!this.opts.apiKey) {
      throw new ProtocolError(
        ERR_NO_API_KEY,
        "CURSOR_API_KEY not set in the runner's environment; agent mode disabled",
      );
    }
    if (p.runtime !== "local" && p.runtime !== "cloud") {
      throw new ProtocolError(ERR_INVALID_PARAMS, `runtime must be 'local' or 'cloud', got ${p.runtime}`);
    }
    if (p.runtime === "local" && (!p.cwd || p.cwd.length === 0)) {
      throw new ProtocolError(ERR_INVALID_PARAMS, "local runtime requires a non-empty cwd");
    }
    if (p.runtime === "cloud" && (!p.repos || p.repos.length === 0)) {
      throw new ProtocolError(ERR_INVALID_PARAMS, "cloud runtime requires at least one repo in repos[]");
    }
    if (!p.model || typeof p.model.id !== "string" || p.model.id.length === 0) {
      throw new ProtocolError(ERR_INVALID_PARAMS, "model.id is required");
    }

    let agent: SdkAgent;
    try {
      agent = await this.opts.factory.create({
        apiKey: this.opts.apiKey,
        model: p.model,
        local: p.runtime === "local" ? { cwd: p.cwd! } : undefined,
        cloud:
          p.runtime === "cloud"
            ? {
                repos: p.repos,
                autoCreatePR: p.autoCreatePR,
                envVars: p.envVars,
              }
            : undefined,
      });
    } catch (e) {
      throw new ProtocolError(
        ERR_SDK_FAILURE,
        `agent.create failed: ${(e as Error).message ?? String(e)}`,
      );
    }

    const createdAt = new Date().toISOString();
    this.agents.set(agent.agentId, {
      agent,
      createdAt,
      runtime: p.runtime,
      modelId: p.model.id,
    });
    return { agentId: agent.agentId, createdAt };
  }

  agentList(): AgentListResult {
    const agents: AgentSummary[] = [];
    for (const [agentId, rec] of this.agents) {
      const activeRunIds: string[] = [];
      for (const [runId, run] of this.runs) {
        if (run.agentId === agentId) activeRunIds.push(runId);
      }
      agents.push({
        agentId,
        runtime: rec.runtime,
        model: rec.modelId,
        createdAt: rec.createdAt,
        activeRunIds,
      });
    }
    return { agents };
  }

  agentStatus(p: AgentStatusParams): AgentSummary {
    const rec = this.agents.get(p.agentId);
    if (!rec) {
      throw new ProtocolError(ERR_AGENT_NOT_FOUND, `agent ${p.agentId} not found`);
    }
    const activeRunIds: string[] = [];
    for (const [runId, run] of this.runs) {
      if (run.agentId === p.agentId) activeRunIds.push(runId);
    }
    return {
      agentId: p.agentId,
      runtime: rec.runtime,
      model: rec.modelId,
      createdAt: rec.createdAt,
      activeRunIds,
    };
  }

  async agentClose(p: AgentCloseParams): Promise<AgentCloseResult> {
    const rec = this.agents.get(p.agentId);
    if (!rec) {
      throw new ProtocolError(ERR_AGENT_NOT_FOUND, `agent ${p.agentId} not found`);
    }
    // Cancel all in-flight runs owned by this agent, then close.
    const toCancel: RunRec[] = [];
    for (const run of this.runs.values()) {
      if (run.agentId === p.agentId) toCancel.push(run);
    }
    for (const r of toCancel) {
      try {
        await r.cancel();
      } catch {
        // Cancel is best-effort; drop errors and continue.
      }
      this.runs.delete(r.runId);
    }
    try {
      await rec.agent.close();
    } catch (e) {
      // Close failures are logged but not fatal — the caller
      // asked us to release, we drop our tracking either way.
    }
    this.agents.delete(p.agentId);
    return { ok: true };
  }

  // ---- run lifecycle ----

  async agentSend(p: AgentSendParams): Promise<AgentSendResult> {
    const rec = this.agents.get(p.agentId);
    if (!rec) {
      throw new ProtocolError(ERR_AGENT_NOT_FOUND, `agent ${p.agentId} not found`);
    }
    if (typeof p.prompt !== "string") {
      throw new ProtocolError(ERR_INVALID_PARAMS, "prompt must be a string");
    }
    let run;
    try {
      run = await rec.agent.send(p.prompt);
    } catch (e) {
      throw new ProtocolError(
        ERR_SDK_FAILURE,
        `agent.send failed: ${(e as Error).message ?? String(e)}`,
      );
    }
    const runId = run.runId;

    // Kick off the stream pump. Not awaited — the caller gets its
    // runId back synchronously and drains events via notifications.
    const streamTask = this.pumpStream(runId, run.stream());
    this.runs.set(runId, {
      runId,
      agentId: p.agentId,
      streamTask,
      cancel: () => run.cancel(),
    });
    // On completion (success or failure), drop the run record. We
    // don't await because callers shouldn't block on stream drain.
    void streamTask.finally(() => {
      this.runs.delete(runId);
    });
    return { runId };
  }

  async runCancel(p: RunCancelParams): Promise<RunCancelResult> {
    const rec = this.runs.get(p.runId);
    if (!rec) {
      // Idempotent — return ok even if already gone, so the Go side
      // doesn't have to distinguish "already ended" from "unknown".
      return { ok: true };
    }
    try {
      await rec.cancel();
    } catch {
      // Same reasoning: cancel-then-race with natural end is fine.
    }
    return { ok: true };
  }

  // ---- stream pump ----

  private async pumpStream(runId: string, stream: AsyncIterable<unknown>): Promise<void> {
    try {
      for await (const event of stream) {
        this.opts.emit({
          kind: "notify",
          method: "run.event",
          params: { runId, event },
        });
      }
      this.opts.emit({
        kind: "notify",
        method: "run.done",
        params: { runId },
      });
    } catch (e) {
      const message = (e as Error).message ?? String(e);
      this.opts.emit({
        kind: "notify",
        method: "run.error",
        params: { runId, message, code: ERR_SDK_FAILURE },
      });
    }
  }
}
