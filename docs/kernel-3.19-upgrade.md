# Kernel upgrade: 3.16.17 → 3.17.21 / 3.18.9 / 3.19.7

Three Cursor lines were captured in one pass on 2026-09-05. `main` moved to
3.19.7; 3.17 and 3.18 got their own `release/cursor-3.1{7,8}` branches per the
contract in [versioning.md](versioning.md).

## Version anchors

| Line | Impersonated | `CursorClientCommit` | `releaseHash` |
|---|---|---|---|
| 3.16 (was) | 3.16.17 | `6b2afae…056b0` | `6b2afae…056b2` |
| 3.17 | 3.17.21 | `8f2a112cb2845a97b75fd932ea5c470579ca4060` | `8f2a112cb2845a97b75fd932ea5c470579ca4063` |
| 3.18 | 3.18.9 | `2ba48ff3f7514cc4643c52ca9f7b3173d9b66130` | `2ba48ff3f7514cc4643c52ca9f7b3173d9b66137` |
| 3.19 (main) | 3.19.7 | `90de2327392570a5f5f625c656c6749d228e6430` | `90de2327392570a5f5f625c656c6749d228e6437` |

Note the pattern that holds across every line: `releaseHash` and the
`product.json` commit differ only in the final character. They are separate
values — do not derive one from the other.

### Where the bundles came from

The update endpoint only ever returns the newest build, so it cannot be used to
fetch an intermediate line:

```bash
curl -s "https://api2.cursor.sh/updates/api/update/darwin-arm64/cursor/3.16.17/stable"
# → 3.19.7, regardless of which older version you ask from
```

