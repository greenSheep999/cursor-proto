# Cursor OTP Login — Manual End-to-End Test

The Cursor plugin's OTP mode drives a real magic-code login over
plain HTTP, no browser required. This document is the operator
playbook for verifying the flow against production Cursor.

Automated tests cover every branch of the code path against
`httptest` mocks — see `plugin/cursor/kernel/otp_flow_test.go`. This
document is for the last-mile check that Cursor still accepts the
request shape we ship.

## Prerequisites

- A real Cursor account you can log into.
- IMAP credentials for the mailbox that account receives mail on
  (or a way to read the 6-digit code out-of-band).
- A YesCaptcha account with credit — https://yescaptcha.com. Solve
  fee is roughly $0.001 per Turnstile.
- CPA running with the Cursor plugin loaded (see
  `docs/phase-8d-plugin-management.md` for the plugin registration
  path). The plugin needs read access to `YESCAPTCHA_API_KEY`.

## Environment

Set the YesCaptcha key in the CPA process environment before
starting the server:

```
export YESCAPTCHA_API_KEY=<your-yescaptcha-client-key>
./cli-proxy-api
```

The key can also be passed per-request as `metadata.yescaptcha_key`;
env is the recommended default so operators do not have to paste it
into the management UI on every login.

## Kick off the login

CPA's management API for provider logins is
`POST /v0/management/cursor-auth-url` (mirroring the sibling
endpoints for other providers). For OTP mode, include the email and
IMAP credentials:

```bash
curl -sS -X POST http://localhost:8080/v0/management/cursor-auth-url \
  -H 'Content-Type: application/json' \
  -d '{
    "mode": "otp",
    "email": "you@example.com",
    "mail_host": "outlook.office365.com",
    "mail_port": 993,
    "mail_user": "you@example.com",
    "mail_pass": "<app-password>"
  }'
```

Response shape:

```json
{
  "provider": "cursor",
  "state": "0f4c…",
  "expires_at": "2026-07-13T…",
  "metadata": {
    "mode": "otp",
    "otp_pending": true,
    "email": "you@example.com",
    "magic_state": "<challenge-id>",
    "inbox_source": "imap:outlook.office365.com"
  }
}
```

`state` is the opaque handle you'll pass to Poll. The plugin has
already:

1. Fetched `authenticator.cursor.sh/?…` and lifted the Turnstile
   sitekey + Next.js server-action id.
2. Asked YesCaptcha to solve the Turnstile (this is the ~15–40s
   step you may notice as latency on the Start call).
3. POSTed the multipart form with `1_intent=magic-code` and the
   fingerprint signals blob.
4. Stashed the WorkOS cookies, challenge id, IMAP config, and
   YesCaptcha key in memory.

Cursor should now have sent a 6-digit code to `you@example.com`.

## Poll for completion

Poll every ~5 seconds until you see `success`:

```bash
curl -sS -X POST http://localhost:8080/v0/management/cursor-auth-poll \
  -H 'Content-Type: application/json' \
  -d '{"state": "<state from Start>"}'
```

Expected progression:

- `{"status":"pending","message":"waiting for cursor magic-code email"}`
  — inbox has not yet received the code.
- `{"status":"pending", …}` — one or more times while YesCaptcha
  solves the magic-code page's second Turnstile (~15-40s).
- `{"status":"success","auth":{ … }}` — the auth record is now in
  CPA's live pool. It's also persisted under `auths/cursor-<sanitized-email>.json`.

## If you already have the OTP in hand

Skip IMAP entirely by passing the literal code — useful for testing
the second half of the flow without waiting on mail delivery:

```bash
curl -sS -X POST http://localhost:8080/v0/management/cursor-auth-url \
  -H 'Content-Type: application/json' \
  -d '{"mode":"otp","email":"you@example.com","otp":"123456"}'
```

The immediate next Poll call will submit the code and (assuming the
code was still valid) complete the flow.

## Watching the plugin logs

The Cursor plugin logs at info level for every step of the OTP
handshake. Set the CPA log level to debug to see the raw regex
matches (sitekey, next-action) plus request URLs:

```
CLI_PROXY_LOG_LEVEL=debug ./cli-proxy-api
```

Look for lines starting with `cursor: otp start` and `cursor: otp poll`.

## Common failures

| Symptom                                                                              | Likely cause                                                                                                                                                             |
| ------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `error: otp mode requires YESCAPTCHA_API_KEY env or metadata.yescaptcha_key`         | The key is unset in the CPA env and was not passed as metadata.                                                                                                          |
| `error: turnstile solve: solve timeout after 1m30s`                                  | Your YesCaptcha account is out of credit, or the sitekey we extracted is stale. Check `yescaptcha.com` balance first.                                                    |
| `error: authenticator rejected form submit — likely fingerprint blocked (HTTP 403)`  | Cursor tightened the Turnstile / signals correlation. Rotate the fingerprint fixture in `plugin/cursor/kernel/otp_signals.go` from a fresh Playwright capture and retry. |
| `pending: waiting for cursor magic-code email (last imap error: …)`                  | IMAP credentials are wrong or the mailbox has 2FA that blocks IMAP. Test the creds with an IMAP CLI (e.g. `openssl s_client -connect host:993`).                         |
| `error: magic-code POST did not lead to cursor.com/api/auth/callback?code=…`         | The code was probably wrong or already used. Restart the flow.                                                                                                           |

## Regenerating the fingerprint fixture

If Cursor rejects the current fingerprint (403 on the first POST), you
can capture a fresh one against a real login:

1. Open `authenticator.cursor.sh/password` in a Chromium browser.
2. Open DevTools → Network → filter for the POST to `/`.
3. Grab the `1_signals` form field, base64-decode it, and update
   `signalsFixture` in `plugin/cursor/kernel/otp_signals.go` with
   the new values. Keep `createdAtMs` / `submittedAtMs` out of the
   fixture — those are stamped per-request.

You can also update `otpUserAgent` in
`plugin/cursor/kernel/otp_flow.go` to match if the UA changed
significantly (e.g. Chrome major version bump). The UA in the flow
must match the UA in the signals blob.

## Test cost

Each end-to-end run consumes:

- ~2 YesCaptcha solves (one for `/`, one for `/magic-code`).
- 1 magic-code email quota with Cursor (they rate-limit these).

Budget one full flow per CI shard. For pipeline validation use the
unit tests, which mock every upstream and cost nothing.
