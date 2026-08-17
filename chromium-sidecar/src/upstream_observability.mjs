const RPC_NAMES = new Map([
  ['AvailableModels', 'available_models'],
  ['RunSSE', 'run_sse'],
  ['BidiAppend', 'bidi_append'],
]);

export function createUpstreamObservability(now = () => Date.now()) {
  const stats = new Map();

  return {
    begin(target) {
      const name = rpcName(target);
      const startedAt = now();
      const current = stats.get(name) ?? emptyStat();
      current.requests += 1;
      stats.set(name, current);
      let completed = false;
      let status = 0;
      let responseBytes = 0;

      const complete = (failed) => {
        if (completed) return;
        completed = true;
        current.failures += failed ? 1 : 0;
        current.last_status = finiteNonNegative(status);
        current.last_response_bytes = finiteNonNegative(responseBytes);
        current.last_duration_ms = finiteNonNegative(now() - startedAt);
      };

      return {
        responseStarted(value) {
          status = finiteNonNegative(value);
        },
        addResponseBytes(value) {
          responseBytes += finiteNonNegative(value);
        },
        finish() {
          complete(false);
        },
        fail() {
          complete(true);
        },
      };
    },

    snapshot() {
      return Object.fromEntries([...stats.entries()].map(([name, stat]) => [name, { ...stat }]));
    },
  };
}

function rpcName(target) {
  const method = target.pathname.split('/').filter(Boolean).at(-1) ?? '';
  return RPC_NAMES.get(method) ?? 'other';
}

function emptyStat() {
  return {
    requests: 0,
    failures: 0,
    last_status: 0,
    last_response_bytes: 0,
    last_duration_ms: 0,
  };
}

function finiteNonNegative(value) {
  return Number.isFinite(value) && value > 0 ? Math.round(value) : 0;
}
