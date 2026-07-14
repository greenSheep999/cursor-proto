// Runner unit tests with a mocked SdkFactory. Verifies protocol-level
// behavior end-to-end without touching @cursor/sdk or the network.

import { strict as assert } from "node:assert";
import { test } from "node:test";
import { ProtocolError, Runner } from "./runner.js";
import type { SdkAgent, SdkFactory, SdkRun } from "./sdkInterface.js";

// -------- test doubles --------

class FakeRun implements SdkRun {
  cancelled = false;
  constructor(
    public readonly runId: string,
    private readonly events: unknown[],
  ) {}
  async *stream(): AsyncIterable<unknown> {
    for (const e of this.events) {
      if (this.cancelled) return;
      yield e;
    }
  }
  async cancel(): Promise<void> {
    this.cancelled = true;
  }
}

class FakeAgent implements SdkAgent {
  closed = false;
  runs: FakeRun[] = [];
  constructor(
    public readonly agentId: string,
    private readonly events: unknown[],
  ) {}
  async send(prompt: string): Promise<SdkRun> {
    const r = new FakeRun(`run-${this.runs.length}`, this.events);
    this.runs.push(r);
    return r;
  }
  async close(): Promise<void> {
    this.closed = true;
  }
}

function makeFactory(events: unknown[] = []): { factory: SdkFactory; agents: FakeAgent[] } {
  const agents: FakeAgent[] = [];
  const factory: SdkFactory = {
    async create() {
      const a = new FakeAgent(`agent-${agents.length}`, events);
      agents.push(a);
      return a;
    },
  };
  return { factory, agents };
}

interface Emitted {
  kind: string;
  method?: string;
  id?: number;
  result?: unknown;
  code?: number;
  params?: unknown;
}

function newRunner(opts: { apiKey?: string; events?: unknown[] } = {}): {
  runner: Runner;
  emitted: Emitted[];
  agents: FakeAgent[];
} {
  const emitted: Emitted[] = [];
  const { factory, agents } = makeFactory(opts.events ?? []);
  const runner = new Runner({
    apiKey: opts.apiKey,
    factory,
    sdkVersion: "test",
    emit: (m) => emitted.push(m as Emitted),
  });
  return { runner, emitted, agents };
}

// -------- ping --------

test("ping reports zero agents / runs on a fresh runner", () => {
  const { runner } = newRunner();
  const r = runner.ping();
  assert.equal(r.pong, true);
  assert.equal(r.active_agents, 0);
  assert.equal(r.active_runs, 0);
  assert.equal(r.sdk_version, "test");
});

// -------- agent lifecycle --------

test("agent.create rejects without apiKey", async () => {
  const { runner } = newRunner({ apiKey: undefined });
  await assert.rejects(
    runner.agentCreate({ runtime: "local", cwd: "/tmp", model: { id: "composer-2.5" } }),
    (e: unknown) => e instanceof ProtocolError && (e as ProtocolError).code === -32001,
  );
});

test("agent.create rejects local without cwd", async () => {
  const { runner } = newRunner({ apiKey: "test" });
  await assert.rejects(
    runner.agentCreate({ runtime: "local", model: { id: "composer-2.5" } } as any),
    (e: unknown) => e instanceof ProtocolError && (e as ProtocolError).code === -32602,
  );
});

test("agent.create rejects cloud without repos", async () => {
  const { runner } = newRunner({ apiKey: "test" });
  await assert.rejects(
    runner.agentCreate({ runtime: "cloud", model: { id: "composer-2.5" } } as any),
    (e: unknown) => e instanceof ProtocolError && (e as ProtocolError).code === -32602,
  );
});

test("agent.create rejects invalid runtime", async () => {
  const { runner } = newRunner({ apiKey: "test" });
  await assert.rejects(
    runner.agentCreate({ runtime: "bogus", cwd: "/tmp", model: { id: "m" } } as any),
    (e: unknown) => e instanceof ProtocolError && (e as ProtocolError).code === -32602,
  );
});

test("agent.create succeeds and returns agentId", async () => {
  const { runner } = newRunner({ apiKey: "test" });
  const r = await runner.agentCreate({
    runtime: "local",
    cwd: "/tmp",
    model: { id: "composer-2.5" },
  });
  assert.match(r.agentId, /^agent-/);
  assert.ok(r.createdAt);
});

test("agent.list returns all created agents", async () => {
  const { runner } = newRunner({ apiKey: "test" });
  await runner.agentCreate({ runtime: "local", cwd: "/a", model: { id: "m" } });
  await runner.agentCreate({ runtime: "local", cwd: "/b", model: { id: "m" } });
  const r = runner.agentList();
  assert.equal(r.agents.length, 2);
});

test("agent.status returns known agent, rejects unknown", async () => {
  const { runner } = newRunner({ apiKey: "test" });
  const r = await runner.agentCreate({
    runtime: "local",
    cwd: "/tmp",
    model: { id: "composer-2.5" },
  });
  const s = runner.agentStatus({ agentId: r.agentId });
  assert.equal(s.agentId, r.agentId);
  assert.equal(s.runtime, "local");
  assert.equal(s.model, "composer-2.5");
  assert.throws(() => runner.agentStatus({ agentId: "nonexistent" }), (e: unknown) =>
    e instanceof ProtocolError && (e as ProtocolError).code === -32002,
  );
});

test("agent.close removes the agent and calls SDK close()", async () => {
  const { runner, agents } = newRunner({ apiKey: "test" });
  const r = await runner.agentCreate({ runtime: "local", cwd: "/tmp", model: { id: "m" } });
  const result = await runner.agentClose({ agentId: r.agentId });
  assert.equal(result.ok, true);
  assert.equal(runner.agentList().agents.length, 0);
  assert.equal(agents[0].closed, true);
});

// -------- run lifecycle --------

test("agent.send emits run.event for each SDK event then run.done", async () => {
  const events = [
    { type: "assistant", delta: "Hello" },
    { type: "assistant", delta: " world" },
  ];
  const { runner, emitted } = newRunner({ apiKey: "test", events });
  const a = await runner.agentCreate({
    runtime: "local",
    cwd: "/tmp",
    model: { id: "m" },
  });
  const sendRes = await runner.agentSend({
    agentId: a.agentId,
    prompt: "hi",
  });
  // Give the stream pump a tick to drain.
  await new Promise((r) => setTimeout(r, 20));

  const notifies = emitted.filter((e) => e.kind === "notify");
  const eventNotifies = notifies.filter((n) => n.method === "run.event");
  const doneNotifies = notifies.filter((n) => n.method === "run.done");
  assert.equal(eventNotifies.length, 2, "two run.event emissions");
  assert.equal(doneNotifies.length, 1, "one run.done emission");
  assert.equal(
    (eventNotifies[0].params as { runId: string }).runId,
    sendRes.runId,
  );
});

test("run.cancel is idempotent (unknown runId returns ok)", async () => {
  const { runner } = newRunner({ apiKey: "test" });
  const r = await runner.runCancel({ runId: "no-such-run" });
  assert.equal(r.ok, true);
});
