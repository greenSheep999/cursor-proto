// Bridges @cursor/sdk's SDKCustomTool.execute callback to the Go
// supervisor over stdio. When the SDK invokes a registered custom
// tool, we send a tool.execute JSON-RPC request out over stdout and
// return a Promise that resolves when either:
//   1. Go writes back an RpcResponse with the tool result (success
//      path — the HTTP client POSTed /tool_results).
//   2. Go writes a tool.result notification with an RpcError (fail
//      paths — client disconnect / timeout / run cancel), in which
//      case we synthesize a `{content:[{type:"text",text:msg}], isError:true}`
//      result so the SDK surfaces a tool_error to the model instead
//      of exploding the run.
//
// The bridge owns one pending map for outbound requests and a second
// keyed by callId (so tool.result notifications from Go can target
// a specific in-flight execute without needing the RPC id).

import type { Writable } from "node:stream";
import { writeMessage } from "./rpc.js";
import type { RpcError, RpcRequest } from "./protocol.js";
import {
  ERR_INTERNAL,
  ERR_TOOL_CALL_CANCELLED,
  ERR_TOOL_CALL_TIMEOUT,
} from "./protocol.js";

// SDK types — imported for the return shape only; we do not depend
// on runtime SDK code here, so this module stays unit-testable
// without instantiating a real Agent.
export type SDKCustomTool = {
  description?: string;
  inputSchema?: Record<string, unknown>;
  execute: (
    args: Record<string, unknown>,
    context: { toolCallId?: string },
  ) => Promise<SDKCustomToolResult>;
};

export type SDKCustomToolResult =
  | string
  | {
      content: Array<
        | { type: "text"; text: string }
        | { type: "image"; data: string; mimeType?: string }
      >;
      isError?: boolean;
      structuredContent?: Record<string, unknown>;
    };

/** Shape declared by the HTTP client on POST /runs/stream. */
export interface CustomToolDef {
  name: string;
  description?: string;
  inputSchema?: Record<string, unknown>;
  timeoutMs?: number;
}

/** Wire shape of a tool.execute request Node emits to Go. */
export interface ToolExecuteParams {
  runId: string;
  callId: string;
  name: string;
  args: Record<string, unknown>;
}

/** Wire shape of the tool.result notification Go emits to Node. */
export interface ToolResultNotifyParams {
  callId: string;
  error?: RpcError;
}

const DEFAULT_TIMEOUT_MS = 300_000;

type Pending = {
  resolve: (result: SDKCustomToolResult) => void;
  reject: (err: Error) => void;
  timer: NodeJS.Timeout | null;
  callId: string;
  runId: string;
};

export class ToolBridge {
  private nextId = 1;
  /** id → pending. Populated on outbound tool.execute request. */
  private byId: Map<number, Pending> = new Map();
  /** callId → pending. Same entries as byId; second index for the
   * tool.result notification path which does not carry the RPC id. */
  private byCallId: Map<string, Pending> = new Map();

  constructor(private readonly stdout: Writable) {}

  /**
   * Build a Record<string, SDKCustomTool> that the SDK will register
   * for a single run. Each execute() forwards to Go and awaits the
   * bridged reply.
   *
   * `runId` is captured in the closure so the SDK's stable Context
   * (`{toolCallId}`) is enough to correlate — the SDK generates
   * `toolCallId` per invocation and passes it into execute; we
   * forward it as `callId`.
   */
  buildCustomTools(
    runId: string,
    defs: CustomToolDef[],
  ): Record<string, SDKCustomTool> {
    const out: Record<string, SDKCustomTool> = {};
    for (const def of defs) {
      const timeoutMs = def.timeoutMs && def.timeoutMs > 0 ? def.timeoutMs : DEFAULT_TIMEOUT_MS;
      out[def.name] = {
        description: def.description,
        inputSchema: def.inputSchema,
        execute: async (args, ctx) => {
          // SDK's ctx.toolCallId is optional. Fall back to a locally
          // synthesized id so the byCallId map still has a key.
          // Locally synthesized ids are prefixed so they're
          // recognizable in logs; the HTTP client sees whichever
          // form we send.
          const callId = ctx.toolCallId ?? `local-${runId}-${this.nextId}`;
          return this.callExecute(runId, callId, def.name, args, timeoutMs);
        },
      };
    }
    return out;
  }

