# Plugin quota slot — design proposal

**Status**: draft for review
**Scope**: CPA management center (`router-for-me/Cli-Proxy-API-Management-Center`)
  + any plugin that wants a card on the `/quota` page (starting with our
  cursor + kiro plugins).
**Non-goal**: replacing the existing built-in provider grids (Claude,
  Codex, Antigravity, xAI, Kimi). Those stay as-is.

## Motivation

The `/quota` page today lists five providers, each mounted as one
`<QuotaSection config={XXX_CONFIG}>` line in `QuotaPage.tsx`. Every
`XXX_CONFIG` is a hand-written `QuotaConfig<TState, TData>` in
`quotaConfigs.ts` — filter fn, fetch fn, render fn, error-state fn,
etc. all live in CPA-frontend source.

For a plugin like `cursor` or `kiro` to show up on that page, one of
these has to be true:

1. Its config lives in CPA-frontend source (hardcoded — the current
   pattern). New plugin → PR to CPA → wait for release. Merge
   conflicts every rebase, downstream forks diverge fast.
2. CPA-frontend has a **generic plugin-quota slot** the plugin fills
   over the wire. New plugin → declare a manifest → done. This
   proposal is that.

Kiro today works around this by exposing its own separate page
(`quota_page.go`) — the comment there explicitly says a shared slot
mechanism would be preferable. This proposal is that mechanism.

## Key design choices

### 1. Wire-only contract, no plugin-shipped JS

Alternative considered: plugin ships a compiled React widget the
frontend loads dynamically. Rejected — XSS surface, version-skew,
sandboxing, SDK maintenance. Every Kubernetes-dashboard-plugin
project regrets this.

The wire contract is **JSON**. CPA-frontend has ONE renderer that
turns any conformant JSON blob into a grid of cards visually
consistent with Claude/Codex/etc.

### 2. Reuse the existing QuotaConfig factory

Do not add a new `<PluginQuotaSection>` component. Instead, add a
factory `buildPluginQuotaConfig(manifest)` that returns a
`QuotaConfig<PluginQuotaState, PluginQuotaData>`. The existing
`<QuotaSection>` renders it. This keeps the layout + pagination +
"Reset" button + error-state UX identical to built-in providers —
one fewer thing that can drift.

### 3. Slot discovery happens once at page load

The frontend already fetches `authStore.plugins` from
`/v0/management/plugins` to render the sidebar. That same list is
the source of truth for quota slots: any plugin whose manifest
declares `capabilities.quota_slot != null` gets a `<QuotaSection>`
appended to `<QuotaPage>` after the built-in ones.

## Wire contract

### Plugin manifest addition (backward compatible)

CPA's plugin registration already returns metadata (Name, Logo,
Description, ConfigFields, Routes, Resources). Add one optional
top-level field:

```json
{
  "quota_slot": {
    "id": "cursor",
    "title_i18n_key": "quota.cursor.title",
    "title_fallback": "Cursor accounts",
    "description_i18n_key": "quota.cursor.description",
    "description_fallback": "Per-account plan + windowed spend + token breakdown, refreshed from Cursor's usage API.",
    "data_path": "/v0/management/cli-proxy-api/cursor/quota-rows",
    "reset_path": null,
    "columns": [
      { "key": "email",           "label": "Account",          "type": "identifier" },
      { "key": "plan",            "label": "Plan",             "type": "tag" },
      { "key": "country",         "label": "Region",           "type": "tag" },
      { "key": "spend_cents",     "label": "Spend (period)",   "type": "cents" },
      { "key": "limit_cents",     "label": "Limit",            "type": "cents" },
      { "key": "total_percent",   "label": "Total used",       "type": "percent_bar" },
      { "key": "auto_percent",    "label": "Auto+Composer",    "type": "percent_bar" },
      { "key": "api_percent",     "label": "API",              "type": "percent_bar" },
      { "key": "reset_at",        "label": "Reset",            "type": "iso8601" },
      { "key": "cache_read_7d",   "label": "Cache-read 7d",    "type": "int",  "hint": "tokens" }
    ],
    "row_actions": [
      { "id": "probe",   "label": "Refresh",         "method": "POST", "path": "/v0/management/cli-proxy-api/cursor/account/probe", "email_param": "email" },
      { "id": "events",  "label": "Per-request log", "method": "GET",  "path": "/v0/management/cli-proxy-api/cursor/account/events", "email_param": "email", "target": "modal" }
    ]
  }
}
```

