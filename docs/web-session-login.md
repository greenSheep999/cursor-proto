# Cursor website-session compatibility login

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
