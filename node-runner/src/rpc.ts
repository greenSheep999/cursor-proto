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

// -------- reader: parse a Readable line-by-line into RpcRequests --------

/**
 * Buffered line reader. Given raw stdin chunks, emits one Buffer per
 * line (without the trailing `\n`). Uses a rolling Buffer so we
 * survive chunks that split a JSON object across boundaries.
 */
export class LineReader {
  private buf: Buffer = Buffer.alloc(0);

  /** Push one chunk (arbitrary length). Returns 0-or-more complete lines. */
  push(chunk: Buffer): string[] {
    this.buf = Buffer.concat([this.buf, chunk]);
    const lines: string[] = [];
    let start = 0;
    while (true) {
      const nl = this.buf.indexOf(0x0a /* \n */, start);
      if (nl === -1) {
        break;
      }
      const line = this.buf.subarray(start, nl).toString("utf8");
      if (line.length > 0) {
        lines.push(line);
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
 */
export function writeMessage(
  stream: Writable,
  msg: RpcResponse | RpcNotification,
): void {
  stream.write(JSON.stringify(msg) + "\n");
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
