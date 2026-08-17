// cursor-export reads the currently signed-in Cursor IDE account from
// the local state.vscdb and emits a CPA-compatible auth JSON so it can
// be dropped into CLIProxyAPI's auths/ directory (or any tool that
// consumes the cursor-proto CPA shape).
//
// It replaces the previous two-step flow of
//
//	cursor-login (or an ad-hoc IDE dumper)  →  cursor-to-cpa
//
// with one command that reads the IDE's SQLite storage directly and
// writes the final CPA JSON in one pass. Refresh tokens, machine IDs,
// and email are all pulled from the same DB the Cursor IDE uses at
// runtime, so the exported file is exactly what the running IDE holds.
//
// Usage:
//
//	cursor-export                                       # → ~/.cli-proxy-api/cursor-<email>.json
//	cursor-export -stdout                               # → stdout (no file written)
//	cursor-export -out /path/to/cursor-foo.json         # → explicit path
//	cursor-export -dir /custom/auth-dir                 # → <dir>/cursor-<email>.json
//	cursor-export -db /path/to/state.vscdb              # → override IDE DB path
//	cursor-export -note "prod #7" -priority 10 -stdout  # attach operator knobs
//
// Design notes:
//
//   - Reads state.vscdb read-only. The IDE can keep running.
//   - Never prints the access or refresh token to stdout unless the
//     user asks for -stdout (in which case the JSON is the whole point).
//   - Writes files with 0600 permissions since they carry live tokens.
//   - Fails loudly rather than silently producing an unusable file
//     when the DB has no accessToken row.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
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

// commaList is a repeatable comma-separated string-slice flag.
type commaList []string

func (c *commaList) String() string { return strings.Join(*c, ",") }
func (c *commaList) Set(v string) error {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	for _, part := range strings.Split(v, ",") {
		if p := strings.TrimSpace(part); p != "" {
			*c = append(*c, p)
		}
	}
	return nil
}

func main() {
	dbPath := flag.String("db", "", "path to Cursor IDE's state.vscdb (default: OS-specific IDE location)")
	outPath := flag.String("out", "", "explicit output file path")
	outDir := flag.String("dir", "", "output directory (defaults to $CLIPROXY_AUTH_DIR or ~/.cli-proxy-api)")
	stdout := flag.Bool("stdout", false, "print CPA auth JSON to stdout; skip writing to disk")
	// Operator knobs (recorded verbatim in the output file, same as cursor-to-cpa).
	prefix := flag.String("prefix", "", "model routing prefix")
	proxyURL := flag.String("proxy-url", "", "per-account proxy URL")
	priority := flag.Int("priority", 0, "scheduler priority hint (larger = preferred)")
	note := flag.String("note", "", "operator note")
	disabled := flag.Bool("disabled", false, "mark the account disabled")
	disableCooling := flag.Bool("disable-cooling", false, "opt out of provider-wide cooldowns")
	requestRetry := flag.Int("request-retry", 0, "per-request retry override")
	var excluded commaList
	flag.Var(&excluded, "excluded-models", "comma-separated model blocklist")
	flag.Parse()

	if err := run(*dbPath, *outPath, *outDir, *stdout, knobs{
		prefix:         strings.TrimSpace(*prefix),
		proxyURL:       strings.TrimSpace(*proxyURL),
		priority:       *priority,
		note:           strings.TrimSpace(*note),
		disabled:       *disabled,
		disableCooling: *disableCooling,
		requestRetry:   *requestRetry,
		excluded:       excluded,
	}); err != nil {
		log.Fatal(err)
	}
}

type knobs struct {
	prefix         string
	proxyURL       string
	priority       int
	note           string
	disabled       bool
	disableCooling bool
	requestRetry   int
	excluded       []string
}

