package kernel

// IDE import helper for cursor login mode "ide". Reads the local
// Cursor IDE state.vscdb (SQLite) and assembles the same
// cpaformat.AuthFile the OAuth flow produces.
//
// This mirrors cmd/cursor-export/main.go's loadAccountFromIDE and
// defaultIDEStatePath. The two implementations are kept in sync by
// hand because cursor-export is a standalone CLI and the plugin is
// a c-shared library; they do not share code beyond the auth and
// cpaformat packages.
//
// Design note: the Cursor plugin also runs on Linux/arm64 CPA
// servers, where the IDE is almost never installed. In that
// environment the IDE mode returns an error rather than a
// non-descriptive failure. Callers can override the database path
// with the "db_path" metadata field for tests or non-standard
// installs (e.g. flatpak, portable installs).

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// defaultIDEStatePath returns the OS-specific location where Cursor
// IDE keeps its SQLite state. Copied from
// cmd/cursor-export/main.go so the plugin does not depend on the CLI
// package.
func defaultIDEStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor",
			"User", "globalStorage", "state.vscdb"), nil
	case "linux":
		return filepath.Join(home, ".config", "Cursor",
			"User", "globalStorage", "state.vscdb"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Cursor",
			"User", "globalStorage", "state.vscdb"), nil
	default:
		return "", fmt.Errorf("no default IDE path for GOOS=%s; pass db_path in metadata", runtime.GOOS)
	}
}

// loadIDEAccount reads state.vscdb read-only and assembles a
// cpaformat.AuthFile. Mirrors loadAccountFromIDE in
// cmd/cursor-export/main.go.
func loadIDEAccount(dbPath string) (*cpaformat.AuthFile, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", dbPath, err)
	}
	defer func() { _ = db.Close() }()

	q := func(key string) string {
		var v string
		_ = db.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, key).Scan(&v)
		return v
	}
	access := q("cursorAuth/accessToken")
	if access == "" {
		return nil, fmt.Errorf("no accessToken in %s (is Cursor IDE signed in?)", dbPath)
	}
	refresh := q("cursorAuth/refreshToken")
	email := q("cursorAuth/cachedEmail")
	authType := q("cursorAuth/cachedSignUpType")
	authID := q("cursorAuth/authId")
	userID := q("cursorAuth/cachedUserID")
	teamID := teamIDFromIDEValues(q("cursorAuth/teamId"), q("cursorAuth/cachedTeam"))

	mid, _ := auth.GetMachineID()
	mac, _ := auth.GetMacMachineID()

	now := time.Now()
	return &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:         cpaformat.ProviderType,
			AccessToken:  access,
			RefreshToken: refresh,
			Email:        email,
			UserID:       userID,
			AuthID:       authID,
			AuthKind:     authType,
			TeamID:       teamID,
			MachineID:    mid,
			MacMachineID: mac,
			IssuedAt:     cpaformat.FormatTime(now),
			LastRefresh:  cpaformat.FormatTime(now),
			Expired:      cpaformat.FormatTime(auth.ExpiresAtFromJWT(access)),
			Refreshable:  refresh != "",
		},
	}, nil
}

// teamIDFromIDEValues normalises the two SQLite keys Cursor IDE persists for
// team membership into the string representation stored on
// cpaformat.CursorTokenStorage.TeamID. Cursor writes the numeric team id under
// cursorAuth/teamId and a companion JSON blob (`{"teamId":<n>,"name":"..."}`)
// under cursorAuth/cachedTeam. Either can be missing on personal accounts;
// both are strings on the wire because ItemTable.value is TEXT. Non-team
// accounts return "" so downstream headers stay unset.
func teamIDFromIDEValues(directKey, cachedTeamJSON string) string {
	if id := strings.TrimSpace(directKey); id != "" && id != "0" {
		return id
	}
	raw := strings.TrimSpace(cachedTeamJSON)
	if raw == "" {
		return ""
	}
	// storageService.getObject values are JSON-encoded; some IDE builds
	// double-quote-wrap the raw JSON so ItemTable.value parses to a string
	// first — accept both shapes.
	if unquoted, err := strconv.Unquote(raw); err == nil {
		raw = strings.TrimSpace(unquoted)
	}
	var payload struct {
		TeamID any `json:"teamId"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	switch v := payload.TeamID.(type) {
	case string:
		return strings.TrimSpace(v)
	case float64:
		if v <= 0 {
			return ""
		}
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		return string(v)
	}
	return ""
}
