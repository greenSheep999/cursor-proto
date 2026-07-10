package main

import (
	"database/sql"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// seedIDEState writes a minimal state.vscdb that mirrors the schema
// Cursor IDE uses (ItemTable key/value). Only the keys the exporter
// reads are populated.
func seedIDEState(t *testing.T, entries map[string]string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for k, v := range entries {
		if _, err := db.Exec(`INSERT INTO ItemTable(key, value) VALUES (?, ?)`, k, v); err != nil {
			t.Fatalf("insert %s: %v", k, err)
		}
	}
	return dbPath
}

func TestLoadAccountFromIDE(t *testing.T) {
	db := seedIDEState(t, map[string]string{
		"cursorAuth/accessToken":      "access-abc",
		"cursorAuth/refreshToken":     "refresh-xyz",
		"cursorAuth/cachedEmail":      "someone@example.com",
		"cursorAuth/cachedSignUpType": "Auth_0",
		"cursorAuth/cachedUserID":     "user_01ABC",
		"cursorAuth/authId":           "auth0|user_01ABC",
	})
	acc, err := loadAccountFromIDE(db)
	if err != nil {
		t.Fatalf("loadAccountFromIDE: %v", err)
	}
	if acc.AccessToken != "access-abc" || acc.RefreshToken != "refresh-xyz" {
		t.Fatalf("token mismatch: %+v", acc)
	}
	if acc.Email != "someone@example.com" || acc.AuthType != "Auth_0" {
		t.Fatalf("email/authType mismatch: %+v", acc)
	}
	if !acc.Refreshable {
		t.Fatalf("Refreshable should be true when refresh token is present")
	}
	if acc.IssuedAt.IsZero() {
		t.Fatalf("IssuedAt not populated")
	}
}

func TestLoadAccountFromIDE_MissingToken(t *testing.T) {
	db := seedIDEState(t, map[string]string{
		"cursorAuth/cachedEmail": "empty@example.com",
	})
	if _, err := loadAccountFromIDE(db); err == nil {
		t.Fatalf("expected error when accessToken is missing")
	}
}

func TestRun_WritesFile(t *testing.T) {
	db := seedIDEState(t, map[string]string{
		"cursorAuth/accessToken":  "access-abc",
		"cursorAuth/refreshToken": "refresh-xyz",
		"cursorAuth/cachedEmail":  "person@example.com",
	})
	dir := t.TempDir()
	if err := run(db, "", dir, false, knobs{note: "hello", priority: 7}); err != nil {
		t.Fatalf("run: %v", err)
	}
	want := filepath.Join(dir, cpaformat.FileNameForEmail("person@example.com"))
	buf, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got cpaformat.AuthFile
	if err := json.Unmarshal(buf, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != cpaformat.ProviderType {
		t.Fatalf("Type = %q, want %q", got.Type, cpaformat.ProviderType)
	}
	if got.Email != "person@example.com" || got.AccessToken != "access-abc" {
		t.Fatalf("account fields wrong: %+v", got)
	}
	if got.Note != "hello" || got.Priority != 7 {
		t.Fatalf("operator knobs not applied: note=%q priority=%d", got.Note, got.Priority)
	}
	// The refresh_token field should be captured so downstream refresh
	// paths can renew the JWT unattended.
	if got.RefreshToken != "refresh-xyz" {
		t.Fatalf("RefreshToken = %q, want %q", got.RefreshToken, "refresh-xyz")
	}
}

func TestRun_ExplicitOutPath(t *testing.T) {
	db := seedIDEState(t, map[string]string{
		"cursorAuth/accessToken": "access-abc",
		"cursorAuth/cachedEmail": "explicit@example.com",
	})
	dir := t.TempDir()
	dest := filepath.Join(dir, "custom-name.json")
	if err := run(db, dest, "", false, knobs{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("expected %s to exist: %v", dest, err)
	}
	if _, err := os.Stat(filepath.Join(dir, cpaformat.FileNameForEmail("explicit@example.com"))); err == nil {
		t.Fatalf("should not have written the default-named file when -out was set")
	}
}

func TestRun_Stdout(t *testing.T) {
	db := seedIDEState(t, map[string]string{
		"cursorAuth/accessToken": "access-abc",
		"cursorAuth/cachedEmail": "stdout@example.com",
	})
	// Redirect stdout to a pipe; drain in a goroutine so run() never
	// blocks on a full pipe buffer for large JSON payloads.
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	orig := os.Stdout
	os.Stdout = w
	t.Cleanup(func() { os.Stdout = orig })

	drained := make(chan []byte, 1)
	go func() {
		buf, _ := io.ReadAll(r)
		drained <- buf
	}()

	if err := run(db, "", "", true, knobs{}); err != nil {
		t.Fatalf("run: %v", err)
	}
	_ = w.Close()
	out := <-drained

	if !strings.Contains(string(out), `"email": "stdout@example.com"`) {
		t.Fatalf("stdout does not contain expected JSON, got: %s", string(out))
	}
	var got cpaformat.AuthFile
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal stdout JSON: %v — raw: %s", err, string(out))
	}
	if got.Type != cpaformat.ProviderType {
		t.Fatalf("stdout JSON Type = %q, want %q", got.Type, cpaformat.ProviderType)
	}
}

func TestDefaultIDEStatePath_HasHomePrefix(t *testing.T) {
	p, err := defaultIDEStatePath()
	if err != nil {
		t.Skipf("skip: no default path on this GOOS: %v", err)
	}
	if p == "" {
		t.Fatalf("empty path")
	}
	if !strings.HasSuffix(p, "state.vscdb") {
		t.Fatalf("path %q should end with state.vscdb", p)
	}
}
