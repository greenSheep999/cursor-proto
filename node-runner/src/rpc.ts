// Line-delimited JSON-RPC framing on stdio.
//
// Wire rule: exactly one JSON object per line, `\n`-terminated.
// No Content-Length header, no framing bytes, no batching. This
// matches how the Go supervisor emits requests (bufio.Writer with
// explicit newline) and how it parses replies (bufio.Scanner with
// default SplitLines).
//
// stdin is treated as authoritative — no length or checksum
// validation beyond JSON.parse; the sender is the parent Go
// process which we trust by construction. stderr is left free
// for panic traces and Node warnings; the Go side prefixes it
// with [node] in its log stream.

import { Writable } from "node:stream";
import type {
  RpcError,
  RpcNotification,
  RpcRequest,
  RpcResponse,
  RpcResponseErr,
  RpcResponseOk,
} from "./protocol.js";

// A parsed inbound frame. Stdin used to carry only requests (Go → Node),
// so `parseLine` returned `request | parse_error`. With customTools we
// added a reverse channel: Node makes tool.execute requests to Go, and
// Go replies with an RpcResponse. Go also sends tool.result
// notifications when it can't wait for a stdio response (client
// disconnect / timeout). `parseAny` covers all four inbound shapes so
// the stdin dispatcher in index.ts can route each to the right handler.
export type ParseAnyResult =
  | { kind: "request"; req: RpcRequest }
  | { kind: "response"; resp: RpcResponse }
  | { kind: "notification"; note: RpcNotification }
  | { kind: "parse_error"; error: RpcError; id: number | null };

// -------- reader: parse a Readable line-by-line into RpcRequests --------

/**
 * Buffered line reader. Given raw stdin chunks, emits one Buffer per
 * line (without the trailing `\n`). Uses a rolling Buffer so we
 * survive chunks that split a JSON object across boundaries.
 */
export class LineReader {
  static readonly MAX_FRAME_BYTES = 4 * 1024 * 1024;
  private buf: Buffer = Buffer.alloc(0);
  private discardingOversized = false;

  /** Push one chunk (arbitrary length). Returns 0-or-more complete lines. */
  push(chunk: Buffer): string[] {
    if (this.discardingOversized) {
      const nl = chunk.indexOf(0x0a);
      if (nl === -1) return [];
      this.discardingOversized = false;
      chunk = chunk.subarray(nl + 1);
    }
    this.buf = Buffer.concat([this.buf, chunk]);
    const lines: string[] = [];
    let start = 0;
    while (true) {
      const nl = this.buf.indexOf(0x0a /* \n */, start);
      if (nl === -1) {
        if (this.buf.length > LineReader.MAX_FRAME_BYTES) {
          // Drop an oversized frame without retaining attacker-controlled
          // bytes until its newline arrives. The peer gets no response for
          // this malformed frame, but the runner remains available.
          this.buf = Buffer.alloc(0);
          this.discardingOversized = true;
        }
        break;
      }
      const line = this.buf.subarray(start, nl).toString("utf8");
      if (line.length > 0) {
        if (Buffer.byteLength(line, "utf8") <= LineReader.MAX_FRAME_BYTES) {
          lines.push(line);
        }
      }
      start = nl + 1;
    }
    if (start > 0) {
      this.buf = this.buf.subarray(start);
    }
    return lines;
  }

  /** Whatever bytes are still buffered (unterminated). Callers rarely need this. */
  pending(): Buffer {
    return this.buf;
  }
}

/**
 * Parse one line into a JSON-RPC request. Returns either the
 * request or a "please respond with this error" descriptor. We
 * choose this over throwing because a malformed line from stdin
 * shouldn't kill the process — we reply with a parse error and
 * keep serving.
 */
export type ParseResult =
  | { kind: "request"; req: RpcRequest }
  | { kind: "parse_error"; error: RpcError; id: number | null };