- **3.19.7** — captured from the locally installed `/Applications/Cursor.app`.
- **3.18.9** — from a DMG already on disk.
- **3.17.21** — downloaded by release hash, which is the only reliable way to
  reach a superseded build:

  ```bash
  curl -L -o Cursor-3.17.21-darwin-universal.dmg \
    "https://downloads.cursor.com/production/8f2a112cb2845a97b75fd932ea5c470579ca4063/darwin/universal/Cursor-darwin-universal.dmg"
  ```

  Release hashes for past versions come from the community-maintained
  [oslook/cursor-ai-downloads](https://github.com/oslook/cursor-ai-downloads)
  `version-history.json`.

## Checksum algorithm — unchanged

The obfuscated builder is renamed every release but its body is byte-identical
after normalising its own name away:

| Version | Builder |
|---|---|
| 3.15.19 | `Bvg` |
| 3.17.21 | `gGg` |
| 3.18.9 | `TJg` |
| 3.19.7 | `$ff` |

`auth/checksum.go` needed no work. Note 3.19.7's builder is itself
`$`-prefixed — the same minifier trend that broke schema extraction below.

## The extractor bug this upgrade exposed

`scripts/extract_schema.py` silently produced a degraded schema. `build_ref_map`
matched bindings with:

```python
r'\b([A-Za-z_$][\w$]*)\s*=\s*[A-Za-z_$][\w$]*\.make(?:MessageType|Enum)\(\s*"([a-zA-Z_][\w.]*)"'
```

`\b` is a word-boundary assertion and `$` is not a word character, so
`\b\$Bt` cannot match when the binding follows `,` or `;` — nearly always, in
minified output. Every `$`-prefixed `makeMessageType` binding was dropped from
the ref map, and each field referencing one lost its `T_name` and was emitted as
`bytes`.

Cursor's minifier uses more `$` names each release, so the damage escalated:

| Version | Missed bindings | Result |
|---|---|---|
| 3.15.19 | 3 | harmless — none in the core closure |
| 3.17.21 | 126 | **build break** |
| 3.18.9 | ~84 field refs | would have broken the same way |
| 3.19.7 | ~197 field refs | would have broken the same way |

At 3.17.21 it surfaced as:

```
executor/chat.go:536: started.GetToolCall undefined (type []byte has no field or method GetToolCall)
executor/chat_build.go:267: undefined: cursorpb.AgentV1_ConversationHistoryToolResultContent
```

`agent.v1.ToolCallStartedUpdate` is bound as `$Bt` in 3.17.21, so
`InteractionUpdate.tool_call_started` degraded to `bytes`.

Removing `\b` fixes it — unresolved `$`-refs drop to 0 on every captured
version. The ~250 unresolved refs that remain per version are the pre-existing
baseline (JSON structs, timestamps) that 3.15.19 already had.

**If a future upgrade fails to compile on a type that plainly exists in the
IDE bundle, check the unresolved-ref count before suspecting a protocol
change:**

```bash
python3 - <<'PY'
import json
d=json.load(open("captures/schema-<ver>.raw.json"))
miss=[(n,f.get("name"),f.get("T_ref")) for n,fs in d["messages"].items() for f in fs
      if isinstance(f,dict) and f.get("kind")=="message" and "T_ref" in f and "T_name" not in f]
print(len(miss),"unresolved"); print(miss[:10])
PY
```

## Wire compatibility

The raw schema is **not** purely additive at any of these versions — unlike the
3.11 upgrade. It drops 44–46 messages (`Crew*`, some BugBot and `origin.v1`
types) and retypes one field
(`GetTeamAdminSettingsResponse` field 62, `dot_onboarding_completed` →
`sand_onboarding`).

None of that is reachable from `CORE_ROOTS`. What matters is the generated
closure, and inside it nothing is removed and no field changes type:

| Version | Closure messages | Enums |
|---|---|---|
| 3.15.19 | 900 | 64 |
| 3.17.21 | 948 | 71 |
| 3.18.9 | 948 | 71 |
| 3.19.7 | 949 | 71 |

Closure growth is partly the `$`-ref fix resolving previously-broken references
into real types, not new protocol surface. Check the closure — not the raw
schema — when judging whether an upgrade is wire-safe:

```python
import json, importlib.util
spec = importlib.util.spec_from_file_location("gp", "scripts/gen_proto.py")
gp = importlib.util.module_from_spec(spec); spec.loader.exec_module(gp)
d = json.load(open("captures/schema-<ver>.raw.json"))
c, e = gp.transitive_closure(d["messages"], d["enums"], gp.CORE_ROOTS)
```

## Files touched

- `executor/headers.go` — `CursorClientVersion`, `CursorClientCommit`, `CursorReleaseHash`
- `auth/machineid.go` — new `KnownReleaseHash_*` constant + `KnownReleaseHashFor` case
- `proto/cursor.proto`, `gen/cursor/cursor.pb.go` — regenerated
- `scripts/extract_schema.py` — the `\b` fix (cherry-picked onto all three branches)
- `captures/product-<ver>.json`, `captures/schema-<ver>.raw.json`
- `docs/versioning.md`, `README.md` — release map and current-line references

`captures/wb-*.js` is no longer tracked; the gitignore rule that was supposed
to exclude it had an inline comment appended to the pattern, so it matched
nothing and three 39 MB bundles were committed. Fixed separately.

## Verification status

Done on all three branches:

- `go build ./...`, `go vet ./...`, `gofmt -l .` clean
- `go test ./...` — 14 packages, 0 failures
- `cursor-proxy -version` reports the right `cursor_line`,
  `impersonated_version`, `impersonated_commit`, `release_hash`

**Not done** — playbook step 10, a live roundtrip against a real account
(`go run ./cmd/test-getme` for headers, then a real `/v1/chat/completions`).
Until that runs, these three lines are verified at the build/schema level only.
A wrong `releaseHash` or commit still compiles and still passes unit tests; it
fails at the checksum handshake against the live server. Run it before tagging
any of these.

## Redoing this

The 11-step procedure in
[kernel-3.11-upgrade.md](kernel-3.11-upgrade.md#how-to-redo-this-in-future-upgrades)
still applies. Two additions from this pass:

- `protoc-gen-go` must match `go.mod`'s `google.golang.org/protobuf`
  (v1.36.11 here): `go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.11`.
- After extracting the schema, check the unresolved-ref count and confirm no
  `$`-prefixed refs remain before trusting the generated Go.
