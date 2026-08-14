// Entry point. Wires stdin → LineReader → parseLine → Runner
// dispatch → stdout serializer. The only novel logic here is the
// method dispatch table; everything else is threading.
//
// This file never speaks the SDK directly — it goes through the
// SdkFactory injected into Runner. That indirection is what lets
// unit tests exercise the full parse-dispatch-emit pipeline
// without a Cursor account or the real @cursor/sdk installed.

import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import { LineReader, err, notify, ok, parseAny, writeMessage } from "./rpc.js";
import type {
  AgentCloseParams,
  AgentCreateParams,
  AgentSendParams,
  AgentStatusParams,
  RpcRequest,
  RunCancelParams,
} from "./protocol.js";
import { ERR_INTERNAL, ERR_METHOD_NOT_FOUND } from "./protocol.js";
import { ProtocolError, Runner } from "./runner.js";
import { realSdkFactory } from "./realSdk.js";
import type { Emit } from "./runner.js";
import { ToolBridge } from "./toolBridge.js";
import type { ToolResultNotifyParams } from "./toolBridge.js";

// Read our own SDK version out of the installed @cursor/sdk
// package.json without imposing a build-time constant. The SDK's
// package.json is not in its exports map, so `require("@cursor/sdk/
// package.json")` fails with ERR_PACKAGE_PATH_NOT_EXPORTED. We
// fall back to resolving the SDK entry file and reading its
// sibling package.json directly.
function readSdkVersion(): string {
  try {
    const require = createRequire(import.meta.url);
    const entry = require.resolve("@cursor/sdk");
    // Walk up until we find a package.json (npm may nest under
    // exports paths). The SDK entry lives at
    // node_modules/@cursor/sdk/dist/index.js and its package.json
    // is one level above.
    let dir = dirname(entry);
    for (let i = 0; i < 5; i++) {
      const candidate = resolve(dir, "package.json");
      try {
        const raw = readFileSync(candidate, "utf8");
        const pkg = JSON.parse(raw) as { name?: string; version?: string };
        if (pkg.name === "@cursor/sdk" && pkg.version) {
          return pkg.version;
        }
      } catch {
        // keep walking
      }
      const parent = dirname(dir);
      if (parent === dir) break;
      dir = parent;
    }
    return "unknown";
  } catch {
    return "unknown";
  }
}

// Silence "declared but never used" for fileURLToPath (imported for
// completeness in case we need it later).
void fileURLToPath;

/**
 * Build the emit callback that runner uses. Delegates to
 * `writeMessage` on process.stdout so we serialize both
 * responses and notifications through the same framing.
 */
function makeEmit(): Emit {
  return (m) => {
    if (m.kind === "response") {
      writeMessage(process.stdout, ok(m.id, m.result));
    } else if (m.kind === "error") {
      writeMessage(process.stdout, err(m.id, { code: m.code, message: m.message, data: m.data }));
    } else {
      writeMessage(process.stdout, notify(m.method, m.params));
    }
  };
}

