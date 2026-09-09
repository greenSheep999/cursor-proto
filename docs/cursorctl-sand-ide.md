# `cursorctl sand ide`

`cursorctl sand ide` is the local Cursor bundle patch entry point. It is
separate from the HTTP Sand dashboard client: the dashboard client claims and
reads allowance, while this command changes the Electron bundle so the IDE can
use the Sand client mode.

The command accepts an explicit `Cursor.app` path. The main operations are:

```text
cursorctl sand ide status --app /Applications/Cursor.app
cursorctl sand ide plan --app /Applications/Cursor.app
cursorctl sand ide provision-box
cursorctl sand ide setup-all --app /Applications/Cursor.app --restart
cursorctl sand ide install --app /Applications/Cursor.app
cursorctl sand ide uninstall --app /Applications/Cursor.app
```

`status` and `plan` are read-only. `install` refuses to proceed unless all
stream anchors are present; it never writes a partial patch. The default is to
leave Cursor stopped after a mutation. Pass `--restart` only when the bundle
has been checked and a restart is desired.

The embedded rules execute with Python 3 (`python3` by default); set
`CURSORCTL_PYTHON` when the interpreter is installed under a non-standard
name.

On macOS, `/Applications/Cursor.app` can reject writes even when the app is
owned by the logged-in user. In that case run the checked-in script once with
administrator privileges, keeping backups under the current user's home:

```sh
sudo env SAND_PATCH_CONFIG_DIR="$HOME/Library/Application Support/SandClientMode/sand-client-cli" \
  python3 /Users/danlio/Repositories/cursor-proto/sand/ide/sand_patch.py \
  --path /Applications/Cursor.app install
```

The same command with `uninstall` restores the saved original bytes.

Every installation stores the original bytes and SHA-256 values in the local
SandClientMode backup directory. Uninstall restores those bytes first and only
falls back to marker removal when no owned backup exists. This is important for
header expressions: reversing a marker alone can turn `v ?? "ide"` into a
different, fixed expression.

## 3.19.7 verification

The official 3.19.7 bundle was copied to a writable temporary app and tested
without touching `/Applications/Cursor.app`:

- `plan`: 83 scanned targets, 9 planned files, all stream anchors present.
- `install`: 23 client markers, 2 eligibility markers, `ide_matches=0`.
- `codesign --verify --deep --strict`: passed.
- `node --check`: passed for every JavaScript target.
- `uninstall`: 83/83 target SHA-256 values matched the pre-install snapshot.

The installed 3.19.13 bundle was tested the same way in a writable copy:

- `plan`: 65 scanned targets, 8 planned files, all stream anchors present.
- `install`: 23 client markers, 2 eligibility markers, `ide_matches=0`.
- 286 JavaScript files passed `node --check`; deep code-sign verification passed.
- `uninstall`: 65/65 target SHA-256 values matched the pre-install snapshot.

The 3.19.13 route uses a separate local-loop router anchor from 3.19.7. The
rule is version-aware and keeps the original route body behind the marker, so
the two bundle layouts do not share a brittle regular expression.

The current 3.19.19 bundle was also verified with relay contract v4:

- `plan`: 10 JavaScript targets plus `product.json`; both relay auth anchors
  and all 23 client-type sites matched.
- `install`: complete marker validation, 10/10 JavaScript syntax checks, deep
  strict code-sign verification, and a successful isolated Electron startup.
- `uninstall`: all 10 target hashes exactly matched the pre-install snapshot;
  code-sign verification still passed.
- Real Claude and Grok Stream probes returned text and only increased the Bot
  usage bucket. The v4 contract uses Sand client version `0.46.0`, strips
  `x-cursor-client-source`, config version, and client commit, and derives the
  checksum from the Box service machine identity.
