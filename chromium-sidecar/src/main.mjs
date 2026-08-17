import http from 'node:http';
import { chromium } from 'playwright';
import { browserFetchAndStream } from './browser_fetch.mjs';

const listen = parseListen(process.env.CURSOR_CHROMIUM_LISTEN ?? '127.0.0.1:18901');
const maxConcurrency = positiveInt(process.env.CURSOR_CHROMIUM_MAX_CONCURRENCY, 8);
const requestLimit = positiveInt(process.env.CURSOR_CHROMIUM_REQUEST_LIMIT, 16 * 1024 * 1024);
const responseStartTimeoutMs = positiveInt(process.env.CURSOR_CHROMIUM_RESPONSE_START_TIMEOUT_MS, 20_000);
const requiredToken = process.env.CURSOR_CHROMIUM_SIDECAR_TOKEN ?? '';
const executablePath = process.env.CURSOR_CHROMIUM_EXECUTABLE_PATH || undefined;
let semaphore;

const browser = await chromium.launch({
  headless: true,
  executablePath,
  args: ['--disable-dev-shm-usage'],
});
const context = await browser.newContext({
  storageState: { cookies: [], origins: [] },
});

let shuttingDown = false;
browser.on('disconnected', () => {
  if (!shuttingDown) {
    process.stderr.write('[cursor-chromium-sidecar] browser disconnected\n');
    process.exit(1);
  }
});

const server = http.createServer(async (request, response) => {
  if (request.method === 'GET' && request.url === '/healthz') {
    const healthy = browser.isConnected() && !shuttingDown;
    response.writeHead(healthy ? 200 : 503, { 'content-type': 'application/json' });
    response.end(JSON.stringify({
      ok: healthy,
      active: semaphore.active,
      queued: semaphore.queued,
      max_concurrency: maxConcurrency,
    }));
    return;
  }

  if (requiredToken && request.headers['x-cursor-chromium-sidecar-token'] !== requiredToken) {
    response.writeHead(401, { 'content-type': 'text/plain' });
    response.end('invalid sidecar token');
    return;
  }

  const target = targetURL(request.url);
  if (!target || request.method !== 'POST') {
    response.writeHead(404, { 'content-type': 'text/plain' });
    response.end('unsupported route');
    return;
  }

  let body;
  try {
    body = await readRequestBody(request, requestLimit);
  } catch (error) {
    response.writeHead(error.code === 'BODY_TOO_LARGE' ? 413 : 400, { 'content-type': 'text/plain' });
    response.end(error.message);
    return;
  }

  const release = await semaphore.acquire();
  if (response.destroyed) {
    release();
    return;
  }

  let page;
  let completed = false;
  const cancel = () => {
    if (!completed && page) page.close().catch(() => {});
  };
  request.once('aborted', cancel);
  response.once('close', cancel);

  try {
    page = await context.newPage();
    await proxyThroughPage(page, request, response, target, body);
    completed = true;
  } catch (error) {
    if (!response.headersSent) {
      response.writeHead(502, { 'content-type': 'text/plain' });
    }
    if (!response.destroyed) response.end(`Chromium fetch failed: ${safeError(error)}`);
  } finally {
    completed = true;
    request.off('aborted', cancel);
    response.off('close', cancel);
    await page?.close().catch(() => {});
    release();
  }
});

server.requestTimeout = 0;
server.headersTimeout = 30_000;
server.keepAliveTimeout = 5_000;
server.listen(listen.port, listen.host, () => {
  process.stdout.write(`[cursor-chromium-sidecar] listening on http://${listen.host}:${listen.port} max_concurrency=${maxConcurrency}\n`);
});

