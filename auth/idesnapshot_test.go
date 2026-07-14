package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSnapshotIDEDB_CopiesMainAndSiblings verifies the three-file copy
// behavior: main, -wal, -shm all land in the cache dir with correct
// contents, and subsequent calls short-circuit when source mtime hasn't
// advanced.
func TestSnapshotIDEDB_CopiesMainAndSiblings(t *testing.T) {
	// Route UserCacheDir at a per-test tmp to avoid stomping the user's
	// real cache. On macOS UserCacheDir reads $HOME; on Linux it reads
	// XDG_CACHE_HOME first.
	tmpCache := t.TempDir()
	t.Setenv("HOME", tmpCache)
	t.Setenv("XDG_CACHE_HOME", tmpCache)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "state.vscdb")
	mustWrite(t, src, "MAIN-v1")
	mustWrite(t, src+"-wal", "WAL-v1")
	mustWrite(t, src+"-shm", "SHM-v1")

	snap, err := SnapshotIDEDB(src)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if got := mustRead(t, snap); got != "MAIN-v1" {
		t.Errorf("main contents: %q", got)
	}
	if got := mustRead(t, snap+"-wal"); got != "WAL-v1" {
		t.Errorf("wal contents: %q", got)
	}
	if got := mustRead(t, snap+"-shm"); got != "SHM-v1" {
		t.Errorf("shm contents: %q", got)
	}

	// Second call with unchanged source: mtime stamp matches, no re-copy.
	// We assert this by recording the snapshot's inode-mtime *before* the
	// call and confirming it is unchanged after.
	pre, _ := os.Stat(snap)
	snap2, err := SnapshotIDEDB(src)
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}
	if snap2 != snap {
		t.Errorf("snapshot path changed: %q vs %q", snap2, snap)
	}
	post, _ := os.Stat(snap)
	if !post.ModTime().Equal(pre.ModTime()) {
		t.Errorf("snapshot re-copied when it shouldn't have: %v → %v",
			pre.ModTime(), post.ModTime())
	}
}

// TestSnapshotIDEDB_RefreshesOnSourceChange verifies that advancing the
// source mtime triggers a re-copy and the new contents land in the cache.
func TestSnapshotIDEDB_RefreshesOnSourceChange(t *testing.T) {
	tmpCache := t.TempDir()
	t.Setenv("HOME", tmpCache)
	t.Setenv("XDG_CACHE_HOME", tmpCache)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "state.vscdb")
	mustWrite(t, src, "OLD")

	snap, err := SnapshotIDEDB(src)
	if err != nil {
		t.Fatalf("snapshot 1: %v", err)
	}
	if got := mustRead(t, snap); got != "OLD" {
		t.Fatalf("initial: %q", got)
	}

	// Simulate the IDE writing an account switch: overwrite the source
	// and bump its mtime forward. The 2-second bump matters because
	// filesystem mtime resolution on some macOS versions is 1s.
	mustWrite(t, src, "NEW-v2")
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(src, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	snap2, err := SnapshotIDEDB(src)
	if err != nil {
		t.Fatalf("snapshot 2: %v", err)
	}
	if got := mustRead(t, snap2); got != "NEW-v2" {
		t.Errorf("post-refresh contents: %q", got)
	}
}

// TestSnapshotIDEDB_SourceMissingKeepsExistingSnapshot verifies the
// "source vanished mid-poll" resilience: if the IDE renamed/removed
// state.vscdb since our last snapshot, we return the last-known copy
// rather than error out and take the proxy offline.
func TestSnapshotIDEDB_SourceMissingKeepsExistingSnapshot(t *testing.T) {
	tmpCache := t.TempDir()
	t.Setenv("HOME", tmpCache)
	t.Setenv("XDG_CACHE_HOME", tmpCache)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "state.vscdb")
	mustWrite(t, src, "PRIMED")
	if _, err := SnapshotIDEDB(src); err != nil {
		t.Fatalf("prime: %v", err)
	}

	// Vanish.
	if err := os.Remove(src); err != nil {
		t.Fatalf("remove src: %v", err)
	}

	snap, err := SnapshotIDEDB(src)
	if err != nil {
		t.Fatalf("snapshot after source gone: %v", err)
	}
	if got := mustRead(t, snap); got != "PRIMED" {
		t.Errorf("expected previous snapshot to survive, got %q", got)
	}
}

// TestSnapshotIDEDB_OptionalSiblings verifies the -wal / -shm branches
// are skipped cleanly when the source doesn't have them, and that a
// later refresh drops any stale sibling from a prior generation.
func TestSnapshotIDEDB_OptionalSiblings(t *testing.T) {
	tmpCache := t.TempDir()
	t.Setenv("HOME", tmpCache)
	t.Setenv("XDG_CACHE_HOME", tmpCache)

	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "state.vscdb")
	// First generation has -wal.
	mustWrite(t, src, "MAIN")
	mustWrite(t, src+"-wal", "WAL-1")
	snap, err := SnapshotIDEDB(src)
	if err != nil {
		t.Fatalf("snap 1: %v", err)
	}
	if _, err := os.Stat(snap + "-wal"); err != nil {
		t.Errorf("expected -wal in first snapshot: %v", err)
	}

	// IDE checkpoints and removes -wal, then updates main.
	if err := os.Remove(src + "-wal"); err != nil {
		t.Fatalf("rm wal: %v", err)
	}
	mustWrite(t, src, "MAIN-v2")
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(src, future, future)

	if _, err := SnapshotIDEDB(src); err != nil {
		t.Fatalf("snap 2: %v", err)
	}
	if _, err := os.Stat(snap + "-wal"); !os.IsNotExist(err) {
		t.Errorf("stale -wal should have been removed: %v", err)
	}
}

func mustWrite(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
