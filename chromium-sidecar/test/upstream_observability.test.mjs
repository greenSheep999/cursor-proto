import assert from 'node:assert/strict';
import test from 'node:test';

import { createUpstreamObservability } from '../src/upstream_observability.mjs';

test('reports only allowlisted RPC transport metadata', () => {
  let now = 1_000;
  const upstream = createUpstreamObservability(() => now);

  const run = upstream.begin(new URL('https://api2.cursor.sh/agent.v1.AgentService/RunSSE'));
  run.responseStarted(200);
  run.addResponseBytes(37);
  now = 1_125;
  run.finish();

  const append = upstream.begin(new URL('https://api2.cursor.sh/aiserver.v1.BidiService/BidiAppend'));
  append.responseStarted(200);
  append.addResponseBytes(5);
  now = 1_170;
  append.fail();

  assert.deepEqual(upstream.snapshot(), {
    run_sse: {
      requests: 1,
      failures: 0,
      last_status: 200,
      last_response_bytes: 37,
      last_duration_ms: 125,
    },
    bidi_append: {
      requests: 1,
      failures: 1,
      last_status: 200,
      last_response_bytes: 5,
      last_duration_ms: 45,
    },
  });
});
