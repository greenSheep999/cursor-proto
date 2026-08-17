# Versioning & Release Contract

This document is the **source of truth** for how `cursor-proto` publishes
`cursor-proxy` binaries and Docker images, and how downstream consumers
(currently `cursor2api`) should pin against them.

If you are the `cursor2api` maintainer: **§Consumer contract** below is what
you need. If you are working on `cursor-proto` itself: **§Producer rules**
tells you how to cut a release without breaking downstream.

---

## Why two axes

`cursor-proxy` impersonates a specific Cursor IDE version — its
`x-cursor-client-version`, `x-cursor-client-commit`, `x-cursor-checksum`
`releaseHash`, and the entire protobuf wire schema (`gen/cursor/*.pb.go`)
are all captured from **one specific Cursor build**. When Cursor releases a
new major/minor (3.10 → 3.11), those anchors change together, and the
resulting binary is a **different product**, not an upgrade.

But `cursor-proto` itself also evolves inside a Cursor version — bug fixes,
new proxy flags, new handler paths — and that's a normal `semver` axis.

So every published artifact has **two version dimensions**:

- **Cursor line**: which major.minor Cursor family we impersonate (e.g. `cursor3.10`, `cursor3.11`)
- **Proto version**: our own `semver` inside that Cursor line (e.g. `v0.1.1`)

Downstream needs both to decide what to ship.

### Line vs. impersonated version — don't confuse them

There are actually **three** version strings in play, and they answer
different questions:

| Question | String | Where it lives | Example |
|---|---|---|---|
| Which Cursor family does this binary target? | **Cursor line** (`major.minor`) | Tag prefix, artifact filename, cursor2api's `cursor_version_lock` field | `3.16` |
| Which exact Cursor build does it impersonate on the wire? | **Impersonated version** (`major.minor.patch`) | `x-cursor-client-version` header, `CursorClientVersion` constant | `3.16.17` |
| Which `cursor-proto` release is this binary? | **Proto version** (`v<semver>`) | Git tag suffix, `main.ProtoVersion` ldflag | `v0.4.0` |

**Why the tag stops at major.minor**: cursor2api's licence payload
stores `cursor_version_lock` as `"3.10"` (line), not `"3.10.20"`
(impersonated). Making the tag prefix `cursor3.10/*` lets cursor2api
match tag ↔ CDK lock with a bare string-prefix compare — no
version-parsing needed. And one binary usually covers an entire
`major.minor.*` family, because Cursor's protocol layer stays
stable across patch releases (only UI/bug-fix churn).

**When the impersonated patch bumps** (e.g. we re-capture at 3.10.25
and update the constants), we cut a new **proto** version on the same
line — `cursor3.10/v0.1.2` — and cursor2api's `cursor_version_lock`
field is untouched. Downstream just bumps `CURSOR_PROTO_TAG_3_10` in
its own release workflow. See `docs/kernel-3.11-upgrade.md` for a
worked example.

### How to read the values at runtime

Every published binary exposes all three strings, plus the release
hash, via:

```bash
# CLI — prints once and exits
cursor-proxy -version

# HTTP — same JSON, always reachable (bypasses -api-keys auth)
curl -s http://127.0.0.1:8317/v1/proxy-info
```

Response shape (stable, additive):

```json
{
  "cursor_line":           "3.16",
  "impersonated_version":  "3.16.17",
  "impersonated_commit":   "6b2afae0257df2bb5e1835f15165dc2f0de056b0",
  "release_hash":          "6b2afae0257df2bb5e1835f15165dc2f0de056b2",
  "proto_version":         "cursor3.16/v0.8.1"
}
```

`proto_version` reads `"dev"` for local `go build` / `go run`; release
CI stamps the git tag via `-ldflags="-X main.ProtoVersion=<tag>"`. Use
it to verify a running sidecar actually is the tag you pinned.

---

## Tag naming

**Format**: `cursor<major>.<minor>/v<semver>`

**Examples**:

```
cursor3.10/v0.1.0    # first release, Cursor 3.10.20
cursor3.10/v0.1.1    # patch on the 3.10 line — bug fix, new flag, whatever
cursor3.11/v0.2.0    # first release impersonating Cursor 3.11.x
cursor3.11/v0.2.1    # patch on the 3.11 line
cursor3.15/v0.4.0    # first release impersonating Cursor 3.15.x
cursor3.16/v0.7.0    # first release impersonating Cursor 3.16.x
```