**Missing = no quota slot.** Plugins that don't want to appear on
`/quota` simply omit `quota_slot`. Fully backward compatible.

### Data endpoint response shape

The frontend GETs `quota_slot.data_path` and expects:

```json
{
  "rows": [
    {
      "id":            "tinsels_boxy.5l@icloud.com",
      "email":         "tinsels_boxy.5l@icloud.com",
      "plan":          "Pro",
      "country":       "DK",
      "spend_cents":   5701,
      "limit_cents":   2000,
      "total_percent": 16.52,
      "auto_percent":  2.34,
      "api_percent":   100,
      "reset_at":      "2026-08-01T00:00:00Z",
      "cache_read_7d": 15186747,
      "_status":       "ok"
    }
  ],
  "fetched_at": "2026-07-16T14:38:39Z",
  "count": 1
}
```

Column `key`s in the manifest map 1:1 to fields in each `row`. Types
listed below (see § Column type registry) decide how the cell is
rendered — cents get formatted with a currency symbol, `percent_bar`
gets a coloured progress bar, `iso8601` gets relative-time.

The `_status` field per row is reserved: `"ok"` renders normally,
`"loading"` shows a spinner, `"error"` renders an error state with
optional `_error` + `_error_status` sibling fields. This lets a
plugin mark one account as broken without failing the whole card.

### Column type registry (v1)

| type            | Cell renderer                                                 |
|-----------------|----------------------------------------------------------------|
| `identifier`    | Monospaced, highlighted; usually first column.                 |
| `tag`           | Coloured pill ("Pro", "Free", "Team", country codes).          |
| `int`           | Right-aligned; commas at thousands; optional `hint` suffix.    |
| `cents`         | `$xx.yy` (locale-aware).                                       |
| `percent`       | `xx.y%` inline number.                                         |
| `percent_bar`   | Full progress bar with `xx.y%` label — matches Antigravity.    |
| `iso8601`       | Relative + absolute tooltip ("in 15 days · Aug 1, 2026").      |
| `boolean`       | Check / cross icon.                                            |
| `enum`          | Pill with per-value colour; needs `values: {a: "#..."}` in col.|
| `text`          | Wrapping plain text.                                           |

Frontend v1 ships these ten. Unknown types render as `text` with a
console warning. Adding a new type is a frontend PR — but a plugin
sticking to the ten never needs one.

### Row actions

Each row optionally gets buttons declared under `row_actions`. On
click, the frontend issues the declared HTTP request, substituting
`{email_param}` from the row into the query string (or into the path
if `path` contains `{email}`). `target: "modal"` opens the response
in a modal with syntax-highlighted JSON — useful for
per-request-log endpoints without shipping a dedicated modal.

### i18n

All human-readable labels come in **both** `..._i18n_key` and
`..._fallback` shapes. CPA-frontend prefers the i18n key if it
resolves in the loaded locale; otherwise falls back. Plugins ship
their fallbacks in English + optionally a `translations` field
holding `{ "zh-CN": { "quota.cursor.title": "Cursor 账号" } }`
which the frontend merges into i18next on plugin load.

## Frontend implementation

Total new code in CPA-frontend:

1. `src/components/quota/PluginQuotaSection.tsx` — reads the
   manifest slot from `pluginsStore`, calls `buildPluginQuotaConfig`,
   passes to existing `<QuotaSection>`.
2. `src/components/quota/pluginQuotaConfig.ts` — factory:
   `buildPluginQuotaConfig(slot: QuotaSlotManifest, t: TFunction): QuotaConfig<PluginQuotaState, PluginQuotaData>`.