export function parseLine(line: string): ParseResult {
  let obj: unknown;
  try {
    obj = JSON.parse(line);
  } catch (e) {
    return {
      kind: "parse_error",
      error: {
        code: -32700, // ERR_PARSE_ERROR
        message: `parse error: ${(e as Error).message}`,
        data: { line: line.slice(0, 200) },
      },
      id: null,
    };
  }
  if (
    typeof obj !== "object" ||
    obj === null ||
    (obj as { jsonrpc?: unknown }).jsonrpc !== "2.0" ||
    typeof (obj as { method?: unknown }).method !== "string" ||
    typeof (obj as { id?: unknown }).id !== "number"
  ) {
    const idRaw = (obj as { id?: unknown } | null)?.id;
    return {
      kind: "parse_error",
      error: {
        code: -32600, // ERR_INVALID_REQUEST
        message: "not a valid JSON-RPC 2.0 request (require jsonrpc:'2.0', method:string, id:number)",
      },
      id: typeof idRaw === "number" ? idRaw : null,
    };
  }
  return { kind: "request", req: obj as RpcRequest };
}

// -------- writer: serialize responses / notifications to a Writable --------

/**
 * Serializes one JSON value + `\n` and writes it. Blocks the Node
 * event loop only long enough to JSON.stringify. Errors from the
 * underlying stream (parent died mid-write, e.g.) bubble up and are
 * fatal — the runner should die when its parent dies.
 *
 * The `RpcRequest` case (Node → Go) is used by toolBridge for the
 * customTools reverse channel; it was added when we extended stdio
 * from unidirectional to duplex. See protocol.ts §"Method inventory".
 */
export function writeMessage(
  stream: Writable,
  msg: RpcRequest | RpcResponse | RpcNotification,
): void {
  stream.write(JSON.stringify(msg) + "\n");
}

/**
 * Broader parser used by index.ts's stdin dispatcher: distinguishes
 * request / response / notification purely from the frame shape, per
 * JSON-RPC 2.0 §4-6. Rules:
 *   - method:string + id:number       → request
 *   - method:string, no id            → notification
 *   - id:number + (result | error)    → response
 * Anything else is a parse error.
 *
 * We deliberately do not fall back to `parseLine`: the old parser
 * assumed inbound was always a request, which was true before we
 * added the tool.execute reverse channel. Callers that only want
 * to accept inbound requests (tests, or a future no-reverse mode)
 * can wrap `parseAny` and reject other kinds.
 */
export function parseAny(line: string): ParseAnyResult {
  let obj: unknown;
  try {
    obj = JSON.parse(line);
  } catch (e) {
    return {
      kind: "parse_error",
      error: {
        code: -32700, // ERR_PARSE_ERROR
        message: `parse error: ${(e as Error).message}`,
        data: { line: line.slice(0, 200) },
      },
      id: null,
    };
  }
  if (typeof obj !== "object" || obj === null) {
    return badRequest("frame is not a JSON object");
  }
  const o = obj as Record<string, unknown>;
  if (o.jsonrpc !== "2.0") {
    return badRequest("missing or wrong jsonrpc version");
  }
  const hasMethod = typeof o.method === "string";
  const hasIdNum = typeof o.id === "number";
  const hasResult = Object.prototype.hasOwnProperty.call(o, "result");
  const hasError = Object.prototype.hasOwnProperty.call(o, "error");

  if (hasMethod && hasIdNum) {
    return { kind: "request", req: o as unknown as RpcRequest };
  }
  if (hasMethod && !hasIdNum) {
    return { kind: "notification", note: o as unknown as RpcNotification };
  }
  if (hasIdNum && (hasResult || hasError)) {
    return { kind: "response", resp: o as unknown as RpcResponse };
  }
  const idRaw = o.id;
  return {
    kind: "parse_error",
    error: {
      code: -32600, // ERR_INVALID_REQUEST
      message: "frame is not a valid JSON-RPC 2.0 request/response/notification",
    },
    id: typeof idRaw === "number" ? idRaw : null,
  };
}

function badRequest(msg: string): ParseAnyResult {
  return {
    kind: "parse_error",
    error: { code: -32600, message: msg },
    id: null,
  };
}

export function ok<R>(id: number, result: R): RpcResponseOk<R> {
  return { jsonrpc: "2.0", id, result };
}

export function err(id: number, error: RpcError): RpcResponseErr {
  return { jsonrpc: "2.0", id, error };
}

export function notify<P>(
  method: string,
  params: P,
): RpcNotification<string, P> {
  return { jsonrpc: "2.0", method, params };
}
