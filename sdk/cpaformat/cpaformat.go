// Package cpaformat defines the on-disk JSON layout used by CLIProxyAPI
// (CPA) for its provider auth files, tailored for the Cursor provider.
//
// CPA persists one JSON file per credential under its auths directory
// (default ~/.cli-proxy-api). Each file is a flat object whose "type"
// field selects a provider-specific token-storage shape. CPA's file
// synthesizer (internal/watcher/synthesizer/file.go) reads the file,
// pulls provider metadata directly off the top-level object, and hands
// the raw JSON to a provider parser (either a built-in one or a
// plugin's auth.parse method).
//
// We use two overlapping views of the same JSON blob:
//
//   - CursorTokenStorage: the provider-side "storage" view (the fields
//     the Cursor executor / auth code needs at runtime: access token,
//     refresh token, machine IDs, etc.).
//   - AuthFile: the on-disk view that folds CursorTokenStorage together
//     with the CPA-visible fields ("type", "email", "prefix",
//     "proxy_url", "priority", "note", "disabled", …).
//
// The same struct is imported by both the converter CLI (cmd/cursor-to-cpa)
// and the future Cursor plugin binary, so producer and consumer agree
// on field names at compile time.
package cpaformat

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// ProviderType is the value CPA looks for in the top-level "type" field
// of an auth JSON file when deciding which provider parser to hand it to.
// The CPA plugin we ship (see plugin/cursor) declares the same identifier
// via auth.identifier / plugin.register.
const ProviderType = "cursor"

// DefaultAuthDirEnv is the environment variable used by CPA to override
// the default auth directory. When empty, CPA uses ~/.cli-proxy-api.
const DefaultAuthDirEnv = "CLIPROXY_AUTH_DIR"

// CursorTokenStorage is the provider-side view of a Cursor credential.
//
// Every field the runtime Cursor client (cursor-proto's auth + executor
// packages) needs to make an authenticated request must be persisted
// here so the plugin can rebuild an *auth.Account from StorageJSON
// alone. The exact shape mirrors auth.Account in this repo, minus the
// non-persistent session fields that FillSessionDefaults regenerates
// on load.
//
// JSON key naming follows CPA's other provider token storages
// (access_token / refresh_token / expired / last_refresh / email /
// type) so operators editing files by hand see a familiar layout.
type CursorTokenStorage struct {
	// Type is always ProviderType. CPA's synthesizer requires it.
	Type string `json:"type"`

	// AccessToken is the Cursor OAuth access token (long JWT-ish bearer).
	AccessToken string `json:"access_token"`

	// RefreshToken is used to obtain new access tokens when AccessToken
	// expires. May be empty if the login response did not return one.
	RefreshToken string `json:"refresh_token,omitempty"`

	// Email is the account email address. Used as CPA's auth Label and
	// to derive the auth filename.
	Email string `json:"email,omitempty"`

	// UserID is Cursor's opaque user identifier (e.g. "user_01KX…").
	UserID string `json:"user_id,omitempty"`

	// AuthID is the full "auth0|user_…" / "workos|user_…" identity string
	// returned by Cursor's poll response.
	AuthID string `json:"auth_id,omitempty"`

	// AuthKind classifies the identity provider that fronts Cursor's
	// login flow (e.g. "Auth_0", "workos"). Copied from pr.Type.
	AuthKind string `json:"auth_kind,omitempty"`

	// MachineID and MacMachineID pin the device identifiers so requests
	// coming from CPA look like the same physical device to Cursor.
	// If empty, the plugin regenerates them at load time.
	MachineID    string `json:"machine_id,omitempty"`
	MacMachineID string `json:"mac_machine_id,omitempty"`

	// Client platform fields describe the Cursor IDE environment presented to
	// upstream. They are optional for backward compatibility; the executor
	// falls back to a coherent profile for its local runtime when omitted.
	ClientOS        string `json:"client_os,omitempty"`
	ClientOSVersion string `json:"client_os_version,omitempty"`
	ClientArch      string `json:"client_arch,omitempty"`
	ClientShell     string `json:"client_shell,omitempty"`
	WorkspacePath   string `json:"workspace_path,omitempty"`

	// IssuedAt is when the token was last issued/refreshed, in RFC3339.
	IssuedAt string `json:"issued_at,omitempty"`

	// LastRefresh mirrors CPA's other providers (last_refresh field on
	// their token storages) so refresh logic is easy to reason about.
	// Same value semantics as IssuedAt when populated by the converter.
	LastRefresh string `json:"last_refresh,omitempty"`

	// Expired is the access-token expiration timestamp. Uses the
	// "expired" key (not "expires_at") to match CPA's convention;
	// CPA.ExpirationTime() understands it directly.
	Expired string `json:"expired,omitempty"`

	// Refreshable is true when we hold enough material (a refresh_token
	// or equivalent) to perform an unattended token refresh. Set to
	// false for batch-imported access-only accounts so operators can
	// spot the "manual re-auth required" ones at a glance.
	Refreshable bool `json:"refreshable,omitempty"`

	// RefreshLeadNanos is how far ahead of Expired a refresh should be
	// attempted, in nanoseconds. Zero means "use the caller's default".
	// Stored as a raw int so the field round-trips as a Go
	// time.Duration without importing that type into CPA.
	RefreshLeadNanos int64 `json:"refresh_lead,omitempty"`
}

