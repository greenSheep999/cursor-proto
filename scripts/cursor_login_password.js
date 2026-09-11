// scripts/cursor_login_password.js
//
// One-shot Cursor password login: consent → email → password → radar/magic-code
// (read from an Outlook web tab that stays signed in) → loginDeepControl
// authorize → cursor-login poller writes the account JSON.
//
// This closes the gap I hit twice: the three .tmp-cursor-login-*.js scripts
// stopped short of the loginDeepControl "Sign in to Cursor desktop" button and
// depended on a third-party mail-code service (150.158.1.71) that only knows
// its own domain. This script reads the code from a real Outlook tab you keep
// open in a shared CDP Chrome, then completes every step in-process so the
// short-lived radar code is used within seconds of arriving.
//
// Usage:
//   1. Start (or reuse) CDP Chrome:
//        open -na "Google Chrome" --args --remote-debugging-port=9222 \
//          --user-data-dir=/tmp/cursor-import-profile \
//          --no-first-run --no-default-browser-check about:blank
//   2. Sign that Chrome into outlook.live.com under the account's inbox.
//   3. Start `cursor-login -no-browser` in another terminal; copy the printed
//      URL into CURSOR_LOGIN_URL below.
//   4. Run:
//        CURSOR_LOGIN_URL=... CURSOR_LOGIN_EMAIL=... CURSOR_LOGIN_PASSWORD=... \
//          node scripts/cursor_login_password.js
//   The poller writes ~/.cli-proxy-api/cursor-<email>.json. It now includes
//   "type":"cursor" because auth.SaveAccount injects it.
//
// Secrets come from env only; nothing sensitive is logged.

const { chromium } = require('playwright');

const red = (s) => String(s)
  .replace(/((?:challenge|state|code|token|session_id|authorization_session_id|uuid)=)[^&]+/g, '$1[RED]')
  .replace(/[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}/gi, '[acct]')
  .replace(/\b\d{6}\b/g, '[OTP]');

const log = (...a) => console.log(`[${new Date().toISOString().slice(11, 19)}]`, ...a);

async function readCodes(mail) {
  await mail.reload({ waitUntil: 'domcontentloaded' }).catch(() => {});
  await mail.waitForTimeout(2500);
  return mail.evaluate(() => {
    const out = [];
    document.querySelectorAll('[role="option"],[role="listitem"]').forEach((el) => {
      const t = (el.innerText || '').replace(/\s+/g, ' ').trim();
      if (!/Cursor/i.test(t)) return;
      const m = t.match(/one-time code is (\d{6})/i);
      if (m) out.push(m[1]);
    });
    return out;
  });
}

(async () => {
  const url = process.env.CURSOR_LOGIN_URL;
  const email = process.env.CURSOR_LOGIN_EMAIL;
  const password = process.env.CURSOR_LOGIN_PASSWORD;
  if (!url || !email || !password) throw new Error('need CURSOR_LOGIN_URL / _EMAIL / _PASSWORD');
  const cdp = process.env.CDP_URL || 'http://127.0.0.1:9222';

  const browser = await chromium.connectOverCDP(cdp);
  const ctx = browser.contexts()[0];
  const mail = ctx.pages().find((p) => p.url().includes('outlook.live.com'));
  if (!mail) throw new Error('outlook.live.com tab not found — sign in first');

  const before = new Set(await readCodes(mail));
  log(`mailbox baseline: ${before.size} existing Cursor codes`);

  const page = await ctx.newPage();
  await page.goto(url, { waitUntil: 'domcontentloaded', timeout: 60000 });

  try {
    const consent = page.getByRole('button', { name: /Continue to sign in/i });
    await consent.waitFor({ state: 'visible', timeout: 15000 });
    await consent.click();
    log('consent');
  } catch { log('no consent'); }

  try {
    const emailField = page.locator('input[type="email"]');
    await emailField.waitFor({ state: 'visible', timeout: 15000 });
    await emailField.fill(email);
    await page.getByRole('button', { name: /Continue with email/i }).click();
    log('email submitted');
  } catch { log('no email field'); }

  const pw = page.locator('input[type="password"]');
  await pw.waitFor({ state: 'visible', timeout: 25000 });
  await pw.fill(password);
  // First() with exact match — must NOT catch the "Email sign-in code" button.
  await page.getByRole('button', { name: /^\s*Sign in\s*$/i }).first().click();
  log('password submitted (triggers radar code send)');

  for (let i = 0; i < 20; i++) {
    if (/radar-challenge|magic-code/.test(page.url())) break;
    await page.waitForTimeout(1000);
  }
  log('at challenge:', red(page.url()));

  let fresh = null;
  for (let i = 0; i < 30 && !fresh; i++) {
    const now = await readCodes(mail);
    fresh = now.find((c) => !before.has(c)) || null;
    if (!fresh) await mail.waitForTimeout(2500);
  }
  if (!fresh) throw new Error('no new code arrived after 75s');
  log('fresh code arrived, filling immediately');

  await page.bringToFront();
  const boxes = page.locator('input:not([type="hidden"])');
  const n = await boxes.count();
  if (n >= 6) {
    for (let i = 0; i < 6; i++) await boxes.nth(i).fill(fresh[i]);
  } else {
    const single = page.locator('input[autocomplete="one-time-code"], input[name="code"], input[inputmode="numeric"]').first();
    if (await single.count().catch(() => 0)) { await single.click(); await single.fill(fresh); }
    else { await page.keyboard.type(fresh, { delay: 60 }); }
  }
  await page.waitForTimeout(600);
  const codeSubmit = page.getByRole('button', { name: /continue|submit|verify|sign in/i }).first();
  if (await codeSubmit.count().catch(() => 0)) await codeSubmit.click().catch(() => {});
  else await page.keyboard.press('Enter');
  log('code submitted');

  for (let i = 0; i < 30; i++) {
    await page.waitForTimeout(1200);
    if (/loginDeepControl/.test(page.url())) break;
  }
  log('landed:', red(page.url()));
  if (!/loginDeepControl/.test(page.url())) throw new Error('did not reach loginDeepControl');

  // Final step every earlier .tmp script forgot: authorize the desktop client.
  await page.waitForTimeout(2500);
  const authorize = page.locator('button:has-text("Sign in")').last();
  await authorize.waitFor({ state: 'visible', timeout: 15000 });
  await authorize.click();
  log('clicked "Sign in to Cursor desktop"');

  await page.waitForTimeout(4000);
  log('done — cursor-login poller should complete within one interval');
})().catch((e) => { console.error('ERR', e.message); process.exitCode = 1; });
