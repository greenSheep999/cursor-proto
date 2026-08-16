package cpaformat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
)

func sampleAccount() *auth.Account {
	return &auth.Account{
		Email:           "Test.User@Example.com",
		UserID:          "user_01KX3G01P34XHA85JW68Z9ES36",
		AccessToken:     "cursor-access-token",
		RefreshToken:    "cursor-refresh-token",
		AuthID:          "auth0|user_01KX3G01P34XHA85JW68Z9ES36",
		AuthType:        "Auth_0",
		IssuedAt:        time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC),
		ExpiresAt:       time.Date(2025, 1, 3, 3, 4, 5, 0, time.UTC),
		MachineID:       "machine-id-hex",
		MacMachineID:    "mac-machine-id-hex",
		ClientOS:        "win32",
		ClientOSVersion: "10.0.22631",
		ClientArch:      "x64",
		ClientShell:     `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		WorkspacePath:   `C:\Users\Test.User\project`,
	}
}

func TestFromAccount_Basics(t *testing.T) {
	a, err := FromAccount(sampleAccount())
	if err != nil {
		t.Fatalf("FromAccount: %v", err)
	}
	if a.Type != ProviderType {
		t.Errorf("Type = %q, want %q", a.Type, ProviderType)
	}
	if a.AccessToken != "cursor-access-token" {
		t.Errorf("AccessToken not carried over: %q", a.AccessToken)
	}
	if a.RefreshToken != "cursor-refresh-token" {
		t.Errorf("RefreshToken not carried over: %q", a.RefreshToken)
	}
	if a.Email != "Test.User@Example.com" {
		t.Errorf("Email should preserve case: %q", a.Email)
	}
	if a.MachineID != "machine-id-hex" {
		t.Errorf("MachineID not carried over: %q", a.MachineID)
	}
	if a.MacMachineID != "mac-machine-id-hex" {
		t.Errorf("MacMachineID not carried over: %q", a.MacMachineID)
	}
	if a.ClientOS != "win32" || a.ClientOSVersion != "10.0.22631" || a.ClientArch != "x64" {
		t.Errorf("client platform not carried over: os=%q version=%q arch=%q", a.ClientOS, a.ClientOSVersion, a.ClientArch)
	}
	if a.ClientShell != srcWindowsShell || a.WorkspacePath != `C:\Users\Test.User\project` {
		t.Errorf("client environment not carried over: shell=%q workspace=%q", a.ClientShell, a.WorkspacePath)
	}
	if a.Expired == "" {
		t.Errorf("Expired should be set for known expiration")
	}
	if a.IssuedAt == "" {
		t.Errorf("IssuedAt should be set")
	}
}

func TestFromAccount_MissingAccessToken(t *testing.T) {
	acc := sampleAccount()
	acc.AccessToken = ""
	if _, err := FromAccount(acc); err == nil {
		t.Fatal("expected error for empty access_token, got nil")
	}
}

func TestAuthFile_FileName(t *testing.T) {
	a, err := FromAccount(sampleAccount())
	if err != nil {
		t.Fatalf("FromAccount: %v", err)
	}
	got := a.FileName()
	want := "cursor-test.user_at_example.com.json"
	if got != want {
		t.Errorf("FileName = %q, want %q", got, want)
	}
}

func TestAuthFile_MarshalRoundTrip(t *testing.T) {
	a, err := FromAccount(sampleAccount())
	if err != nil {
		t.Fatalf("FromAccount: %v", err)
	}
	a.Prefix = "team-a"
	a.ProxyURL = "http://proxy.local:8080"
	a.Priority = 5
	a.Note = "primary cursor account"
	a.DisableCooling = true
	a.RequestRetry = 2
	a.ExcludedModels = []string{"composer-1"}

	buf, err := a.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// CPA reads the file into a generic map first, so make sure the
	// top-level object exposes the fields it looks for directly.
	var meta map[string]any
	if err := json.Unmarshal(buf, &meta); err != nil {
		t.Fatalf("unmarshal into map: %v", err)
	}
	for _, k := range []string{"type", "access_token", "email", "client_os", "client_os_version", "client_arch", "client_shell", "workspace_path", "prefix", "proxy_url"} {
		if _, ok := meta[k]; !ok {
			t.Errorf("missing top-level key %q", k)
		}
	}
	if got := meta["type"]; got != ProviderType {
		t.Errorf("meta type = %v, want %v", got, ProviderType)
	}

	// Round-trip.
	round, err := Unmarshal(buf)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if round.AccessToken != a.AccessToken {
		t.Errorf("AccessToken mismatch: %q vs %q", round.AccessToken, a.AccessToken)
	}
	if round.Prefix != a.Prefix {
		t.Errorf("Prefix mismatch: %q vs %q", round.Prefix, a.Prefix)
	}
	if round.Priority != a.Priority {
		t.Errorf("Priority mismatch: %d vs %d", round.Priority, a.Priority)
	}
	if !round.DisableCooling {
		t.Errorf("DisableCooling not preserved")
	}
	if len(round.ExcludedModels) != 1 || round.ExcludedModels[0] != "composer-1" {
		t.Errorf("ExcludedModels not preserved: %v", round.ExcludedModels)
	}
}

func TestAuthFile_ToAccountRoundTrip(t *testing.T) {
	src := sampleAccount()
	a, err := FromAccount(src)
	if err != nil {
		t.Fatalf("FromAccount: %v", err)
	}
	buf, err := a.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Unmarshal(buf)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	back, err := got.ToAccount()
	if err != nil {
		t.Fatalf("ToAccount: %v", err)
	}
	if back.AccessToken != src.AccessToken {
		t.Errorf("AccessToken round-trip mismatch")
	}
	if back.RefreshToken != src.RefreshToken {
		t.Errorf("RefreshToken round-trip mismatch")
	}
	if back.UserID != src.UserID {
		t.Errorf("UserID round-trip mismatch")
	}
	if back.AuthID != src.AuthID {
		t.Errorf("AuthID round-trip mismatch")
	}
	if back.AuthType != src.AuthType {
		t.Errorf("AuthType round-trip mismatch")
	}
	if back.MachineID != src.MachineID {
		t.Errorf("MachineID round-trip mismatch")
	}
	if back.ClientOS != src.ClientOS || back.ClientOSVersion != src.ClientOSVersion || back.ClientArch != src.ClientArch {
		t.Errorf("client platform round-trip mismatch: got %q/%q/%q", back.ClientOS, back.ClientOSVersion, back.ClientArch)
	}
	if back.ClientShell != src.ClientShell || back.WorkspacePath != src.WorkspacePath {
		t.Errorf("client environment round-trip mismatch: got %q/%q", back.ClientShell, back.WorkspacePath)
	}
	if !back.IssuedAt.Equal(src.IssuedAt) {
		t.Errorf("IssuedAt round-trip mismatch: %v vs %v", back.IssuedAt, src.IssuedAt)
	}
	if !back.ExpiresAt.Equal(src.ExpiresAt) {
		t.Errorf("ExpiresAt round-trip mismatch: %v vs %v", back.ExpiresAt, src.ExpiresAt)
	}
}

const srcWindowsShell = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`

func TestAuthFile_WriteToDir(t *testing.T) {
	a, err := FromAccount(sampleAccount())
	if err != nil {
		t.Fatalf("FromAccount: %v", err)
	}
	dir := t.TempDir()
	path, err := a.WriteToDir(dir)
	if err != nil {
		t.Fatalf("WriteToDir: %v", err)
	}
	if !strings.HasPrefix(path, dir) {
		t.Errorf("path %q not under %q", path, dir)
	}
	if filepath.Base(path) != a.FileName() {
		t.Errorf("path base %q != FileName %q", filepath.Base(path), a.FileName())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perms = %v, want 0600", info.Mode().Perm())
	}
	// Ensure the file is valid CPA-shape JSON.
	buf, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	got, err := Unmarshal(buf)
	if err != nil {
		t.Fatalf("Unmarshal from disk: %v", err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestValidate_RejectsWrongType(t *testing.T) {
	a := &AuthFile{
		CursorTokenStorage: CursorTokenStorage{
			Type:        "claude",
			AccessToken: "x",
		},
	}
	if err := a.Validate(); err == nil {
		t.Fatal("expected error for wrong type, got nil")
	}
}
