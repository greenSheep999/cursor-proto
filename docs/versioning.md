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

- **Cursor version**: which Cursor IDE build we impersonate (e.g. `cursor3.10`, `cursor3.11`)
- **Proto version**: our own `semver` inside that Cursor version (e.g. `v0.1.1`)

Downstream needs both to decide what to ship.

---

## Tag naming

**Format**: `cursor<major>.<minor>/v<semver>`

**Examples**:

```
cursor3.10/v0.1.0    # first release, Cursor 3.10.20
cursor3.10/v0.1.1    # patch on the 3.10 line — bug fix, new flag, whatever
cursor3.11/v0.2.0    # first release impersonating Cursor 3.11.x
cursor3.11/v0.2.1    # patch on the 3.11 line
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
  now that's `cursor3.11` (once cut); before that it was `cursor3.10`.
- `release/cursor-3.10`, `release/cursor-3.11`, … — long-lived
  maintenance branches, one per Cursor version we still ship. Only
  accepts bug fixes and safety patches; never rebased.
- Any tag matching `cursor<X.Y>/v*` **must** be on the corresponding
  `release/cursor-<X.Y>` branch (or on `main` if `<X.Y>` is the current
  latest). CI does not enforce this — reviewers do.

**Cutting a new Cursor line**:

1. From `main`, ensure everything is committed and passing.
2. `git checkout -b release/cursor-3.10` (branch off the last commit
   that still targets that Cursor version).
3. Push the branch: `git push -u origin release/cursor-3.10`.
4. On `main`, do the version bump: update `executor/headers.go`
   constants, `auth/machineid.go` release hash, regenerate protobuf,
   commit.
5. Tag the first new-line release on `main`: `git tag cursor3.11/v0.2.0 && git push --tags`.

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
- `cursor_version_lock: null` — spawn the latest Cursor line bundled
  in this desktop app build (typically the highest `cursor<X.Y>` we
  ship)

---

## Current release map

Kept in sync with git tags. If this table drifts from `git tag -l`, the
tags win — please update this document.

| Cursor line | Latest tag        | Branch                 | Status  |
|-------------|-------------------|------------------------|---------|
| 3.10        | `cursor3.10/v0.1.0` (also `v0.1.0`) | `release/cursor-3.10` (planned) | maintained |
| 3.11        | *(not yet cut)*   | `main`                 | in-progress |

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
