import assert from 'node:assert/strict';
import test from 'node:test';

import { createRouteObservability } from '../src/route_observability.mjs';

test('reports credential-safe direct and proxied route counters', () => {
  const routes = createRouteObservability();

  assert.deepEqual(routes.snapshot(0), {
    direct_requests: 0,
    proxied_requests: 0,
    last_route: 'none',
    proxy_bridge_count: 0,
  });

  routes.record('direct');
  routes.record('socks-bridge');
  routes.record('http-proxy');

  assert.deepEqual(routes.snapshot(2), {
    direct_requests: 1,
    proxied_requests: 2,
    last_route: 'http-proxy',
    proxy_bridge_count: 2,
  });
});
