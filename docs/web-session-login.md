# Cursor website-session compatibility login

> Three ways to get a browser or a pool entry authenticated, cheapest first:
>
> | Goal | Method | Section |
> |---|---|---|
> | Open `cursor.com` in a browser as the account this machine's IDE already uses | Reuse the local IDE session token as a cookie | [Fastest](#fastest-reuse-the-local-ide-session-token-in-a-browser) |
> | Save the current IDE account (access + refresh token) before touching the session | `cursor-export` to a private JSON | [Backing up](#backing-up-the-ide-account-before-you-touch-the-session) |
> | Mint a fresh IDE `type=session` token from a website-only export | PKCE + companion cookies | [Compatibility flow](#the-compatibility-flow) below |

## Fastest: reuse the local IDE session token in a browser

When the IDE on this machine is already signed in, you do **not** need OAuth,
OTP, or the website password to open `cursor.com` as that account. The IDE
holds a `type=session` JWT that the site's Dashboard endpoints accept, so
copying it into a browser cookie is enough.

This reuses the existing session rather than minting a new one: it will not
revoke the IDE session and the IDE will not revoke it (unlike
[the compatibility flow](#the-compatibility-flow), where a fresh PKCE login
can bump the active session — see the last paragraph).

1. Read the session token and user id from the IDE's SQLite storage
   (read-only; the IDE can keep running):

   ```bash
   DB="$HOME/Library/Application Support/Cursor/User/globalStorage/state.vscdb"
   USER_ID=$(sqlite3 "file:$DB?mode=ro" "SELECT value FROM ItemTable WHERE key='cursorAuth/cachedUserID'")
   JWT=$(sqlite3 "file:$DB?mode=ro" "SELECT value FROM ItemTable WHERE key='cursorAuth/accessToken'")
   ```

   The `WorkosCursorSessionToken` cookie is the `user_id::JWT` pair.
   `cursor-export -stdout` reads the same keys if you prefer the Go tool.

2. Launch a throwaway Chrome so you never touch your daily profile. `open -na`
   fully detaches it from the shell (a plain `chrome & ` dies when the shell
   that spawned it is recycled):

   ```bash
   open -na "Google Chrome" --args \
     --remote-debugging-port=9222 \
     --user-data-dir=/tmp/cursor-login-profile \
     --no-first-run --no-default-browser-check about:blank
   ```

3. Inject the cookie on `.cursor.com` over the DevTools protocol and open the
   dashboard. Requires `playwright` (`npm i playwright`), which the repo
   already depends on:

   ```bash
   BP_USER_ID="$USER_ID" BP_ACCESS_TOKEN="$JWT" node - <<'JS'
   const { chromium } = require('playwright');
   (async () => {
     const value = `${process.env.BP_USER_ID}::${process.env.BP_ACCESS_TOKEN}`;
     const browser = await chromium.connectOverCDP('http://127.0.0.1:9222');
     const context = browser.contexts()[0];
     await context.addCookies([{
       name: 'WorkosCursorSessionToken', value, domain: '.cursor.com', path: '/',
       secure: true, sameSite: 'Lax', expires: Math.floor(Date.now()/1000) + 60*24*3600,
     }]);
     const page = context.pages()[0] || await context.newPage();
     await page.goto('https://cursor.com/dashboard', { waitUntil: 'domcontentloaded' });
     process.exit(0);
   })().catch((e) => { console.error(e.message); process.exit(1); });
   JS
   ```

Never echo `$JWT` into logs, terminal scrollback, or a tracked file — it is a
live 60-day credential.

### When this does not apply

The token→cookie shortcut only works when *this* machine's IDE is signed in as
the account you want. For a website-only export (`type=web` JWT) with no local
IDE session, or to mint a real IDE `type=session` token, use
[the compatibility flow](#the-compatibility-flow).

Automated browser logins that drive `loginDeepControl` and pull the code from
an email-OTP service only cover accounts registered with that service; a
normal personal account (e.g. an `@outlook.com` address) returns
`该邮箱不在库中` / "not in library" and the OTP step cannot complete. Reach for
the cookie shortcut or a manual OTP entry instead.

## Backing up the IDE account before you touch the session

Any flow that can replace the active session (a fresh OAuth login, an account
switch, another device) can leave the IDE returning `ERROR_NOT_LOGGED_IN`.
Snapshot the current account first so you can restore or re-import it:

```bash
BK="$HOME/.cursor-backup/cursor-ide-backup-$(date +%Y%m%d-%H%M%S).json"
mkdir -p "$(dirname "$BK")" && chmod 700 "$(dirname "$BK")"
go run ./cmd/cursor-export -out "$BK" -note "pre-login backup $(date -u +%FT%TZ)"
```

`cursor-export` reads `state.vscdb` read-only and writes the access token,
refresh token, machine ids, and team id as a CPA-compatible JSON with `0600`
permissions. The same file drops straight into a `cursor-login` / `cursor-proxy`
pool directory, so the backup doubles as a re-import path. Keep it outside the
repo (the `accounts/*.json` entries are gitignored, and this repo's convention
for in-place backups is a timestamped `*.bak-<YYYYMMDD-HHMMSS>` sibling).

For the long-running proxy there is also a runtime safety copy: `auth.SnapshotIDEDB`
mirrors `state.vscdb` (plus `-wal`/`-shm`) into `~/Library/Caches/cursor-proxy/`
so account reloads never race the IDE's writer. That snapshot is an
operational read-safety mechanism, not a user-facing account backup — use
`cursor-export` when you want a portable copy of the credentials.

## The compatibility flow

Some account exports use this shape:

```text
email----password----user_id::JWT
```

The JWT in these exports can be a Cursor website token with
`"type":"web"`. It is not an IDE access token and must not be written
directly to `cursorAuth/accessToken`.

Cursor's stable browser-assisted flow is:

1. Sign in to `cursor.com` with a normal account so WorkOS creates the
   companion browser/device cookies.
2. Export the complete `Cookie` request header for `cursor.com` into a
   private file. Do not export only `WorkosCursorSessionToken`.
3. Put the target account export in a separate private credential file.
4. Run `cursor-login`; it preserves the companion cookies, replaces only
   `WorkosCursorSessionToken`, authorizes the PKCE request through
   `loginDeepCallbackControl`, and polls for real IDE `type=session`
   access and refresh tokens.

```bash
chmod 600 account.txt cursor-cookie-header.txt

go run ./cmd/cursor-login \
  -credential-file account.txt \
  -cookie-header-file cursor-cookie-header.txt \
  -out ~/.cursor-pool
```

`account.txt` may contain either the full exported line or only the
`user_id::JWT` value. The password column is ignored and is never written
to the generated account JSON.

`cursor-cookie-header.txt` contains one HTTP Cookie header value, for
example:

```text
workos-device-cookie=...; WorkosCursorSessionToken=old-value; other=...
```

The command refuses to run when it cannot find a companion WorkOS cookie.
This prevents the short-lived false login observed when a standalone web
cookie authorizes PKCE but the resulting IDE session is revoked shortly
afterward.

The resulting account file contains only the OAuth poll result. Its access
token is checked to ensure it is an IDE `type=session` token. Batch imports
also reject `type=web` tokens before Dashboard validation.

If another device or account manager replaces the active Cursor session,
the IDE can start returning `ERROR_NOT_LOGGED_IN` even though the JWT has
not reached its `exp` timestamp. Re-run the same command to obtain a fresh
IDE session. The source credential and cookie-header files are deliberately
kept separate from the generated pool entry so operators can control their
lifetime and permissions.