3. `src/components/quota/PluginQuotaCell.tsx` — renders one cell by
   `column.type` (10 branches from the type registry).
4. `src/pages/QuotaPage.tsx` — one new block after the last built-in
   `<QuotaSection>`:
   ```tsx
   {pluginQuotaSlots.map(slot => (
     <PluginQuotaSection key={slot.id} slot={slot} disabled={disableControls} />
   ))}
   ```
5. `src/types/quota.ts` — `QuotaSlotManifest`, `PluginQuotaRow`,
   `PluginQuotaState`.
6. `src/stores/plugins.ts` — pluginQuotaSlots selector (already has
   `plugins` state).

Existing `QuotaType` union widens from
`'antigravity' | 'claude' | ...` to `... | \`plugin:${string}\``.
Everything else in `quotaConfigs.ts` stays untouched.

## Plugin implementation (Cursor example)

Plugin-side changes are additive:

1. In `plugin/cursor/kernel/management.go` `managementRegisterResult()`,
   add a top-level `quota_slot` field to the returned JSON.
2. New handler `handleQuotaRows(ctx)` at
   `GET /v0/management/cli-proxy-api/cursor/quota-rows` — reuses
   `globalRegistry.List()` + `Status(...)` (already there), projects
   `AccountStatus` into the wire shape above.

That's it. Estimated ~120 lines Go including the handler + tests.

Kiro's equivalent is the same shape — its `kiroQuotaRow` type already
matches the wire contract; just move it into a `/quota-rows`
handler and add a `quota_slot` field.

## Backward compatibility

- Plugins without `quota_slot` in their manifest behave identically
  to today. Zero effect on Claude/Codex/Antigravity/xAI/Kimi builds.
- CPA-frontend without this proposal ignores the new `quota_slot`
  field (unknown property) — a plugin can ship it early, it just
  won't appear until the frontend upgrades. No coordination
  required.
- Kiro's existing `/kiro/quota` HTML page keeps working. When the
  slot is available, Kiro can either serve both (transitional) or
  drop the HTML page — its choice.

## Migration path

1. Cursor plugin ships `quota_slot` + `/quota-rows` handler.
   Deployed CPA ignores it (harmless).
2. CPA-frontend PR: add `PluginQuotaSection` + factory + cell
   renderer + one line in `QuotaPage.tsx`. Merges cleanly (only
   additions).
3. Users update CPA to that release → both plugins' cards appear.
4. Kiro migrates its custom quota page to the slot, delete
   `quota_page.go`.

Each step is independently valuable; no big-bang.

## Open questions

1. **Auth**: `/v0/management/*/quota-rows` inherits management-key
   auth (Authorization: Bearer). Confirmed compatible with the
   panel's existing `authHeaders()` call. Any endpoint the plugin
   registers under `/v0/management/` is auth-gated by CPA by default,
   so we don't need to invent new middleware.
2. **Reset button**: does the CPA-side Reset action reuse a plugin
   endpoint? Proposed: `quota_slot.reset_path` — POST with per-row
   body `{ ids: [...] }`. Only shown when non-null. Cursor doesn't
   need it (no server-side reset action); Kiro might.
3. **Refresh interval**: Card refreshes on manual "Refresh All" +
   on `useHeaderRefresh` (the existing hook the built-in grids
   already subscribe to). No polling → no extra CPA load.
4. **Localisation of column labels**: currently column `label` is
   a string. Add optional `label_i18n_key` sibling in v1.1 if we
   need Chinese labels; string fallback works day-1.

## What I'm asking you

- Does the wire contract look right? Column types cover Kiro +
  Cursor without needing custom renderers?
- Are you OK with putting the CPA-frontend change upstream as a PR,
  or do you want to keep a fork? Proposal is drafted to be
  upstream-friendly so the burden of maintaining a fork is optional,
  not required.
- Should i18n keys be flat (`quota.cursor.title`) or nested
  (`quota.plugins.cursor.title`)? Existing built-ins use the flat
  pattern.
- Any objection to widening `QuotaType` to allow `plugin:<id>`
  strings, vs. adding a separate `PluginQuotaType` alias?
