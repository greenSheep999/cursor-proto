package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// SaveAccount MUST write "type":"cursor" so cpaformat.Unmarshal accepts the
// file. A regression here breaks every fresh import (all four CLI entry
// points) with `bad_auth: validate storage: unexpected type "" (want cursor)`.
func TestSaveAccountWritesProviderType(t *testing.T) {
	dir := t.TempDir()
	acc := &Account{Email: "probe@example.com", AccessToken: "AT-x"}
	path, err := SaveAccount(dir, acc)
	if err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	if !filepath.IsAbs(path) && !filepath.IsLocal(path) {
		t.Fatalf("bad path: %q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["type"] != AccountFileProviderType {
		t.Fatalf(`missing/incorrect "type" field: got %v, want %q`, obj["type"], AccountFileProviderType)
	}
	if obj["email"] != "probe@example.com" || obj["access_token"] != "AT-x" {
		t.Fatalf("other fields lost during marshal round-trip: %v", obj)
	}
}

// SaveAccount MUST rename Account.AuthType ("auth_type") to CPA's on-disk
// "auth_kind" key. Every production cursor-*.json in
// /data/cli-proxy-api/auths/ carries auth_kind (e.g. "Auth_0"); leaving
// auth_type in place breaks parity with the already-deployed accounts.
func TestSaveAccountRenamesAuthTypeToAuthKind(t *testing.T) {
	dir := t.TempDir()
	acc := &Account{Email: "probe2@example.com", AccessToken: "AT", AuthType: "Auth_0"}
	path, err := SaveAccount(dir, acc)
	if err != nil {
		t.Fatalf("SaveAccount: %v", err)
	}
	raw, _ := os.ReadFile(path)
	var obj map[string]any
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatal(err)
	}
	if _, present := obj["auth_type"]; present {
		t.Fatalf(`"auth_type" must be renamed to "auth_kind"; still present: %v`, obj["auth_type"])
	}
	if obj["auth_kind"] != "Auth_0" {
		t.Fatalf(`"auth_kind" = %v, want "Auth_0"`, obj["auth_kind"])
	}
}
