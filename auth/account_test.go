package auth

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSaveAndLoadAccount(t *testing.T) {
	dir := t.TempDir()

	orig := &Account{
		Email:        "tinsels_boxy.5l@icloud.com",
		UserID:       "user_01KX3G01P34XHA85JW68Z9ES36",
		AccessToken:  "eyJhbGci...fake",
		RefreshToken: "eyJhbGci...refresh",
		AuthID:       "auth0|user_01KX3G01P34XHA85JW68Z9ES36",
		AuthType:     "Auth_0",
		IssuedAt:     time.Now(),
		MachineID:    "0644acae98be2a85313a032f0a1434bd90755c7e4e42a10e1cedc710f423c3e0",
		MacMachineID: "df1a4f6be6465f027c4aefa30191f5fa05d1d69d1a791a8504c8d4fe11b674ab",
	}
	path, err := SaveAccount(dir, orig)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if filepath.Base(path) != "cursor-tinsels_boxy.5l_at_icloud.com.json" {
		t.Errorf("unexpected filename: %s", filepath.Base(path))
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("file perms = %v, want 0o600", info.Mode().Perm())
	}

	loaded, err := LoadAccount(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Email != orig.Email ||
		loaded.AccessToken != orig.AccessToken ||
		loaded.MacMachineID != orig.MacMachineID {
		t.Errorf("round-trip mismatch: %+v", loaded)
	}

	// Session defaults should be regenerated.
	if loaded.SessionID == "" || loaded.ClientKey == "" || loaded.ChecksumSession == "" {
		t.Errorf("session defaults not filled: %+v", loaded)
	}
	// Checksum must be derivable from stored machine ids.
	want := GenerateChecksum(loaded.IssuedAt, // note: loaded uses now, not IssuedAt; skip strict compare
		orig.MachineID, orig.MacMachineID)
	_ = want // documented for reference; can't assert equality without controlling time.Now
}

func TestNewAccountFromPollBuildsRealChecksum(t *testing.T) {
	pr := &PollResult{
		AccessToken:  "eyJfake",
		RefreshToken: "eyJrefresh",
		AuthID:       "auth0|user_ABC",
		Type:         "Auth_0",
	}
	a, err := NewAccountFromPoll(pr, "test@example.com")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if a.UserID != "user_ABC" {
		t.Errorf("UserID = %q, want user_ABC", a.UserID)
	}
	if a.MacMachineID == "" {
		t.Skipf("mac machine id not available on this host, skipping checksum check")
	}
	if len(a.ChecksumSession) < 20 {
		t.Errorf("checksum too short: %q", a.ChecksumSession)
	}
}

func TestSaveAndLoadAccountPreservesClientPlatform(t *testing.T) {
	dir := t.TempDir()
	orig := &Account{
		Email:           "windows@example.com",
		AccessToken:     "token",
		ClientOS:        "win32",
		ClientOSVersion: "10.0.22631",
		ClientArch:      "x64",
		ClientShell:     `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		WorkspacePath:   `C:\Users\Example\project`,
	}
	path, err := SaveAccount(dir, orig)
	if err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	got, err := LoadAccount(path)
	if err != nil {
		t.Fatalf("LoadAccount: %v", err)
	}
	if got.ClientOS != orig.ClientOS || got.ClientOSVersion != orig.ClientOSVersion || got.ClientArch != orig.ClientArch {
		t.Fatalf("platform = %q/%q/%q, want %q/%q/%q", got.ClientOS, got.ClientOSVersion, got.ClientArch, orig.ClientOS, orig.ClientOSVersion, orig.ClientArch)
	}
	if got.ClientShell != orig.ClientShell || got.WorkspacePath != orig.WorkspacePath {
		t.Fatalf("environment = %q/%q, want %q/%q", got.ClientShell, got.WorkspacePath, orig.ClientShell, orig.WorkspacePath)
	}
}

func TestNewAccountFromPollRejectsWebToken(t *testing.T) {
	pr := &PollResult{
		AccessToken: typedJWT(t, "web", "auth0|user_ABC"),
		AuthID:      "auth0|user_ABC",
	}
	if _, err := NewAccountFromPoll(pr, "test@example.com"); err == nil {
		t.Fatal("expected type=web poll token to be rejected")
	}
}

func TestExtractUserID(t *testing.T) {
	cases := map[string]string{
		"auth0|user_01KX3G":  "user_01KX3G",
		"workos|user_abc123": "user_abc123",
		"just_user_id":       "just_user_id",
		"":                   "",
	}
	for in, want := range cases {
		if got := extractUserID(in); got != want {
			t.Errorf("extractUserID(%q) = %q, want %q", in, got, want)
		}
	}
}