// AuthFile is the exact on-disk shape written by the converter and read
// by CPA's synthesizer. It embeds CursorTokenStorage so provider fields
// sit at the top level (which is what CPA expects), and adds the
// CPA-visible operator knobs.
type AuthFile struct {
	CursorTokenStorage

	// Prefix optionally namespaces this account's models when routing.
	Prefix string `json:"prefix,omitempty"`

	// ProxyURL overrides the global proxy for this account.
	ProxyURL string `json:"proxy_url,omitempty"`

	// Disabled hides the account from the scheduler when true.
	Disabled bool `json:"disabled,omitempty"`

	// Priority is a scheduler hint (larger = preferred).
	Priority int `json:"priority,omitempty"`

	// Note is a free-form human-readable comment for the operator.
	Note string `json:"note,omitempty"`

	// ExcludedModels blocklists specific models for this account.
	ExcludedModels []string `json:"excluded_models,omitempty"`

	// DisableCooling opts out of provider-wide cooldowns for this auth.
	DisableCooling bool `json:"disable_cooling,omitempty"`

	// RequestRetry overrides the per-request retry count.
	RequestRetry int `json:"request_retry,omitempty"`
}

// FileName returns the canonical on-disk filename for this account.
// Layout: cursor-<sanitized_email>.json (matches auth.AccountFilePath
// so cursor-proto and CPA agree on where files live).
func (a *AuthFile) FileName() string {
	return FileNameForEmail(a.Email)
}

// FileNameForEmail returns the canonical filename for an email address.
func FileNameForEmail(email string) string {
	base := strings.TrimSpace(email)
	if base == "" {
		base = "unknown"
	}
	safe := SanitizeEmail(base)
	return fmt.Sprintf("cursor-%s.json", safe)
}

// SanitizeEmail lowercases and replaces filesystem-hostile characters
// so an email can be embedded in a file name safely. Mirrors the helper
// in auth/account.go so both producers agree on the output.
func SanitizeEmail(email string) string {
	r := strings.NewReplacer(
		"@", "_at_",
		"/", "_",
		"\\", "_",
		" ", "_",
		":", "_",
	)
	return r.Replace(strings.ToLower(strings.TrimSpace(email)))
}

// Validate performs the minimal sanity checks CPA's synthesizer will
// enforce before accepting the file.
func (a *AuthFile) Validate() error {
	if a == nil {
		return fmt.Errorf("nil auth file")
	}
	if a.Type != ProviderType {
		return fmt.Errorf("unexpected type %q (want %q)", a.Type, ProviderType)
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return fmt.Errorf("access_token is required")
	}
	return nil
}

// Marshal returns the JSON encoding using two-space indentation to
// match what CPA writes for its own providers.
func (a *AuthFile) Marshal() ([]byte, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	return json.MarshalIndent(a, "", "  ")
}

// Unmarshal parses the JSON encoding of an AuthFile. It does not enforce
// Validate() so callers can inspect partial files.
func Unmarshal(data []byte) (*AuthFile, error) {
	var out AuthFile
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WriteToDir writes the auth file into dir under its canonical filename,
// creating dir with 0700 (matching CPA's own token savers) and the file
// with 0600. Returns the absolute path that was written.
func (a *AuthFile) WriteToDir(dir string) (string, error) {
	if err := a.Validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(dir) == "" {
		return "", fmt.Errorf("output dir is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create auth dir: %w", err)
	}
	path := filepath.Join(dir, a.FileName())
	buf, err := a.Marshal()
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		return "", fmt.Errorf("write auth file: %w", err)
	}
	return path, nil
}

// FormatTime formats t as RFC3339 in UTC. Zero times return "".
func FormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

// ParseTime parses an RFC3339 string. Empty input returns the zero Time.
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, s)
}