**Rules**:

- `cursor<major>.<minor>` is fixed per branch. Never bump it in the tag
  without also being on the matching release branch.
- The `v<semver>` half follows normal semver **within** its Cursor line:
  - MAJOR — proto breaking change (rare; e.g. reworked handler URL scheme)
  - MINOR — new features on this Cursor line
  - PATCH — bug fixes only
- Do **not** reuse a `v<semver>` across Cursor lines. `v0.1.1` on the
  3.10 line and the 3.11 line refer to different codebases. Version
  numbers are line-scoped, not global.
- The legacy tag `v0.1.0` (flat, no prefix) is grandfathered and points
  to the same commit as `cursor3.10/v0.1.0`. Do not create new flat tags.

---

## Branch model

- `main` — always tracks the **latest supported Cursor version**. Right
  now that's `cursor3.16`; before that it was `cursor3.15`.
- `release/cursor-3.10`, `release/cursor-3.11`, … — long-lived
  maintenance branches, one per Cursor version we still ship. Only
  accepts bug fixes and safety patches; never rebased.
- Any tag matching `cursor<X.Y>/v*` **must** be on the corresponding
  `release/cursor-<X.Y>` branch (or on `main` if `<X.Y>` is the current
  latest). CI does not enforce this — reviewers do.

**Cutting a new Cursor line**:

1. From `main`, ensure everything is committed and passing.
2. `git checkout -b release/cursor-3.11` (branch off the last commit
   that still targets that Cursor version).
3. Push the branch: `git push -u origin release/cursor-3.11`.
4. On `main`, do the version bump: update `executor/headers.go`
   constants, `auth/machineid.go` release hash, regenerate protobuf,
   commit.
5. Tag the first new-line release on `main`: `git tag cursor3.16/v0.7.0 && git push --tags`.

**Patching an older line**:

1. `git checkout release/cursor-3.10`.
2. Cherry-pick the fix (or commit directly if it's line-specific).
3. `git tag cursor3.10/v0.1.2 && git push --tags`.

---

## Artifact naming

CI produces one binary per (Cursor version × OS × arch) tuple. Names:

```
cursor-proxy-cursor3.10-linux-amd64
cursor-proxy-cursor3.10-linux-arm64
cursor-proxy-cursor3.10-darwin-arm64
cursor-proxy-cursor3.10-darwin-amd64
cursor-proxy-cursor3.11-linux-amd64
cursor-proxy-cursor3.11-linux-arm64
cursor-proxy-cursor3.11-darwin-arm64
cursor-proxy-cursor3.11-darwin-amd64
```

Format: `cursor-proxy-<cursor-line>-<goos>-<goarch>`.

These names are **stable across proto versions**. `cursor3.10/v0.1.0` and
`cursor3.10/v0.1.5` both produce a file called
`cursor-proxy-cursor3.10-darwin-arm64` — the `v<semver>` is only in the
GitHub Release tag, not in the filename. Downstream pins the tag; the
filename doesn't need to encode what the tag already says.

---

## Docker image tags

Registry: `ghcr.io/<owner>/cursor-proxy`

Tag scheme (per Cursor version):

- `cursor3.10-v0.1.1` — exact release
- `cursor3.10-latest` — latest release on the 3.10 line
- `cursor3.11-v0.2.0` — exact release
- `cursor3.11-latest` — latest release on the 3.11 line
- `cursor3.15-v0.4.0` — exact release
- `cursor3.15-latest` — latest release on the 3.15 line
- `cursor3.16-v0.7.0` — exact release
- `cursor3.16-latest` — latest release on the 3.16 line
- `latest` — alias for the **current `main`** line's latest (moves when
  we cut a new Cursor version on `main`). **Do not pin to this in
  production**; use `cursor<X.Y>-latest` or a pinned semver.

Per-arch intermediate tags (e.g. `cursor3.10-v0.1.1-amd64`) exist for
build plumbing but are not part of the public contract.

---

## Consumer contract (for cursor2api and other downstream)

**To bundle a specific Cursor version**, pin the tag:

```yaml
env:
  CURSOR_PROTO_TAG_3_10: cursor3.10/v0.1.1
  CURSOR_PROTO_TAG_3_11: cursor3.11/v0.2.0
  CURSOR_PROTO_TAG_3_15: cursor3.15/v0.4.0
  CURSOR_PROTO_TAG_3_16: cursor3.16/v0.8.1
```

