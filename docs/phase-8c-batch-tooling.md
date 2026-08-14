# Phase 8c — Batch account management tooling

Operators of cursor-proto (and by extension CLIProxyAPI's cursor
plugin) want to manage a **pool** of Cursor accounts rather than one at
a time. Phase 8c ships four CLI binaries plus a small `sdk/batch`
package that makes the pool workflow first-class.

## Deliverables

| Binary | Purpose |
| ------ | ------- |
| `cmd/cursor-login-batch`  | Interactive OAuth login for many emails back-to-back. |
| `cmd/cursor-batch-import` | Bulk-import existing tokens from CSV/JSON. |
| `cmd/cursor-to-cpa`       | Extended: `--dir-in <src> --dir <out>` converts a whole pool. |
| `cmd/cursor-from-cpa`     | Reverse of the above, so pools can round-trip. |
| `cmd/cursor-pool`         | Pool inspector: `status`, `verify`, `refresh`. |

Shared code lives under `sdk/batch/` (row parsing, account synthesis,
pool scanning, display helpers) and in two new helpers in `auth/`:

- `auth.JWTClaims` + `auth.DecodeJWTClaims` — extract `iat` / `exp`
  from an access-token JWT so imported rows land with accurate
  `issued_at` / `expires_at` even when the CSV doesn't spell them out.
- `auth.Account.Refreshable` + `RefreshLead` — persistent flags that
  let the pool inspector distinguish "temporarily unhealthy" from
  "will need manual re-auth on expiry".

## Data flow

```
    CSV / JSON                  cursor-login (single)
        |                              |
   cursor-batch-import         cursor-login-batch
        \                          /
         v                        v
       ~/.cursor-pool/*.json  (auth.Account shape)
             ^                       |
             |               cursor-to-cpa --dir-in
      cursor-from-cpa                 |
             |                        v
             +----- ~/.cli-proxy-api/*.json (CPA shape)
                     (loaded by plugin/cursor)
```

Both directions of the CPA converter round-trip **byte-for-byte** for
files produced by the batch tools; `cmd/cursor-from-cpa/roundtrip_test.go`
enforces this in CI.

## `cursor-login-batch`

```
cursor-login-batch --emails a@icloud.com,b@gmail.com --out ~/.cursor-pool/
cursor-login-batch --emails-file emails.txt --out ~/.cursor-pool/
cursor-login-batch --emails … --force        # overwrite existing files
cursor-login-batch --emails … --no-browser   # print URL, don't open a tab
```

- Reuses `auth.StartLogin` / `LoginSession.WaitForLogin` — no
  duplication of the OAuth flow.
- Skips accounts whose file already exists on disk unless `--force`.
- Continues past per-account failures; a final summary prints
  `X/Y succeeded, Z skipped, failed: [list]`.
- The test hook `CURSOR_POLL_URL_OVERRIDE` routes the poll endpoint at
  a mock server for integration testing (`scripts/phase-8c-verify.sh`
  drives this).

## `cursor-batch-import`

```
cursor-batch-import --csv tokens.csv --out ~/.cursor-pool/
cursor-batch-import --csv tokens.json --out ~/.cursor-pool/
cursor-batch-import --csv … --skip-validate   # don't call GetMe
```

- Accepts both `.csv` (header + rows, column order flexible) and
  `.json` (array of objects). Alternate names for fields are
  recognised: `access_token` / `accesstoken` / `token`, etc.
- For each row, machine identifiers are pulled from the local host so
  the batch is tied to whoever ran the import; downstream (CPA plugin,
  another cursor-proto host) can override at load time.
- `issued_at` and `expires_at` are decoded from the JWT payload when
  the token is a JWT; otherwise `issued_at = now`.
- Validation calls `DashboardService.GetMe` through the executor. If
  it fails and a refresh token is present, one retry runs. Rows
  without a refresh token skip validation and land with
  `refreshable: false` — the pool inspector surfaces those so
  operators know they need manual re-auth on expiry.
- `refresh_lead: 30m` is stamped onto every imported account (as a
  Go duration in the JSON), giving the future refresh loop a default
  headroom to work with.
- JWTs with `type=web` are rejected before Dashboard validation. Dashboard
  acceptance is not sufficient proof that a token can authenticate Cursor
  IDE AI requests; use the cookie-backed `cursor-login` flow documented in
  `web-session-login.md` to exchange them for `type=session` credentials.

