package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadCredentialFileCompoundLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.txt")
	if err := os.WriteFile(path, []byte("alice@example.com----ignored-password----user_abc::jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	email, credential, err := readCredentialFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if email != "alice@example.com" || credential != "user_abc::jwt" {
		t.Fatalf("got email=%q credential=%q", email, credential)
	}
}

func TestReadCredentialFileRawCredential(t *testing.T) {
	path := filepath.Join(t.TempDir(), "account.txt")
	if err := os.WriteFile(path, []byte("user_abc::jwt\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	email, credential, err := readCredentialFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if email != "" || credential != "user_abc::jwt" {
		t.Fatalf("got email=%q credential=%q", email, credential)
	}
}