async function dispatch(runner: Runner, req: RpcRequest, emit: Emit): Promise<void> {
  try {
    switch (req.method) {
      case "ping": {
        emit({ kind: "response", id: req.id, result: runner.ping() });
        return;
      }
      case "agent.create": {
        const result = await runner.agentCreate(req.params as AgentCreateParams);
        emit({ kind: "response", id: req.id, result });
        return;
      }
      case "agent.list": {
        emit({ kind: "response", id: req.id, result: runner.agentList() });
        return;
      }
      case "agent.status": {
        const result = runner.agentStatus(req.params as AgentStatusParams);
        emit({ kind: "response", id: req.id, result });
        return;
      }
      case "agent.close": {
        const result = await runner.agentClose(req.params as AgentCloseParams);
        emit({ kind: "response", id: req.id, result });
        return;
      }
      case "agent.send": {
        const result = await runner.agentSend(req.params as AgentSendParams);
        emit({ kind: "response", id: req.id, result });
        return;
      }
      case "run.cancel": {
        const result = await runner.runCancel(req.params as RunCancelParams);
        emit({ kind: "response", id: req.id, result });
        return;
      }
      default: {
        emit({
          kind: "error",
          id: req.id,
          code: ERR_METHOD_NOT_FOUND,
          message: `method not found: ${req.method}`,
        });
        return;
      }
    }
  } catch (e) {
    if (e instanceof ProtocolError) {
      emit({ kind: "error", id: req.id, code: e.code, message: e.message });
    } else {
      const message = (e as Error).message ?? String(e);
      emit({ kind: "error", id: req.id, code: ERR_INTERNAL, message });
      // Log to stderr for the Go side's [node] channel. Never
      // stdout — stdout is reserved for framed JSON only.
      // Stderr is also where a panic trace would land.
      // eslint-disable-next-line no-console
      console.error(`internal error in ${req.method} (id=${req.id}):`, e);
    }
  }
}

async function main(): Promise<void> {
  // toolBridge owns outbound tool.execute requests + their pending
  // map. Wired here (not in Runner) because it writes directly to
  // stdout, mirroring the emit() serializer's contract.
  const toolBridge = new ToolBridge(process.stdout);

  const runner = new Runner({
    apiKey: process.env.CURSOR_API_KEY,
    factory: realSdkFactory,
    sdkVersion: readSdkVersion(),
    emit: makeEmit(),
    toolBridge,
  });

  const reader = new LineReader();
  const emit = makeEmit();

  process.stdin.on("data", (chunk: Buffer) => {
    for (const line of reader.push(chunk)) {
      const parsed = parseAny(line);
      switch (parsed.kind) {
        case "parse_error": {
          // If the request had a valid id, use it; otherwise 0 is a
          // reasonable "we couldn't tell who this was for" marker.
          // The Go side treats id=0 responses as unsolicited errors
          // and logs them without matching to a pending call.
          const id = parsed.id ?? 0;
          emit({
            kind: "error",
            id,
            code: parsed.error.code,
            message: parsed.error.message,
            data: parsed.error.data,
          });
          break;
        }
        case "request": {
          // Dispatch asynchronously — but do NOT parallelize different
          // requests, because agent.send / agent.create can race on
          // the runner's state maps if we let them. Serial dispatch
          // is what index.ts guarantees.
          void dispatch(runner, parsed.req, emit);
          break;
        }
        case "response": {
          // Reply to a Node → Go request. Today the only outbound
          // requests are tool.execute; toolBridge owns them and
          // will resolve the matching pending promise.
          const r = parsed.resp;
          if ("result" in r) {
            toolBridge.handleResponse(r.id, r.result, null);
          } else {
            toolBridge.handleResponse(r.id, null, r.error);
          }
          break;
        }
        case "notification": {
          // Only tool.result is defined for the Go → Node direction
          // today. Anything else logs; we don't want an unknown
          // notification to crash the runner.
          if (parsed.note.method === "tool.result") {
            toolBridge.handleToolResultNotify(
              parsed.note.params as ToolResultNotifyParams,
            );
          } else {
            // eslint-disable-next-line no-console
            console.error(`unhandled notification method=${parsed.note.method}`);
          }
          break;
        }
      }
    }
  });

  process.stdin.on("end", () => {
    // Parent closed stdin => shut down cleanly. In practice the Go
    // supervisor kills us with SIGTERM first; this is the fallback.
    process.exit(0);
  });

  process.on("SIGTERM", () => {
    // Graceful stop: rely on Node's default which will run any
    // remaining microtasks. We don't try to close agents here —
    // in-flight runs would need per-agent close() awaits that can
    // themselves hang; SIGTERM should be a fast path.
    process.exit(0);
  });
}

// eslint-disable-next-line @typescript-eslint/no-floating-promises
main();
