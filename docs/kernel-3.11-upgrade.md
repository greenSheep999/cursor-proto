# Kernel upgrade: 3.10.20 → 3.11.19

Captured 2026-07-14. This is the diff between the two Cursor lines and
the record of what changed in `cursor-proto` to keep the proxy working
against `api2.cursor.sh` after Cursor released 3.11.

## Cursor version anchors

|                     | 3.10.20                                                         | 3.11.19                                                         |
|---------------------|-----------------------------------------------------------------|-----------------------------------------------------------------|
| `x-cursor-client-version`  | `3.10.20`                                                | `3.11.19`                                                       |
| `x-cursor-client-commit`   | `23b9fb205fe595ea2be29da7214e19762d037fc0`               | `bf249e6efb5b097f23d7e21d7283429f0760b740`                      |
| `releaseHash` (checksum machineID) | `4071c661bcb367c518becc7b3d4d57cbd69d2291d8b302c558d79080f8fd4f75` | `bf249e6efb5b097f23d7e21d7283429f0760b74a` |
| `workbench.desktop.main.js` | 40.7 MB                                                 | 41.1 MB                                                         |

`releaseHash` derived from:

```bash
curl -s "https://api2.cursor.sh/updates/api/update/darwin-arm64/cursor/3.10.20/stable" | jq .url
# → https://downloads.cursor.com/production/bf249e6efb5b097f23d7e21d7283429f0760b74a/...
```

The URL's first path segment IS the releaseHash.

## Checksum algorithm — unchanged

Both `nVg` (main entry) and `tVg` (obfuscator) survived the rename:

| 3.10.20 | 3.11.19 |
|---------|---------|
| `nVg`   | `DZg`   |
| `tVg`   | `IZg`   |

Function bodies are **byte-identical** modulo minified variable names.
`auth/checksum.go` needed no algorithm changes — only the docstring
picked up a second version reference.

Verified by extracting both functions and diffing (see `captures/`):

```
function tVg(e){let t=165;for(let n=0;n<e.length;n++)e[n]=(e[n]^t)+n%256,t=e[n];return e}
function IZg(e){let t=165;for(let n=0;n<e.length;n++)e[n]=(e[n]^t)+n%256,t=e[n];return e}
```

Same `t=165`, same XOR-then-plus-index pattern, same `Uint8Array`
truncation. The 32-bit-signed `>>40` bug in the JS timestamp packing is
still there — our Go reproduction stays correct.

## Header field set — unchanged

`DZg` in 3.11 sets the same headers as `nVg` in 3.10, in the same order.
Only the internal symbol names of a couple of header-name constants
changed (`_` → `b` for `clientOs`, `E9t` → `l6t`, `poh` → `bdh`, etc.),
but the header names themselves and the field bindings are identical.

## Protobuf schema — additive only

Extracted schemas: `captures/schema-3.10.20.raw.json` (5,064 msg / 395
enum) vs `captures/schema-3.11.19.raw.json` (5,527 msg / 438 enum).

**Core RPC types (what `cursor-proxy` actually uses on the wire):**

| Message                                  | 3.10 fields | 3.11 fields | Delta                    |
|------------------------------------------|-------------|-------------|--------------------------|
| `agent.v1.AgentRunRequest`               | 23          | 23          | none                     |
| `agent.v1.AgentServerMessage`            | 6           | 6           | none                     |
| `agent.v1.ExecClientMessage`             | 41          | 42          | +field #53               |
| `agent.v1.RequestContext`                | 44          | 45          | +field #50               |
| `aiserver.v1.AvailableModelsRequest`     | 13          | 13          | none                     |
| `aiserver.v1.AvailableModelsResponse`    | 14          | 16          | +field #17, +field #18   |
| `aiserver.v1.ErrorDetails`               | 3           | 3           | none                     |

All deltas are new fields, no renames, no deletions. That means:

- Old wire data (from a 3.10 client) still deserializes cleanly in a
  3.11 decoder (new fields default to zero).
- New wire data (from a 3.11 client) still deserializes cleanly in a
  3.10 decoder (unknown fields are dropped on the floor).

In other words the two lines are wire-compatible; a `cursor-proxy`
built against 3.10 could — in theory — impersonate 3.11 at the wire
level if we only swapped the `x-cursor-client-version` header. We
regenerated the pb.go anyway so we get accurate field names for the
new fields and so IDE-specific features Cursor adds later don't fall
into an `Unknown` bucket.

