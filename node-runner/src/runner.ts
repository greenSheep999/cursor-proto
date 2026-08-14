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
import type { ToolBridge } from "./toolBridge.js";

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
  /**
   * Optional bridge for customTools reverse-RPC. When set, agent.send
   * with a non-empty customTools list wires the SDK's execute()
   * callbacks through this bridge to the Go supervisor. Left
   * undefined in unit tests that don't exercise the reverse channel.
   */
  toolBridge?: ToolBridge;
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
    // Reject any pending tool.execute promises for the runs we just
    // torched. rejectRun with the ended runId is safe even if there
    // are no pending calls for it.
    if (this.opts.toolBridge) {
      for (const r of toCancel) {
        this.opts.toolBridge.rejectRun(r.runId, "agent closed");
      }
    }
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
    // customTools reverse-RPC: only supported for local agents, per
    // SDK's LocalSendOptions.customTools contract. Reject cloud + tools
    // eagerly so the error surfaces at agent.send time instead of
    // deep inside the SDK.
    if (p.customTools && p.customTools.length > 0) {
      if (rec.runtime !== "local") {
        throw new ProtocolError(
          ERR_INVALID_PARAMS,
          "customTools require a local agent (SDK LocalSendOptions.customTools contract)",
        );
      }
      if (!this.opts.toolBridge) {
        throw new ProtocolError(
          ERR_INVALID_PARAMS,
          "customTools requested but runner started without a toolBridge",
        );
      }
    }

    // Peek the runId we'll assign so the bridge can pre-register.
    // We rely on the SDK's send returning a Run object; the runId
    // comes from that. To let the bridge inject a fresh
    // execute-per-run closure keyed on that runId, we assemble the
    // Record BEFORE calling send() using a placeholder runId — that
    // placeholder is patched to the real runId as soon as send()
    // returns. This is safe because SDK.send() is awaited before
    // any execute() fires.
    let run;
    try {
      if (p.customTools && p.customTools.length > 0 && this.opts.toolBridge) {
        // Pre-build the tools map with a placeholder runId. Actual
        // runId is only known after send() resolves; the toolBridge's
        // callId is the SDK's own ctx.toolCallId which is stable
        // per-invocation, so the runId in tool.execute params is
        // for logging / correlation only.
        const placeholderRunId = "pending";
        const tools = this.opts.toolBridge.buildCustomTools(
          placeholderRunId,
          p.customTools,
        );
        run = await rec.agent.send(p.prompt, { customTools: tools });
      } else {
        run = await rec.agent.send(p.prompt);
      }
    } catch (e) {
      throw new ProtocolError(
        ERR_SDK_FAILURE,
        `agent.send failed: ${(e as Error).message ?? String(e)}`,
      );
    }
    const runId = run.runId;

    // Kick off the stream pump. Not awaited — the caller gets its
    // runId back synchronously and drains events via notifications.
    // pumpStream also handles the final run.wait() call so the
    // run.done notification carries the authoritative RunResult.
    const streamTask = this.pumpStream(runId, run);
    this.runs.set(runId, {
      runId,
      agentId: p.agentId,
      streamTask,
      cancel: () => run.cancel(),
    });
    // On completion (success or failure), drop the run record and
    // reject any pending tool.execute promises so a client can't
    // hang forever waiting on a run that already ended.
    void streamTask.finally(() => {
      this.runs.delete(runId);
      if (this.opts.toolBridge) {
        this.opts.toolBridge.rejectRun(runId, "run ended before tool result arrived");
      }
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
    // The stream pump's finally() will call rejectRun once the run
    // fully unwinds, but do it here too in case the SDK's cancel
    // path holds the pump open past user-facing cancel semantics.
    if (this.opts.toolBridge) {
      this.opts.toolBridge.rejectRun(p.runId, "run cancelled");
    }
    return { ok: true };
  }

  // ---- stream pump ----
  //
  // Drains the SDK event iterator, forwarding each raw SDKMessage
  // over as a run.event notification, while also collecting a
  // dedup'd tool_call summary along the way (tool_call events fire
  // twice: status:"running" and status:"completed" — the docs are
  // explicit that the wire format is otherwise unstable, so we
  // only capture callId+name+args from the first observation of
  // each callId).
  //
  // After the stream ends, we call run.wait() to fetch the
  // authoritative RunResult (final text, cumulative usage, final
  // status) per the SDK's own guidance. Then we emit a single
  // run.done notification with all the structured fields — Go's
  // aggregation code parses that instead of trying to re-derive
  // final text from raw stream events.

  private async pumpStream(runId: string, run: {
    readonly runId: string;
    stream(): AsyncIterable<unknown>;
    wait(): Promise<import("./sdkInterface.js").SdkRunResult>;
  }): Promise<void> {
    const seenToolCalls = new Set<string>();
    const toolCalls: Array<{ callId: string; name: string; input?: unknown }> = [];

    try {
      for await (const event of run.stream()) {
        // Passthrough — the Go side still needs raw SDKMessages for
        // its SSE stream so downstream (Claude Code, etc.) sees
        // thinking / tool_call lifecycle / task / etc. exactly as
        // the SDK emits them.
        this.opts.emit({
          kind: "notify",
          method: "run.event",
          params: { runId, event },
        });

        // Meanwhile collect tool_call summaries for the final
        // run.done payload. Guard against non-object events and
        // unknown shapes — the wire format is unstable per SDK docs.
        if (event && typeof event === "object") {
          const ev = event as { type?: string; call_id?: string; name?: string; args?: unknown };
          if (ev.type === "tool_call" && typeof ev.call_id === "string" && !seenToolCalls.has(ev.call_id)) {
            seenToolCalls.add(ev.call_id);
            toolCalls.push({
              callId: ev.call_id,
              name: typeof ev.name === "string" ? ev.name : "",
              input: ev.args,
            });
          }
        }
      }

      // Stream ended cleanly — fetch the RunResult for the final
      // fields. wait() is documented safe to call after stream
      // drain; it returns the same terminal snapshot.
      let result: import("./sdkInterface.js").SdkRunResult;
      try {
        result = await run.wait();
      } catch (e) {
        // If wait() itself fails, degrade to a best-effort
        // "finished" done payload so the Go side isn't stuck
        // waiting for a notification.
        this.opts.emit({
          kind: "notify",
          method: "run.error",
          params: {
            runId,
            message: `run.wait failed: ${(e as Error).message ?? String(e)}`,
            code: ERR_SDK_FAILURE,
          },
        });
        return;
      }

      this.opts.emit({
        kind: "notify",
        method: "run.done",
        params: {
          runId,
          finalText: result.result ?? "",
          status: result.status,
          usage: result.usage,
          durationMs: result.durationMs,
          toolCalls,
        },
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