async function proxyThroughPage(page, request, response, target, body) {
  const routeName = target.hostname.startsWith('api3.') ? 'api3' : 'api2';
  const originDocument = `https://${routeName}.cursor.sh/__cursor_chromium_sidecar_origin__`;
  await page.route(originDocument, (route) => route.fulfill({
    status: 200,
    contentType: 'text/html',
    body: '<!doctype html><title>cursor chromium sidecar</title>',
  }));
  await page.goto(originDocument, { waitUntil: 'domcontentloaded', timeout: 30_000 });

  await page.exposeFunction('__cursorSidecarStart', async (status, responseHeaders) => {
    if (response.headersSent || response.destroyed) return;
    response.writeHead(status, filterResponseHeaders(responseHeaders));
  });
  await page.exposeFunction('__cursorSidecarChunk', async (chunkBase64) => {
    if (response.destroyed) throw new Error('downstream closed');
    const chunk = Buffer.from(chunkBase64, 'base64');
    if (!response.write(chunk)) {
      await new Promise((resolve, reject) => {
        const cleanup = () => {
          response.off('drain', onDrain);
          response.off('close', onClose);
        };
        const onDrain = () => {
          cleanup();
          resolve();
        };
        const onClose = () => {
          cleanup();
          reject(new Error('downstream closed'));
        };
        response.once('drain', onDrain);
        response.once('close', onClose);
      });
    }
  });
  await page.exposeFunction('__cursorSidecarEnd', async () => {
    if (!response.destroyed) response.end();
  });

  const headers = filterRequestHeaders(request.headers);
  await page.evaluate(browserFetchAndStream, {
    url: target.href,
    headers,
    bodyBase64: body.toString('base64'),
    responseStartTimeoutMs,
  });
}

function targetURL(requestURL) {
  const match = /^\/(api2|api3)(\/.*)$/.exec(requestURL ?? '');
  if (!match) return null;
  return new URL(`https://${match[1]}.cursor.sh${match[2]}`);
}

function filterRequestHeaders(headers) {
  const removed = new Set([
    'accept-encoding', 'connection', 'content-length', 'host',
    'proxy-authorization', 'transfer-encoding', 'user-agent',
    'x-cursor-chromium-sidecar-token',
  ]);
  return Object.fromEntries(
    Object.entries(headers)
      .filter(([name, value]) => value !== undefined && !removed.has(name.toLowerCase()))
      .map(([name, value]) => [name, Array.isArray(value) ? value.join(', ') : value]),
  );
}

function filterResponseHeaders(headers) {
  const removed = new Set([
    'connection', 'content-encoding', 'content-length', 'transfer-encoding',
  ]);
  return Object.fromEntries(
    Object.entries(headers).filter(([name]) => !removed.has(name.toLowerCase())),
  );
}

async function readRequestBody(request, limit) {
  const chunks = [];
  let size = 0;
  for await (const chunk of request) {
    size += chunk.length;
    if (size > limit) {
      const error = new Error(`request body exceeds ${limit} bytes`);
      error.code = 'BODY_TOO_LARGE';
      throw error;
    }
    chunks.push(chunk);
  }
  return Buffer.concat(chunks);
}

class Semaphore {
  constructor(limit) {
    this.limit = limit;
    this.active = 0;
    this.waiters = [];
  }
  get queued() {
    return this.waiters.length;
  }
  async acquire() {
    if (this.active >= this.limit) {
      await new Promise((resolve) => this.waiters.push(resolve));
    }
    this.active += 1;
    let released = false;
    return () => {
      if (released) return;
      released = true;
      this.active -= 1;
      this.waiters.shift()?.();
    };
  }
}

semaphore = new Semaphore(maxConcurrency);

function parseListen(value) {
  const index = value.lastIndexOf(':');
  if (index <= 0) throw new Error(`invalid CURSOR_CHROMIUM_LISTEN: ${value}`);
  const host = value.slice(0, index);
  const port = Number.parseInt(value.slice(index + 1), 10);
  if (!['127.0.0.1', 'localhost', '::1'].includes(host) || !Number.isInteger(port) || port <= 0) {
    throw new Error(`CURSOR_CHROMIUM_LISTEN must be loopback host:port, got ${value}`);
  }
  return { host, port };
}

function positiveInt(value, fallback) {
  const parsed = Number.parseInt(value ?? '', 10);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : fallback;
}

function safeError(error) {
  return String(error?.message ?? error).replace(/[\r\n]+/g, ' ').slice(0, 500);
}

async function shutdown(signal) {
  if (shuttingDown) return;
  shuttingDown = true;
  process.stderr.write(`[cursor-chromium-sidecar] shutting down on ${signal}\n`);
  await new Promise((resolve) => server.close(resolve));
  await context.close().catch(() => {});
  await browser.close().catch(() => {});
  process.exit(0);
}

process.on('SIGTERM', () => shutdown('SIGTERM'));
process.on('SIGINT', () => shutdown('SIGINT'));