**Bonus fix from the regen**: `agent.v1.AgentServerMessage` field #5
(`exec_server_control_message`) was unresolved-ref in the 3.10 extract
(fell back to `bytes`); in 3.11 it resolves to
`agent.v1.ExecServerControlMessage`. `cmd/test-rawstream/main.go` was
updated from `len(...) > 0` to `!= nil` accordingly.

**Also cross-namespace**: 3.11 added 489 new messages (mostly
`agent.v1.AgentHost*` and `agent.v1.ConversationSearch*` for new
in-IDE features) and dropped 26 messages (`aiserver.v1.AccountClosure*`,
`aiserver.v1.Sandbox*`). None of the drops are reachable from our
`CORE_ROOTS` closure in `scripts/gen_proto.py`, so they don't affect
the compiled `cursor.pb.go`.

## Usage protobuf (`usage/pb/cursor_usage.pb.go`)

Verified all types in this file exist in 3.11 with identical field
numbers. The one exception is `GetTeamMembershipInfoRequest/Response`,
which was renamed to `GetTeamMembersRequest/Response` in 3.11 — but
`cursor_usage.pb.go` doesn't reference the team-membership types, so
no code change needed. Only the docstring was updated.

## Files touched

- `executor/headers.go` — version + commit constants, docstring
- `auth/machineid.go` — added `KnownReleaseHash_3_11_19`, kept
  `KnownReleaseHash_3_10_20` for reference
- `auth/checksum.go` — docstring only (algorithm unchanged)
- `cmd/test-getme/main.go` — reference constants from `executor`
  instead of hardcoding
- `cmd/test-rawstream/main.go` — updated for `ExecServerControlMessage`
  now being a message type
- `usage/pb/cursor_usage.pb.go` — docstring only
- `scripts/gen_proto.py` — added `--schema` CLI arg so we don't have
  to edit `SCHEMA_PATH` next upgrade
- `proto/cursor.proto` — regenerated from `schema-3.11.19.raw.json`
- `gen/cursor/cursor.pb.go` — regenerated from `proto/cursor.proto`
- `docs/versioning.md` — release map bumped

## Smoke test

Ran `cursor-proxy` built with the 3.11.19 anchors against a real 3.11.19
Cursor account:

- `POST aiserver.v1.DashboardService/GetMe` → HTTP 200, correct account
  data (`test-getme`)
- `GET /v1/models` → HTTP 200, 12 models including new
  `cursor-grok-4.5-high-fast`, `composer-2.5-fast`
- `POST /v1/chat/completions` with `cursor-grok-4.5-low-fast` → HTTP 200,
  correct completion, usage metrics returned

Region-restricted models (e.g. `gpt-4` from a CN account) return the
usual `ERROR_UNSUPPORTED_REGION` — this is a Cursor policy, not a
protocol issue.

## How to redo this in future upgrades

1. `cp /Applications/Cursor.app/Contents/Resources/app/out/vs/workbench/workbench.desktop.main.js captures/wb-<ver>.js`
2. `cp /Applications/Cursor.app/Contents/Resources/app/product.json captures/product-<ver>.json`
3. `curl -s "https://api2.cursor.sh/updates/api/update/darwin-arm64/cursor/<prev-ver>/stable" | jq .url` → releaseHash
4. `grep -oE 'function [a-zA-Z]+\(e\)\{let t=165' captures/wb-<ver>.js` → find new obfuscator name; diff its body vs prior. If body diverged, `auth/checksum.go` needs work; else you're done with the algorithm.
5. `CURSOR_PROTO_WB=captures/wb-<ver>.js python3 scripts/extract_schema.py > captures/schema-<ver>.raw.json`
6. `python3 scripts/gen_proto.py --mode core --schema captures/schema-<ver>.raw.json`
7. `protoc --proto_path=proto --go_out=gen --go_opt=paths=source_relative proto/cursor.proto` (then `mv gen/cursor.pb.go gen/cursor/cursor.pb.go`)
8. Update `CursorClientVersion`, `CursorClientCommit`, `CursorReleaseHash`, and add new `KnownReleaseHash_*` constant.
9. `go build ./...` and `go test ./...`
10. Smoke-test with `go run ./cmd/test-getme` (HTTP 200 = headers pass) and a real `/v1/chat/completions` roundtrip.
11. Update `docs/versioning.md` release map, cut a `cursor<X.Y>/v<semver>` tag.