  /**
   * Issue one tool.execute request and await either its response or
   * a tool.result rejection. Exposed for tests; production callers
   * go through buildCustomTools.
   */
  callExecute(
    runId: string,
    callId: string,
    name: string,
    args: Record<string, unknown>,
    timeoutMs: number,
  ): Promise<SDKCustomToolResult> {
    return new Promise<SDKCustomToolResult>((resolve, reject) => {
      const id = this.nextId++;
      const timer = setTimeout(() => {
        this.settleError(id, {
          code: ERR_TOOL_CALL_TIMEOUT,
          message: `tool.execute timed out after ${timeoutMs}ms (callId=${callId})`,
        });
      }, timeoutMs);
      const pending: Pending = { resolve, reject, timer, callId, runId };
      this.byId.set(id, pending);
      this.byCallId.set(callId, pending);

      const req: RpcRequest<"tool.execute", ToolExecuteParams> = {
        jsonrpc: "2.0",
        id,
        method: "tool.execute",
        params: { runId, callId, name, args },
      };
      try {
        writeMessage(this.stdout, req);
      } catch (e) {
        // stdout is closed / parent dead; unwind immediately.
        this.settleError(id, {
          code: ERR_INTERNAL,
          message: `write tool.execute failed: ${(e as Error).message}`,
        });
      }
    });
  }

  /**
   * Called by the stdin dispatcher when Go replies to one of our
   * tool.execute requests. Success payloads resolve the SDK's
   * execute() promise; error payloads reject it so the SDK surfaces
   * a tool_error.
   */
  handleResponse(id: number, result: unknown | null, error: RpcError | null): void {
    const p = this.byId.get(id);
    if (!p) {
      // Late arrival — timer already fired.
      return;
    }
    this.clear(id, p);
    if (error) {
      p.resolve(errorAsResult(error));
      return;
    }
    p.resolve((result as SDKCustomToolResult | null) ?? "");
  }

  /**
   * Called by the stdin dispatcher when Go emits a tool.result
   * notification — used for out-of-band rejections (client
   * disconnect, run cancel, timeout at the Go layer). We map the
   * error to a tool_error result rather than throwing so the SDK
   * keeps the run alive.
   */
  handleToolResultNotify(params: ToolResultNotifyParams): void {
    const p = this.byCallId.get(params.callId);
    if (!p) return;
    // Find the id for this pending to clear both maps atomically.
    let idToClear: number | null = null;
    for (const [id, cand] of this.byId) {
      if (cand === p) {
        idToClear = id;
        break;
      }
    }
    if (idToClear === null) return;
    this.clear(idToClear, p);
    const err = params.error ?? {
      code: ERR_INTERNAL,
      message: "tool.result without error field",
    };
    p.resolve(errorAsResult(err));
  }

  /**
   * Reject every pending call for a given run. Called by
   * agent.close / run.cancel so the SDK doesn't hang forever on
   * dead tools.
   */
  rejectRun(runId: string, reason: string): void {
    // Iterate a snapshot — settleError() mutates the maps.
    for (const [id, p] of Array.from(this.byId.entries())) {
      if (p.runId !== runId) continue;
      this.clear(id, p);
      p.resolve(errorAsResult({ code: ERR_TOOL_CALL_CANCELLED, message: reason }));
    }
  }

  private settleError(id: number, err: RpcError): void {
    const p = this.byId.get(id);
    if (!p) return;
    this.clear(id, p);
    p.resolve(errorAsResult(err));
  }

  private clear(id: number, p: Pending): void {
    if (p.timer) clearTimeout(p.timer);
    this.byId.delete(id);
    this.byCallId.delete(p.callId);
  }
}

function errorAsResult(err: RpcError): SDKCustomToolResult {
  return {
    content: [{ type: "text", text: `[tool-bridge] error ${err.code}: ${err.message}` }],
    isError: true,
  };
}
