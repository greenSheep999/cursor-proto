import assert from 'node:assert/strict';
import http from 'node:http';
import test from 'node:test';
import { chromium } from 'playwright';

import { browserFetchAndStream } from '../src/browser_fetch.mjs';

test('aborts a Chromium fetch that never returns response headers', async (t) => {
  let resolveAborted;
  const aborted = new Promise((resolve) => { resolveAborted = resolve; });
  const server = http.createServer((request, response) => {
    if (request.url === '/origin') {
      response.writeHead(200, { 'content-type': 'text/html' });
      response.end('<!doctype html><title>origin</title>');
      return;
    }
    request.on('aborted', resolveAborted);
    request.on('close', resolveAborted);
    // Intentionally never send response headers for the POST.
  });
  await new Promise((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, '127.0.0.1', resolve);
  });
  t.after(() => server.close());

  const address = server.address();
  const browser = await chromium.launch({ headless: true });
  t.after(() => browser.close());
  const page = await browser.newPage();
  await page.goto(`http://127.0.0.1:${address.port}/origin`);
  await page.exposeFunction('__cursorSidecarStart', () => {});
  await page.exposeFunction('__cursorSidecarChunk', () => {});
  await page.exposeFunction('__cursorSidecarEnd', () => {});

  const startedAt = Date.now();
  await assert.rejects(
    page.evaluate(browserFetchAndStream, {
      url: `http://127.0.0.1:${address.port}/hang`,
      headers: { 'content-type': 'application/octet-stream' },
      bodyBase64: Buffer.from('probe').toString('base64'),
      responseStartTimeoutMs: 100,
    }),
    /Chromium response start timeout after 100ms/,
  );
  assert.ok(Date.now() - startedAt < 1_500, 'timeout should fail promptly');
  await Promise.race([
    aborted,
    new Promise((_, reject) => setTimeout(() => reject(new Error('upstream request was not aborted')), 1_500)),
  ]);
});