Download the matching artifact:

```bash
gh release download "${CURSOR_PROTO_TAG_3_10}" \
  --repo greenSheep999/cursor-proto \
  --pattern "cursor-proxy-cursor3.10-${TARGET}" \
  --output "$BIN"
```

**To bundle both** (so a single desktop app can serve `basic` users
locked to 3.10 and `plus`/`pro` users on 3.11), download both tags into
separate paths and let the desktop app's sidecar supervisor pick which
one to spawn based on `cursor_version_lock` from the license payload.
See cursor2api's `apps/desktop/src-tauri/src/sidecar.rs` and the license
gate's `cursor_version_lock` field.

**Version lock semantics** (mirrored from cursor2api's
`docs/cdk-contract.md`):

- `cursor_version_lock: "3.10"` — spawn `cursor-proxy-cursor3.10-*`
- `cursor_version_lock: "3.11"` — spawn `cursor-proxy-cursor3.11-*`
- `cursor_version_lock: "3.15"` — spawn `cursor-proxy-cursor3.15-*`
- `cursor_version_lock: "3.16"` — spawn `cursor-proxy-cursor3.16-*`
- `cursor_version_lock: null` — spawn the latest Cursor line bundled
  in this desktop app build (typically the highest `cursor<X.Y>` we
  ship)

**Runtime verification.** After spawning the sidecar, hit
`GET /v1/proxy-info` (unauthenticated) and cross-check the response's
`cursor_line` against the licence's `cursor_version_lock`, and
`proto_version` against the tag the desktop bundle was built against.
Fail loud on mismatch — it means the binary on disk doesn't match
what CI pinned, which is either a bad update, a bundle mismatch, or
someone dropped a manual binary in place. UI can show
`impersonated_version` (`"3.11.19"`) verbatim when you want a
user-facing "Locked to Cursor X" string.

---

## Current release map

Kept in sync with git tags. If this table drifts from `git tag -l`, the
tags win — please update this document.

| Cursor line | Latest tag         | Branch                 | Status  | Impersonates       |
|-------------|--------------------|------------------------|---------|--------------------|
| 3.10        | `cursor3.10/v0.1.6` (also legacy `v0.1.0`) | `release/cursor-3.10` | maintained (security patches only) | Cursor 3.10.20     |
| 3.11        | `cursor3.11/v0.3.8` | `release/cursor-3.11` | maintained | Cursor 3.11.19     |
| 3.15        | `cursor3.15/v0.6.7` | `release/cursor-3.15` | maintained | Cursor 3.15.19     |
| 3.16        | `cursor3.16/v0.8.0` | `main`                | current | Cursor 3.16.17     |

Note: `cursor3.11/v0.3.0` is the first release with **agent mode**
(the /v1/agents/* endpoint tree backed by @cursor/sdk). Wire mode
consumers who don't need agents can keep using `cursor3.11/v0.2.7`
and skip the ~80 MB Node.js runtime — see docs/sdk-integration.md
for the trade-off. Agent mode is NOT back-ported to the 3.10 line.

`main` currently tracks the 3.16 line. Older supported lines are maintained
on their matching `release/cursor-<X.Y>` branches.

---

## FAQ

**Q: What if a fix needs to land on both 3.10 and 3.11?**
A: Land it on `main` first, then cherry-pick to `release/cursor-3.10`.
Tag both lines independently (`cursor3.10/v0.1.2` and
`cursor3.11/v0.2.1`).

**Q: Do we ever delete old artifacts?**
A: No. Old GitHub Releases stay forever so downstream can reproduce old
builds. If a build is known-broken, mark it as pre-release and add a
release-note warning, but don't delete.

**Q: Can I skip cutting a `release/cursor-<X.Y>` branch if I'm sure no
one will ever need a patch on the old line?**
A: You can, but you'll regret it the first time you're wrong. Cost of
cutting the branch is ~30 seconds; cost of not having it when you need
it is a full retro-fix on a stale checkout. Cut the branch.

**Q: What if Cursor jumps 3.10 → 4.0 (major)?**
A: Same rules apply. Tags become `cursor4.0/v<semver>`, branch becomes
`release/cursor-4.0`. Nothing about the scheme changes.
