// LineReader + parseLine unit tests.
// Run: npm run test:dev  (or `npm run build && npm test`)

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { LineReader, parseLine } from "./rpc.js";

test("LineReader emits complete lines and buffers partials", () => {
  const r = new LineReader();
  assert.deepEqual(r.push(Buffer.from("hello\nworld\n")), ["hello", "world"]);
  assert.deepEqual(r.push(Buffer.from("half")), []);
  assert.deepEqual(r.push(Buffer.from("-line\n")), ["half-line"]);
});

test("LineReader tolerates empty lines between messages", () => {
  const r = new LineReader();
  assert.deepEqual(r.push(Buffer.from("a\n\n\nb\n")), ["a", "b"]);
});

test("LineReader handles UTF-8 multibyte characters across chunk boundaries", () => {
  const r = new LineReader();
  const emoji = "汉字漢字emoji😀\n";
  const bytes = Buffer.from(emoji, "utf8");
  const half = Math.floor(bytes.length / 2);
  // Split mid-multibyte codepoint. The reader should still yield the
  // full string when both halves have arrived.
  const first = r.push(bytes.subarray(0, half));
  const second = r.push(bytes.subarray(half));
  assert.deepEqual([...first, ...second], ["汉字漢字emoji😀"]);
});

test("LineReader drops oversized frames without retaining them", () => {
  const r = new LineReader();
  assert.deepEqual(r.push(Buffer.alloc(LineReader.MAX_FRAME_BYTES + 1, 0x78)), []);
  assert.equal(r.pending().length, 0);
  assert.deepEqual(r.push(Buffer.from("still-oversized\nsmall\n")), ["small"]);
});

test("parseLine accepts a well-formed request", () => {
  const p = parseLine('{"jsonrpc":"2.0","id":7,"method":"ping"}');
  assert.equal(p.kind, "request");
  if (p.kind === "request") {
    assert.equal(p.req.id, 7);
    assert.equal(p.req.method, "ping");
  }
});

test("parseLine rejects bad JSON with -32700 and null id", () => {
  const p = parseLine("nope");
  assert.equal(p.kind, "parse_error");
  if (p.kind === "parse_error") {
    assert.equal(p.error.code, -32700);
    assert.equal(p.id, null);
  }
});

test("parseLine rejects wrong shape with -32600 and preserves numeric id if present", () => {
  const p = parseLine('{"jsonrpc":"1.0","id":9,"method":"ping"}');
  assert.equal(p.kind, "parse_error");
  if (p.kind === "parse_error") {
    assert.equal(p.error.code, -32600);
    assert.equal(p.id, 9);
  }
});

test("parseLine rejects missing id (protocol requires numeric id)", () => {
  const p = parseLine('{"jsonrpc":"2.0","method":"ping"}');
  assert.equal(p.kind, "parse_error");
  if (p.kind === "parse_error") {
    assert.equal(p.error.code, -32600);
    assert.equal(p.id, null);
  }
});

test("parseLine rejects string id (we only support numeric)", () => {
  const p = parseLine('{"jsonrpc":"2.0","id":"abc","method":"ping"}');
  assert.equal(p.kind, "parse_error");
  if (p.kind === "parse_error") {
    assert.equal(p.error.code, -32600);
    // id is null because our parser doesn't accept the string
    assert.equal(p.id, null);
  }
});
