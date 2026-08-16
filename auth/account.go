package auth

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Account is the persistent auth material for one Cursor user.
//
// Design goals:
//   - Interoperate with CLIProxyAPI's ~/.cli-proxy-api/ conventions if adopted
//     as a plugin (one JSON per account, indexable by email).
//   - Store everything needed to make an authenticated request without touching
//     the browser: access token, refresh token, machine identifiers, session
//     identifiers (regenerated on load if empty).
type Account struct {
	Email        string    `json:"email"`
	UserID       string    `json:"user_id"` // e.g. "user_01KX3G01P34XHA85JW68Z9ES36"
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	AuthID       string    `json:"auth_id,omitempty"`   // full auth0|user_... string
	AuthType     string    `json:"auth_type,omitempty"` // "Auth_0" | "workos" | ...
	TeamID       string    `json:"team_id,omitempty"`
	PrivacyMode  int       `json:"privacy_mode,omitempty"`
	InternalUser bool      `json:"internal_user,omitempty"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`

	// Device identifiers – captured at first login so multi-machine syncing is
	// possible (i.e. this Account can be shipped to another host and still
	// appear as the same "device" to Cursor).
	MachineID    string `json:"machine_id,omitempty"`
	MacMachineID string `json:"mac_machine_id,omitempty"`
	// ChecksumMachineID is the abuse-service identity used inside
	// x-cursor-checksum. Cursor 3.16 keeps this separate from the persisted
	// device MachineID; IDE captures use the bundled 3.10 release identity.
	ChecksumMachineID string `json:"checksum_machine_id,omitempty"`

	// Client platform fields keep the IDE fingerprint internally consistent
	// when an account is used on a host other than the machine that created it.
	// Empty fields fall back to the executor's local runtime defaults.
	ClientOS        string `json:"client_os,omitempty"`
	ClientOSVersion string `json:"client_os_version,omitempty"`
	ClientArch      string `json:"client_arch,omitempty"`
	ClientType      string `json:"client_type,omitempty"`
	ClientLayout    string `json:"client_layout,omitempty"`
	ClientShell     string `json:"client_shell,omitempty"`
	WorkspacePath   string `json:"workspace_path,omitempty"`

	// ProxyURL routes this account's Cursor upstream traffic through an
	// account-specific HTTP, HTTPS, SOCKS5, or SOCKS5H proxy.
	ProxyURL string `json:"proxy_url,omitempty"`

	// Refreshable is true when we hold enough material (a refresh_token) to
	// perform an unattended token refresh. Batch importers that only receive
	// an access_token flip this to false so the pool inspector can flag those
	// accounts as "manual re-auth required" when they expire.
	Refreshable bool `json:"refreshable,omitempty"`

	// RefreshLead is how far ahead of ExpiresAt a refresh should be
	// attempted. Zero means "use the caller's default".
	RefreshLead time.Duration `json:"refresh_lead,omitempty"`

	// Session identifiers – regenerated on load if empty.
	SessionID       string `json:"session_id,omitempty"`
	ConfigVersion   string `json:"config_version,omitempty"`
	ClientKey       string `json:"client_key,omitempty"`
	ChecksumSession string `json:"-"` // pre-computed x-cursor-checksum value
}

// AccountFilePath returns the on-disk path for a given account.
// Layout: <dir>/cursor-<sanitized_email>.json
func AccountFilePath(dir, email string) string {
	safe := sanitizeEmail(email)
	return filepath.Join(dir, fmt.Sprintf("cursor-%s.json", safe))
}

func sanitizeEmail(email string) string {
	// Keep it deterministic and filesystem-safe.
	r := strings.NewReplacer("@", "_at_", "/", "_", "\\", "_", " ", "_", ":", "_")
	return r.Replace(strings.ToLower(email))
}

// SaveAccount writes the account to disk (0600 perms).
func SaveAccount(dir string, a *Account) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := AccountFilePath(dir, a.Email)
	buf, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// LoadAccount reads and validates an account JSON file.
// Regenerates per-session identifiers when they're absent so each process
// gets a fresh session even when the underlying account is reused.
func LoadAccount(path string) (*Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if a.AccessToken == "" {
		return nil, fmt.Errorf("account %s: empty access_token", path)
	}
	a.FillSessionDefaults(time.Now())
	return &a, nil
}

// FillSessionDefaults generates per-process fields that aren't persisted.
// Call it whenever a fresh session should be started (e.g. process startup).
func (a *Account) FillSessionDefaults(now time.Time) {
	if a.SessionID == "" {
		a.SessionID = GenerateSessionID()
	}
	if a.ConfigVersion == "" {
		a.ConfigVersion = GenerateSessionID()
	}
	if a.ClientKey == "" {
		a.ClientKey = GenerateClientKey()
	}
	if a.ChecksumSession == "" {
		mid := a.ChecksumMachineID
		if mid == "" {
			mid = KnownChecksumMachineID_3_16_17
		}
		a.ChecksumSession = GenerateChecksum(now, mid, a.MacMachineID)
	}
}

// NewAccountFromPoll builds an Account from a completed OAuth poll response
// plus locally-discovered machine identifiers. This is what the login CLI calls
// right before SaveAccount.
func NewAccountFromPoll(pr *PollResult, email string) (*Account, error) {
	if pr == nil || pr.AccessToken == "" {
		return nil, fmt.Errorf("poll result missing accessToken")
	}
	if tokenType := TokenType(pr.AccessToken); tokenType != "" && tokenType != "session" {
		return nil, fmt.Errorf("poll access token has type %q, want %q", tokenType, "session")
	}
	userID := extractUserID(pr.AuthID)

	machineID, _ := GetMachineID()
	macID, _ := GetMacMachineID()

	now := time.Now()
	a := &Account{
		Email:        email,
		UserID:       userID,
		AccessToken:  pr.AccessToken,
		RefreshToken: pr.RefreshToken,
		AuthID:       pr.AuthID,
		AuthType:     pr.Type,
		IssuedAt:     now,
		MachineID:    machineID,
		MacMachineID: macID,
		Refreshable:  pr.RefreshToken != "",
		RefreshLead:  30 * time.Minute,
	}
	// JWT exp field could be parsed to fill ExpiresAt; keep unset for now.
	a.FillSessionDefaults(now)
	return a, nil
}

// extractUserID pulls the trailing user id from "auth0|user_xxx" / "workos|user_xxx"
// style AuthID strings. Returns the whole string if no separator is found.
func extractUserID(authID string) string {
	if authID == "" {
		return ""
	}
	if i := strings.Index(authID, "|"); i >= 0 {
		return authID[i+1:]
	}
	return authID
}