## `cursor-to-cpa` (extended)

Existing single-file behaviour is preserved. New batch mode:

```
cursor-to-cpa --dir-in ~/.cursor-pool/ --dir ~/.cpa-pool/
```

- Walks `--dir-in` non-recursively.
- Each `*.json` is loaded as an `auth.Account`, converted via
  `cpaformat.FromAccount`, and written to `--dir` under
  `cursor-<sanitized_email>.json`.
- Operator knobs (`--prefix`, `--priority`, `--proxy-url`, …) apply
  to every converted file when in batch mode.

## `cursor-from-cpa` (new)

```
cursor-from-cpa --in ~/.cpa-pool/cursor-alice.json --out ~/.cursor-pool/
cursor-from-cpa --dir-in ~/.cpa-pool/ --out ~/.cursor-pool/
```

- Ignores non-cursor CPA files in batch mode (`type != "cursor"`) so
  a shared CPA auths directory can be pointed at.
- Round-trips byte-for-byte with the pool that produced it.

## `cursor-pool`

```
cursor-pool status  --dir ~/.cursor-pool/        # printed table
cursor-pool status  --dir ~/.cursor-pool/ --json # JSON for scripting
cursor-pool status  --dir ~/.cursor-pool/ --verify  # populates country/tier
cursor-pool verify  --dir ~/.cursor-pool/        # /GetMe against each
cursor-pool refresh --dir ~/.cursor-pool/        # verify + mtime bump
```

`status` columns:

```
EMAIL                            COUNTRY  TIER   EXP_IN    LAST_USE          NOTES
alice@example.com                CN       Pro    58d 12h   2h ago            slow_pool
bob@example.com                  US       Pro    12d 03h   never             (unused)
carol@example.com                CN       Free   expired   (unavailable)     needs refresh
```

- `EXP_IN` is derived from the account's `expires_at` field, falling
  back to the JWT `exp` claim.
- `LAST_USE` is derived from the pool file's mtime (touched by
  `cursor-pool refresh` and updated by the CPA plugin on every
  successful refresh).
- `TIER` is a heuristic bucket driven by whichever RPC succeeded when
  `--verify` (or `cursor-pool verify`) ran: presence of `hard_limit`
  or `limit` means Pro; only `me` succeeding means Free; otherwise `?`.

`refresh` currently does not perform a real token swap: the refresh
endpoint on Cursor's side isn't wired up in cursor-proto yet
(`plugin/cursor/handlers.go:handleAuthRefresh` is a documented
passthrough). Once a real refresh flow lands, `cursor-pool refresh`
is the seam that calls it — for now it verifies the account is
alive and bumps mtime.

## Interop with CPA (recap)

`cpaformat.CursorTokenStorage` grew two fields to make the imported
metadata survive the CPA round-trip:

```
refreshable    bool  // false => manual re-auth required on expiry
refresh_lead   int   // nanoseconds; consumed by future refresh loop
```

Existing CPA readers ignore unknown fields, so the addition is
backwards-compatible.

## Verification

`scripts/phase-8c-verify.sh` runs an end-to-end smoke of every new
binary:

1. Build all four binaries.
2. `cursor-batch-import` ingests a synthetic three-row CSV whose
   tokens are JWT-shaped but not real Cursor bearers, so `/GetMe` is
   skipped via `--skip-validate`.
3. `cursor-pool status` renders the table and JSON forms of the pool.
4. `cursor-to-cpa --dir-in` converts to CPA shape.
5. `cursor-from-cpa --dir-in` converts back.
6. `diff -r` proves the two pools are byte-identical.
7. `cursor-login-batch` runs against a local mock OAuth server
   (`CURSOR_POLL_URL_OVERRIDE`) and produces two synthetic accounts.
8. `cursor-pool status` renders the mock-login pool.

The captured output is checked in as `phase-8c-verify.log`.

## Unit tests

- `auth/jwt_test.go` — JWT payload decode, iat/exp extraction.
- `sdk/batch/batch_test.go` — CSV/JSON parsing, JWT-derived
  `AccountFromRow`, `refreshable` flagging.
- `cmd/cursor-from-cpa/roundtrip_test.go` — batch-import →
  cursor-to-cpa → cursor-from-cpa round-trip byte diff.
