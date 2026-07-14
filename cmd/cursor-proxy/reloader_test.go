package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// TestIDEAccountReloader_MtimeUnchangedReturnsNil is the hot path: the IDE
// hasn't touched the file since we last checked, so the reloader must be
// zero-cost (nil return, no sqlite read).
func TestIDEAccountReloader_MtimeUnchangedReturnsNil(t *testing.T) {
	dbPath := seedIDEDatabase(t, "old@example.com", "TOKEN_OLD")
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}

	reload := makeIDEAccountReloader(dbPath, info.ModTime())

	if got := reload(); got != nil {
		t.Fatalf("expected nil (mtime unchanged), got account with email=%q", got.Email)
	}
}

// TestIDEAccountReloader_MtimeAdvancedReturnsFreshAccount simulates the IDE
// writing a new account to state.vscdb. Advancing the file's mtime + rewriting
// the row must cause the reloader to return the new email/token.
func TestIDEAccountReloader_MtimeAdvancedReturnsFreshAccount(t *testing.T) {
	dbPath := seedIDEDatabase(t, "old@example.com", "TOKEN_OLD")
	info, err := os.Stat(dbPath)
	if err != nil {
		t.Fatalf("stat db: %v", err)
	}

	reload := makeIDEAccountReloader(dbPath, info.ModTime())

	// Overwrite the rows and bump mtime past `initial`.
	rewriteIDEDatabase(t, dbPath, "new@example.com", "TOKEN_NEW")
	future := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(dbPath, future, future); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := reload()
	if got == nil {
		t.Fatalf("expected fresh account, got nil")
	}
	if got.Email != "new@example.com" {
		t.Fatalf("wrong email: %q", got.Email)
	}
	if got.AccessToken != "TOKEN_NEW" {
		t.Fatalf("wrong token: %q", got.AccessToken)
	}

	// Second call with no further mtime change → nil again (idempotent).
	if again := reload(); again != nil {
		t.Fatalf("second call should be nil, got email=%q", again.Email)
	}
}

// TestIDEAccountReloader_MissingDBReturnsNil covers "IDE is being reset /
// wiped" — the file goes away for a moment. We must not crash and must
// keep the caller's cached account.
func TestIDEAccountReloader_MissingDBReturnsNil(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "no-such-file.vscdb")
	reload := makeIDEAccountReloader(dbPath, time.Time{})
	if got := reload(); got != nil {
		t.Fatalf("expected nil for missing db, got %+v", got)
	}
}

// seedIDEDatabase builds a state.vscdb clone in a tempdir with the two
// keys the IDE writes and cursor-proxy reads.
func seedIDEDatabase(t *testing.T, email, token string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := db.Exec(
		`INSERT INTO ItemTable(key, value) VALUES (?, ?), (?, ?)`,
		"cursorAuth/accessToken", token,
		"cursorAuth/cachedEmail", email,
	); err != nil {
		t.Fatalf("insert: %v", err)
	}
	return dbPath
}

func rewriteIDEDatabase(t *testing.T, dbPath, email, token string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`UPDATE ItemTable SET value = ? WHERE key = 'cursorAuth/accessToken'`,
		token,
	); err != nil {
		t.Fatalf("update token: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE ItemTable SET value = ? WHERE key = 'cursorAuth/cachedEmail'`,
		email,
	); err != nil {
		t.Fatalf("update email: %v", err)
	}
}