func run(dbPath, outPath, outDir string, toStdout bool, k knobs) error {
	if dbPath == "" {
		p, err := defaultIDEStatePath()
		if err != nil {
			return err
		}
		dbPath = p
	}
	if _, err := os.Stat(dbPath); err != nil {
		return fmt.Errorf("state.vscdb not found at %s: %w (is Cursor IDE installed and signed in?)", dbPath, err)
	}

	acc, err := loadAccountFromIDE(dbPath)
	if err != nil {
		return err
	}

	authFile, err := cpaformat.FromAccount(acc)
	if err != nil {
		return fmt.Errorf("convert to CPA format: %w", err)
	}
	// Apply operator knobs. FromAccount deliberately leaves these blank.
	authFile.Prefix = k.prefix
	authFile.ProxyURL = k.proxyURL
	authFile.Priority = k.priority
	authFile.Note = k.note
	authFile.Disabled = k.disabled
	authFile.DisableCooling = k.disableCooling
	authFile.RequestRetry = k.requestRetry
	if len(k.excluded) > 0 {
		authFile.ExcludedModels = append([]string(nil), k.excluded...)
	}

	buf, err := json.MarshalIndent(authFile, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	if toStdout {
		_, err := os.Stdout.Write(append(buf, '\n'))
		return err
	}

	dest := outPath
	if dest == "" {
		if outDir == "" {
			outDir = defaultAuthDir()
		}
		if err := os.MkdirAll(outDir, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", outDir, err)
		}
		dest = filepath.Join(outDir, cpaformat.FileNameForEmail(acc.Email))
	}

	if err := os.WriteFile(dest, buf, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", dest, err)
	}
	fmt.Fprintf(os.Stderr, "wrote %s (email=%s, refreshable=%v, %d bytes)\n",
		dest, acc.Email, acc.Refreshable, len(buf))
	return nil
}

// loadAccountFromIDE reads state.vscdb read-only and assembles a
// cursor-proto Account. Mirrors the helper used by cmd/cursor-proxy
// (see loadAccountFromIDE there) but exposes the full set of fields
// we can persist rather than just what the proxy needs at runtime.
func loadAccountFromIDE(dbPath string) (*auth.Account, error) {
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
		return nil, fmt.Errorf("no accessToken in %s — is the IDE signed in?", dbPath)
	}
	refresh := q("cursorAuth/refreshToken")
	email := q("cursorAuth/cachedEmail")
	authType := q("cursorAuth/cachedSignUpType")
	authID := q("cursorAuth/authId")
	userID := q("cursorAuth/cachedUserID")
	teamID := teamIDFromIDEValues(q("cursorAuth/teamId"), q("cursorAuth/cachedTeam"))

	mid, _ := auth.GetMachineID()
	mac, _ := auth.GetMacMachineID()

	return &auth.Account{
		Email:        email,
		UserID:       userID,
		AccessToken:  access,
		RefreshToken: refresh,
		AuthID:       authID,
		AuthType:     authType,
		TeamID:       teamID,
		MachineID:    mid,
		MacMachineID: mac,
		IssuedAt:     time.Now(),
		Refreshable:  refresh != "",
	}, nil
}

// teamIDFromIDEValues extracts the Cursor team id from the two SQLite keys
// the IDE persists on team accounts: `cursorAuth/teamId` (a bare numeric
// string when present) and `cursorAuth/cachedTeam` (a JSON blob
// `{"teamId":<int>,"name":"…"}`). Personal accounts leave both empty and
// this returns "" so ApplyCommonHeaders does not emit x-cursor-team-id.
//
// Duplicates the equivalent helper in plugin/cursor/kernel/ide_import.go —
// the two are kept in sync by hand because the CLI is a standalone binary
// and the plugin is a c-shared library that cannot import cmd/*.
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

// defaultIDEStatePath returns the OS-specific location where Cursor
// IDE keeps its SQLite state. Only macOS is verified in the field;
// the Linux and Windows paths follow VS Code / Cursor conventions but
// depend on the user having Cursor installed to their default profile
// directory. --db lets the caller override.
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
		return "", fmt.Errorf("no default IDE path for GOOS=%s; pass -db explicitly", runtime.GOOS)
	}
}

// defaultAuthDir mirrors what cursor-to-cpa uses so both tools land on
// the same CPA auth directory unless overridden.
func defaultAuthDir() string {
	if p := strings.TrimSpace(os.Getenv("CLIPROXY_AUTH_DIR")); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cli-proxy-api")
	}
	return ".cli-proxy-api"
}
