// Adapter from our SdkFactory / SdkAgent / SdkRun interface to the
// real @cursor/sdk package. All coupling to `@cursor/sdk` lives here
// so protocol-level code in runner.ts and index.ts stays testable
// without a Cursor account.
//
// The SDK's own API (see https://cursor.com/cn/docs/sdk/typescript):
//
//   Agent.create({ apiKey, model, local?: {cwd}, cloud?: {repos, ...} })
//     → Promise<SDKAgent>
//   agent.send(prompt | SDKUserMessage, options?) → Promise<Run>
//   run.stream() → AsyncIterable<SDKMessage>
//   run.cancel() → Promise<void>
//   agent.close(): void
//
// Our wrapper normalizes those into a small stable shape and
// generates a `runId` at send time so the protocol can address
// runs by an opaque identifier (@cursor/sdk currently returns a
// Run object but no stable id; we assign one).

import { randomUUID } from "node:crypto";
import type {
  SdkAgent,
  SdkAgentOptions,
  SdkFactory,
  SdkRun,
  SdkSendOptions,
} from "./sdkInterface.js";

// Lazy import so unit tests that swap in a fake factory don't have
// to install @cursor/sdk. Node ESM `import()` is async; we await
// it once in create().
type CursorSdkModule = {
  Agent: {
    create(opts: unknown): Promise<CursorSdkAgent>;
  };
};

interface CursorSdkAgent {
  readonly agentId: string;
  send(prompt: string, options?: unknown): Promise<CursorSdkRun>;
  close(): void | Promise<void>;
}

interface CursorSdkRun {
  stream(): AsyncIterable<unknown>;
  wait?: () => Promise<{
    status: "finished" | "error" | "cancelled";
    result?: string;
    usage?: {
      inputTokens: number;
      outputTokens: number;
      cacheReadTokens: number;
      cacheWriteTokens: number;
      totalTokens: number;
      reasoningTokens?: number;
    };
    durationMs?: number;
    error?: { message: string; code?: string };
  }>;
  cancel?: () => Promise<void>;
}

let sdkPromise: Promise<CursorSdkModule> | null = null;

async function loadSdk(): Promise<CursorSdkModule> {
  if (sdkPromise === null) {
    sdkPromise = import("@cursor/sdk") as unknown as Promise<CursorSdkModule>;
  }
  return sdkPromise;
}

/** SdkFactory backed by the real @cursor/sdk package. */
export const realSdkFactory: SdkFactory = {
  async create(opts: SdkAgentOptions): Promise<SdkAgent> {
    const sdk = await loadSdk();

    // Build the SDK's option object. The SDK's own types accept
    // either `local` or `cloud`; we mirror our own opts shape 1:1.
    const sdkOpts: Record<string, unknown> = {
      apiKey: opts.apiKey,
      model: opts.model,
    };
    if (opts.local) {
      sdkOpts.local = { cwd: opts.local.cwd };
    }
    if (opts.cloud) {
      sdkOpts.cloud = opts.cloud;
    }

    const inner = await sdk.Agent.create(sdkOpts);
    return new RealAgent(inner);
  },
};

class RealAgent implements SdkAgent {
  constructor(private readonly inner: CursorSdkAgent) {}

  get agentId(): string {
    return this.inner.agentId;
  }

  async send(prompt: string, options?: SdkSendOptions): Promise<SdkRun> {
    // Translate our SdkSendOptions into the SDK's SendOptions shape.
    // customTools go under `local.customTools` per SDK contract
    // (LocalSendOptions.customTools in @cursor/sdk options.d.ts).
    // Cloud agents reject `local.customTools` — the SDK throws with
    // a clear message; we let it bubble.
    let sdkSendOpts: Record<string, unknown> | undefined;
    if (options?.customTools) {
      sdkSendOpts = { local: { customTools: options.customTools } };
    }
    const runInner = sdkSendOpts
      ? await this.inner.send(prompt, sdkSendOpts)
      : await this.inner.send(prompt);
    // The SDK doesn't currently expose a stable run id; we assign
    // one so the protocol can address runs uniformly across SDK
    // versions.
    const runId = `run-${randomUUID()}`;
    return new RealRun(runId, runInner);
  }

  async close(): Promise<void> {
    const maybe = this.inner.close();
    if (maybe && typeof (maybe as Promise<void>).then === "function") {
      await maybe;
    }
  }
}

class RealRun implements SdkRun {
  constructor(
    public readonly runId: string,
    private readonly inner: CursorSdkRun,
  ) {}

  stream(): AsyncIterable<unknown> {
    return this.inner.stream();
  }

  async wait(): Promise<import("./sdkInterface.js").SdkRunResult> {
    if (this.inner.wait) {
      return await this.inner.wait();
    }
    // Older SDKs without wait(): drain the stream and synthesize a
    // finished result with no usage — better than throwing.
    for await (const _ of this.inner.stream()) {
      // consume
    }
    return { status: "finished" };
  }

  async cancel(): Promise<void> {
    if (this.inner.cancel) {
      await this.inner.cancel();
    }
  }
}
