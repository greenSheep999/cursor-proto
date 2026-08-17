export function createRouteObservability() {
  let directRequests = 0;
  let proxiedRequests = 0;
  let lastRoute = 'none';

  return {
    record(route) {
      if (route === 'direct') {
        directRequests += 1;
      } else {
        proxiedRequests += 1;
      }
      lastRoute = route;
    },

    snapshot(proxyBridgeCount) {
      return {
        direct_requests: directRequests,
        proxied_requests: proxiedRequests,
        last_route: lastRoute,
        proxy_bridge_count: proxyBridgeCount,
      };
    },
  };
}
