// Resolve the repository-owned dependency so this diagnostic does not depend
// on a globally installed Playwright package.
import playwright from '../chromium-sidecar/node_modules/playwright/index.js';

const { chromium } = playwright;

const chunks = [];
for await (const chunk of process.stdin) chunks.push(chunk);
const input = JSON.parse(Buffer.concat(chunks).toString('utf8'));

const browser = await chromium.launch({
  headless: true,
  args: input.chromiumArgs ?? [],
});

try {
  // Playwright creates an isolated, temporary browser profile. Explicitly
  // omit credentials and disable the cache so a successful result cannot be
  // attributed to cookies, storage, a service worker, or a cached response.
  const context = await browser.newContext({
    storageState: { cookies: [], origins: [] },
    ignoreHTTPSErrors: input.ignoreHTTPSErrors ?? false,
    ...(input.userAgent ? { userAgent: input.userAgent } : {}),
  });
  const page = await context.newPage();
  const cdp = await context.newCDPSession(page);
  await cdp.send('Network.enable');
  await cdp.send('Network.setCacheDisabled', { cacheDisabled: true });

  let protocol = '';
  let remoteIPAddress = '';
  let requestHeaderNames = [];
  let browserSignals = {};
  cdp.on('Network.responseReceived', (event) => {
    if (event.response.url === input.url) {
      protocol = event.response.protocol ?? '';
      remoteIPAddress = event.response.remoteIPAddress ?? '';
    }
  });
  cdp.on('Network.requestWillBeSentExtraInfo', (event) => {
    const names = Object.keys(event.headers ?? {}).map((name) => name.toLowerCase());
    if (names.includes('x-cursor-client-version')) {
      requestHeaderNames = names.sort();
      const allowed = new Set([
        'accept', 'accept-encoding', 'accept-language', 'origin', 'priority',
        'referer', 'sec-ch-ua', 'sec-ch-ua-mobile', 'sec-ch-ua-platform',
        'sec-fetch-dest', 'sec-fetch-mode', 'sec-fetch-site', 'user-agent',
      ]);
      browserSignals = Object.fromEntries(
        Object.entries(event.headers ?? {})
          .map(([name, value]) => [name.toLowerCase(), value])
          .filter(([name]) => allowed.has(name)),
      );
    }
  });

  // Establish api2.cursor.sh as the document origin before fetch. This keeps
  // the probe on the same path as the successful browser experiment and
  // avoids CORS/preflight becoming an accidental independent variable.
  const originDocument = new URL('/__cursor_model_probe_origin__', input.url).href;
  await page.route(originDocument, (route) => route.fulfill({
    status: 200,
    contentType: 'text/html',
    body: '<!doctype html><title>cursor model probe</title>',
  }));
  await page.goto(originDocument, {
    waitUntil: 'domcontentloaded',
    timeout: 30_000,
  }).catch(() => {});

  if (input.stripBrowserHeaders) {
    await page.route(input.url, (route) => {
      const removed = new Set([
        'accept', 'origin', 'priority', 'referer',
        'sec-ch-ua', 'sec-ch-ua-mobile', 'sec-ch-ua-platform',
        'sec-fetch-dest', 'sec-fetch-mode', 'sec-fetch-site',
      ]);
      const headers = Object.fromEntries(
        Object.entries(route.request().headers())
          .filter(([name]) => !removed.has(name.toLowerCase())),
      );
      route.continue({ headers });
    });
  }

  const result = await page.evaluate(async ({ url, headers, bodyBase64 }) => {
    const raw = atob(bodyBase64);
    const body = new Uint8Array(raw.length);
    for (let i = 0; i < raw.length; i += 1) body[i] = raw.charCodeAt(i);

    const response = await fetch(url, {
      method: 'POST',
      headers,
      body,
      credentials: 'omit',
      cache: 'no-store',
    });
    const responseBytes = new Uint8Array(await response.arrayBuffer());
    let binary = '';
    const stride = 0x8000;
    for (let i = 0; i < responseBytes.length; i += stride) {
      binary += String.fromCharCode(...responseBytes.subarray(i, i + stride));
    }
    return {
      status: response.status,
      bodyBase64: btoa(binary),
      byteLength: responseBytes.length,
    };
  }, input);

  process.stdout.write(JSON.stringify({
    ...result,
    protocol,
    remoteIPAddress,
    requestHeaderNames,
    browserSignals,
  }));
} finally {
  await browser.close();
}
