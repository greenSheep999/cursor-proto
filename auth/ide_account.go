package auth

// IDE account loading is kept in auth so every command uses the same
// snapshot, identity, and client-fingerprint rules. Reading the live
// state.vscdb directly is unsafe because Cursor keeps the current values in
// its WAL while the main database can contain stale credentials.

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
)

// IDEStatePath returns Cursor's per-user state database path.
func IDEStatePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home dir: %w", err)
	}
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb"), nil
	case "linux":
		return filepath.Join(home, ".config", "Cursor", "User", "globalStorage", "state.vscdb"), nil
	case "windows":
		appData := os.Getenv("APPDATA")
		if appData == "" {
			appData = filepath.Join(home, "AppData", "Roaming")
		}
		return filepath.Join(appData, "Cursor", "User", "globalStorage", "state.vscdb"), nil
	default:
		return filepath.Join(home, ".cursor", "state.vscdb"), nil
	}
}

// LoadAccountFromIDE loads the currently signed-in Cursor account. It takes a
// private three-file snapshot before opening SQLite, so WAL-backed account
// switches are visible without touching the IDE's live database.
func LoadAccountFromIDE() (*Account, error) {
	src, err := IDEStatePath()
	if err != nil {
		return nil, err
	}
	snapshot, err := SnapshotIDEDB(src)
	if err != nil {
		return nil, err
	}
	return LoadAccountFromIDEDB(snapshot)
}

// LoadAccountFromIDEDB loads an account from an already prepared database
// path. It is exported for commands/tests that need an explicit snapshot.
func LoadAccountFromIDEDB(dbPath string) (*Account, error) {
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open ide db: %w", err)
	}
	defer db.Close()

	q := func(key string) string {
		var value string
		_ = db.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, key).Scan(&value)
		return value
	}
	access := q("cursorAuth/accessToken")
	if strings.TrimSpace(access) == "" {
		return nil, fmt.Errorf("no accessToken in %s - is the IDE signed in?", dbPath)
	}
	refresh := q("cursorAuth/refreshToken")
	email := q("cursorAuth/cachedEmail")
	authType := q("cursorAuth/cachedSignUpType")
	authID := q("cursorAuth/authId")
	userID := q("cursorAuth/cachedUserID")

	// The token is the authority for the active identity. Cursor can leave
	// cached authId/cachedUserID behind after an account switch; using those
	// stale values makes otherwise valid requests look unauthenticated.
	if claims, decodeErr := DecodeJWTClaims(access); decodeErr == nil {
		if strings.TrimSpace(claims.Sub) != "" {
			authID = claims.Sub
			userID = claims.Sub
			if i := strings.IndexByte(userID, '|'); i >= 0 {
				userID = userID[i+1:]
			}
		}
	}

	machineID, _ := GetMachineID()
	macID, _ := GetMacMachineID()
	checksumMachineID, checksumMacID := ideTelemetryIDs()
	if checksumMachineID == "" {
		checksumMachineID = KnownChecksumMachineID_3_16_17
	}
	if checksumMacID != "" {
		macID = checksumMacID
	}
	version, commit, releaseHash := installedCursorFingerprint()
	// cachedTeam is updated together with the active auth identity. The bare
	// teamId key can lag after switching between a personal and enterprise
	// account, so prefer the structured value when both exist.
	teamID := teamIDFromIDEValues("", q("cursorAuth/cachedTeam"))
	if teamID == "" {
		teamID = teamIDFromIDEValues(q("cursorAuth/teamId"), "")
	}
	privacyMode := 0
	if strings.EqualFold(strings.TrimSpace(q("cursorai/donotchange/privacyMode")), "true") {
		privacyMode = 1
	}
	configVersion := serverConfigVersion(q("cursorai/serverConfig"))
	account := &Account{
		Email:             email,
		UserID:            userID,
		AccessToken:       access,
		RefreshToken:      refresh,
		AuthID:            authID,
		AuthType:          authType,
		TeamID:            teamID,
		PrivacyMode:       privacyMode,
		MachineID:         machineID,
		MacMachineID:      macID,
		ChecksumMachineID: checksumMachineID,
		ClientVersion:     version,
		ClientCommit:      commit,
		ReleaseHash:       releaseHash,
		ConfigVersion:     configVersion,
		IssuedAt:          time.Now(),
		Refreshable:       refresh != "",
	}
	account.FillSessionDefaults(time.Now())
	return account, nil
}

func serverConfigVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if unquoted, err := strconv.Unquote(raw); err == nil {
		raw = strings.TrimSpace(unquoted)
	}
	var config struct {
		ConfigVersion string `json:"configVersion"`
	}
	if json.Unmarshal([]byte(raw), &config) != nil {
		return ""
	}
	return strings.TrimSpace(config.ConfigVersion)
}

func ideTelemetryIDs() (machineID, macMachineID string) {
	src, err := IDEStatePath()
	if err != nil {
		return "", ""
	}
	storagePath := filepath.Join(filepath.Dir(src), "storage.json")
	buf, err := os.ReadFile(storagePath)
	if err != nil {
		return "", ""
	}
	var storage struct {
		MachineID    string `json:"telemetry.machineId"`
		MacMachineID string `json:"telemetry.macMachineId"`
	}
	if json.Unmarshal(buf, &storage) != nil {
		return "", ""
	}
	return strings.TrimSpace(storage.MachineID), strings.TrimSpace(storage.MacMachineID)
}

func teamIDFromIDEValues(directKey, cachedTeamJSON string) string {
	if id := strings.TrimSpace(directKey); id != "" && id != "0" {
		return id
	}
	raw := strings.TrimSpace(cachedTeamJSON)
	if raw == "" {
		return ""
	}
	if unquoted, err := strconv.Unquote(raw); err == nil {
		raw = strings.TrimSpace(unquoted)
	}
	var payload struct {
		TeamID any `json:"teamId"`
	}
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	switch value := payload.TeamID.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		if value > 0 {
			return strconv.FormatInt(int64(value), 10)
		}
	case json.Number:
		return string(value)
	}
	return ""
}

type cursorProduct struct {
	Version    string `json:"version"`
	Commit     string `json:"commit"`
	RealCommit string `json:"realCommit"`
}

func installedCursorFingerprint() (version, commit, releaseHash string) {
	app := os.Getenv("CURSOR_APP_PATH")
	if app == "" && runtime.GOOS == "darwin" {
		app = "/Applications/Cursor.app"
	}
	if app == "" {
		return "", "", ""
	}
	path := filepath.Join(app, "Contents", "Resources", "app", "product.json")
	buf, err := os.ReadFile(path)
	if err != nil {
		return "", "", ""
	}
	var product cursorProduct
	if json.Unmarshal(buf, &product) != nil {
		return "", "", ""
	}
	version = strings.TrimSpace(product.Version)
	commit = strings.TrimSpace(product.Commit)
	releaseHash = strings.TrimSpace(product.RealCommit)
	if releaseHash == "" {
		releaseHash = commit
	}
	return version, commit, releaseHash
}
