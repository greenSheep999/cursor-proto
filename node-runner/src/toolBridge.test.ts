// Unit tests for ToolBridge — success, notify-reject, and timeout
// paths. We use a mock Writable that records the framed JSON so we
// can drive handleResponse / handleToolResultNotify without a real
// child process.

import { test } from "node:test";
import { strict as assert } from "node:assert";
import { Writable } from "node:stream";
import { ToolBridge } from "./toolBridge.js";
import {
  ERR_TOOL_CALL_CANCELLED,
  ERR_TOOL_CALL_TIMEOUT,
} from "./protocol.js";

function makeCapture(): { stream: Writable; frames: string[] } {
  const frames: string[] = [];
  const stream = new Writable({
    write(chunk: Buffer | string, _enc, cb) {
      frames.push(chunk.toString().replace(/\n$/, ""));
      cb();
    },
  });
  return { stream, frames };
}

test("toolBridge resolves execute() on success response", async () => {
  const { stream, frames } = makeCapture();
  const bridge = new ToolBridge(stream);
  const tools = bridge.buildCustomTools("run-1", [{ name: "greet" }]);
  const p = tools.greet.execute({ x: 1 }, { toolCallId: "call-A" });
  // Wait a tick so writeMessage flushes.
  await new Promise((r) => setImmediate(r));
  assert.equal(frames.length, 1);
  const req = JSON.parse(frames[0]);
  assert.equal(req.method, "tool.execute");
  assert.equal(req.params.callId, "call-A");
  assert.equal(req.params.name, "greet");
  bridge.handleResponse(req.id, { content: [{ type: "text", text: "hi" }] }, null);
  const result = await p;
  assert.deepEqual(result, { content: [{ type: "text", text: "hi" }] });
});

test("toolBridge synthesizes tool_error on error response", async () => {
  const { stream, frames } = makeCapture();
  const bridge = new ToolBridge(stream);
  const tools = bridge.buildCustomTools("run-1", [{ name: "boom" }]);
  const p = tools.boom.execute({}, { toolCallId: "call-B" });
  await new Promise((r) => setImmediate(r));
  const req = JSON.parse(frames[0]);
  bridge.handleResponse(req.id, null, { code: -1, message: "nope" });
  const raw = await p;
  const result = raw as { content: Array<{ text?: string }>; isError?: boolean };
  assert.equal(result.isError, true);
  assert.match(result.content[0]?.text ?? "", /nope/);
});

test("toolBridge honors tool.result notify to short-circuit pending", async () => {
  const { stream, frames } = makeCapture();
  const bridge = new ToolBridge(stream);
  const tools = bridge.buildCustomTools("run-1", [{ name: "slow" }]);
  const p = tools.slow.execute({}, { toolCallId: "call-C" });
  await new Promise((r) => setImmediate(r));
  assert.equal(frames.length, 1);
  bridge.handleToolResultNotify({
    callId: "call-C",
    error: { code: ERR_TOOL_CALL_CANCELLED, message: "run cancelled" },
  });
  const raw = await p;
  const result = raw as { content: Array<{ text?: string }>; isError?: boolean };
  assert.equal(result.isError, true);
  assert.match(result.content[0]?.text ?? "", /run cancelled/);
});

test("toolBridge times out after per-tool timeoutMs", async () => {
  const { stream } = makeCapture();
  const bridge = new ToolBridge(stream);
  const tools = bridge.buildCustomTools("run-1", [
    { name: "quick", timeoutMs: 25 },
  ]);
  const p = tools.quick.execute({}, { toolCallId: "call-D" });
  const raw = await p;
  const result = raw as { content: Array<{ text?: string }>; isError?: boolean };
  assert.equal(result.isError, true);
  assert.match(result.content[0]?.text ?? "", new RegExp(`${ERR_TOOL_CALL_TIMEOUT}`));
});

test("toolBridge rejectRun sweeps in-flight tools with cancelled error", async () => {
  const { stream } = makeCapture();
  const bridge = new ToolBridge(stream);
  const tools = bridge.buildCustomTools("run-9", [{ name: "hang" }]);
  const p = tools.hang.execute({}, { toolCallId: "call-E" });
  await new Promise((r) => setImmediate(r));
  bridge.rejectRun("run-9", "run ended");
  const raw = await p;
  assert.ok(typeof raw === "object" && raw !== null && "content" in raw);
  const result = raw as { content: Array<{ text?: string }>; isError?: boolean };
  assert.equal(result.isError, true);
  const text = result.content[0]?.text ?? "";
  assert.match(text, new RegExp(`${ERR_TOOL_CALL_CANCELLED}`));
});

test("toolBridge rejectRun does not cancel another run", async () => {
  const { stream, frames } = makeCapture();
  const bridge = new ToolBridge(stream);
  const runOne = bridge.buildCustomTools("run-one", [{ name: "hang" }]);
  const runTwo = bridge.buildCustomTools("run-two", [{ name: "hang" }]);
  const first = runOne.hang.execute({}, { toolCallId: "call-one" });
  const second = runTwo.hang.execute({}, { toolCallId: "call-two" });
  await new Promise((r) => setImmediate(r));

  bridge.rejectRun("run-one", "run ended");
  const firstResult = (await first) as { isError?: boolean };
  assert.equal(firstResult.isError, true);

  const secondRequest = JSON.parse(frames[1]);
  bridge.handleResponse(secondRequest.id, "still pending", null);
  assert.equal(await second, "still pending");
});

test("toolBridge falls back to synthesized callId when SDK omits toolCallId", async () => {
  const { stream, frames } = makeCapture();
  const bridge = new ToolBridge(stream);
  const tools = bridge.buildCustomTools("run-42", [{ name: "anon" }]);
  const p = tools.anon.execute({}, {});
  await new Promise((r) => setImmediate(r));
  const req = JSON.parse(frames[0]);
  assert.match(req.params.callId, /^local-run-42-\d+/);
  bridge.handleResponse(req.id, "ok", null);
  const result = await p;
  assert.equal(result, "ok");
});
