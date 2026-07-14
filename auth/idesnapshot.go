package auth

// SnapshotIDEDB copies Cursor IDE's `state.vscdb` (plus its `-wal` and
// `-shm` siblings if present) into cursor-proxy's user cache directory
// and returns the path to the snapshot's main file. All downstream sqlite
// opens must target the snapshot, never the source.
//
// Why: cursor-proxy's IDE bootstrap opens `state.vscdb` with
// `file:...?mode=ro` on every account reload. Read-only opens still show
// up in `lsof`, still touch `-shm`, and still race the IDE's own writer
// during in-place updates. The result is the class of "Cursor IDE cannot
// update while cursor2api is running" bugs reported downstream (see
// docs/upstream-issues/state-vscdb-copy-on-read.md). Reading a private
// snapshot instead of the live file makes the IDE's updater and Sparkle
// scans oblivious to our presence.
//
// Design points (mirroring the design doc):
//
//  1. Atomic 3-file copy: {main, -wal, -shm} each land in a `.tmp` file
//     and are renamed into place in one pass, so a reader mid-open never
//     sees a torn WAL/main pair.
//  2. Idempotent: `SnapshotIDEDB` is safe to call at boot AND on every
//     mtime bump. It skips the copy when the source mtime matches the
//     recorded snapshot mtime (stored via `Chtimes` on the copy).
//  3. Zero coupling to the IDE's lifecycle. If the source vanishes
//     mid-copy we return the previous snapshot (if any) with a warning.
//  4. Cache location: `os.UserCacheDir()/cursor-proxy/`. On macOS this
//     is `~/Library/Caches/cursor-proxy/`; on Linux it follows XDG.

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// snapshotFileName is the fixed basename we use inside the cache dir. Kept
// short and predictable so ops can inspect it easily.
const snapshotFileName = "state-snapshot.vscdb"

// SnapshotIDEDB copies `src` (Cursor IDE's `state.vscdb`) into cursor-
// proxy's user cache directory alongside its `-wal` / `-shm` siblings,
// and returns the path to the snapshot's main file. On repeat calls it
// only re-copies when the source mtime has advanced past the snapshot
// mtime, so callers can invoke it unconditionally from a poll loop.
func SnapshotIDEDB(src string) (string, error) {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve user cache dir: %w", err)
	}
	dir := filepath.Join(cacheDir, "cursor-proxy")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create snapshot dir %s: %w", dir, err)
	}
	dst := filepath.Join(dir, snapshotFileName)

	srcInfo, err := os.Stat(src)
	if err != nil {
		// Source missing: reuse the previous snapshot if we have one so
		// a transient rename doesn't take the proxy offline.
		if _, statErr := os.Stat(dst); statErr == nil {
			return dst, nil
		}
		return "", fmt.Errorf("stat source %s: %w", src, err)
	}

	if dstInfo, err := os.Stat(dst); err == nil {
		// Fast path: source mtime already matches the snapshot's
		// stamped mtime (we set snapshot mtime = source mtime after
		// each successful copy, see os.Chtimes below). Equality — not
		// "source after dst" — is the right test: an IDE that
		// overwrites state.vscdb with the SAME mtime as a prior write
		// (rare, but possible after account switches or clock skew)
		// would otherwise get silently ignored. Equal-mtime hits the
		// fast path; any difference at all forces a re-copy.
		if srcInfo.ModTime().Equal(dstInfo.ModTime()) {
			return dst, nil
		}
	}

	// Copy each of the three files under a `.tmp` suffix, then rename
	// them into place in reverse order (main last). This ordering means
	// a concurrent reader either sees the old triple (main still points
	// at old {-wal, -shm}) or the new triple — never a mismatched pair
	// where the main file references WAL frames not yet on disk.
	tmpMain := dst + ".tmp"
	tmpWAL := dst + "-wal.tmp"
	tmpSHM := dst + "-shm.tmp"

	// Best-effort clean of any leftover tmp files from a previous crash.
	for _, p := range []string{tmpMain, tmpWAL, tmpSHM} {
		_ = os.Remove(p)
	}

	if err := copyFile(src, tmpMain); err != nil {
		return "", fmt.Errorf("copy main: %w", err)
	}
	// -wal / -shm are optional; the IDE only writes them when journal
	// mode is WAL (the VSCode default since ~1.60) and there's an open
	// writer. Missing siblings are legitimate — just skip them.
	walCopied, shmCopied := false, false
	if _, err := os.Stat(src + "-wal"); err == nil {
		if err := copyFile(src+"-wal", tmpWAL); err != nil {
			_ = os.Remove(tmpMain)
			return "", fmt.Errorf("copy -wal: %w", err)
		}
		walCopied = true
	}
	if _, err := os.Stat(src + "-shm"); err == nil {
		if err := copyFile(src+"-shm", tmpSHM); err != nil {
			_ = os.Remove(tmpMain)
			if walCopied {
				_ = os.Remove(tmpWAL)
			}
			return "", fmt.Errorf("copy -shm: %w", err)
		}
		shmCopied = true
	}

	// Rename order: siblings first, main last. Rename is atomic on the
	// same filesystem, which the cache directory always is (we never
	// cross a mount boundary between $HOME/Library/Caches and the tmp
	// files in the same dir).
	if walCopied {
		if err := os.Rename(tmpWAL, dst+"-wal"); err != nil {
			return "", fmt.Errorf("rename -wal: %w", err)
		}
	} else {
		// Source lost its -wal since last snapshot: drop stale copy.
		_ = os.Remove(dst + "-wal")
	}
	if shmCopied {
		if err := os.Rename(tmpSHM, dst+"-shm"); err != nil {
			return "", fmt.Errorf("rename -shm: %w", err)
		}
	} else {
		_ = os.Remove(dst + "-shm")
	}
	if err := os.Rename(tmpMain, dst); err != nil {
		return "", fmt.Errorf("rename main: %w", err)
	}

	// Stamp the snapshot's mtime to match the source so the next call
	// short-circuits until the source moves forward again. Using the
	// source's mtime (not "now") means we track upstream change events,
	// not our own copy-completed events.
	_ = os.Chtimes(dst, srcInfo.ModTime(), srcInfo.ModTime())
	return dst, nil
}

// copyFile writes src's bytes to dst, replacing any existing file at dst.
// Not atomic on its own — SnapshotIDEDB atomicizes across the three-file
// set by staging into `.tmp` and renaming.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
