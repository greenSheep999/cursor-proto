# Kernel upgrade: 3.11.19 → 3.15.19

Captured on 2026-08-13 after Cursor's built-in updater installed 3.15.19.

## Version anchors

| Anchor | Cursor 3.15.19 |
|---|---|
| client version | `3.15.19` |
| client commit | `de07bee81cefe43461ebf4f40c3d2d78d15052a0` |
| release hash | `de07bee81cefe43461ebf4f40c3d2d78d15052aa` |
| extracted messages | 6,064 |
| extracted enums | 502 |

The checksum obfuscator still uses the same seed-165 XOR/add-index
algorithm. Its minified symbol in 3.15.19 is `Bvg`.

## Core protocol changes

- `AgentRunRequest` adds computer-use coordinates, run/session IDs, and
  prompt-context usage capability fields.
- `AgentServerMessage` adds `ttft_breakdown`.
- `InteractionUpdate` adds `context_injection_state`.
- `AvailableModelsRequest` adds `byok_enabled`.
- `AvailableModelsResponse` removes the old non-max experiment fields and
  adds experimental-model and auto-optimization metadata.
- Agent store conflict handling and mini-swe-agent shell messages are added.

The extractor now accepts version and commit metadata through
`CURSOR_PROTO_VERSION` and `CURSOR_PROTO_COMMIT`. It can also inherit resolved
message types from the previous schema with `CURSOR_PROTO_BASE_SCHEMA`; this
handles unchanged fields whose generated symbols are no longer directly
bound after minification.

## Validation

- `go test ./...` passes.
- `DashboardService/GetMe` returns HTTP 200 for the active account.
- `AvailableModels` returns a valid model catalog.
- A real `claude-4.5-sonnet` request returns `OK` and a valid `turn_ended`
  usage event.

## Version monitoring

`scripts/check_cursor_version.sh` checks Cursor's stable update endpoint using
the installed version. A local LaunchAgent runs it every six hours and sends a
macOS notification once per newly observed version. It reports updates but
does not install them automatically, because Cursor must exit cleanly before
ShipIt can replace the application bundle.
