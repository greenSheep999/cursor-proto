package auth

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestParseIDELoginCredentialFillsRefreshForSessionToken(t *testing.T) {
	token := typedJWT(t, "session", "auth0|user_01NEW")
	raw := "== new@example.com----ignored-password----ignored-field----user_01NEW::" + token
	credential, err := ParseIDELoginCredential(raw, "", "", time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if credential.Email != "new@example.com" || credential.UserID != "user_01NEW" || credential.AuthID != "auth0|user_01NEW" {
		t.Fatalf("unexpected identity: %+v", credential)
	}
	if credential.AccessToken != token || credential.RefreshToken != token {
		t.Fatal("session JWT must populate both access and refresh tokens")
	}
	if credential.AuthType != "Auth_0" {
		t.Fatalf("auth type = %q, want Auth_0", credential.AuthType)
	}
}

func TestParseIDELoginCredentialRejectsWebToken(t *testing.T) {
	token := typedJWT(t, "web", "auth0|user_01WEB")
	if _, err := ParseIDELoginCredential(token, "web@example.com", "", time.Now()); err == nil {
		t.Fatal("expected web token to be rejected")
	}
}

func TestWriteIDELoginCredentialWritesCompleteLoginAndClearsStaleIdentity(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"cursorAuth/teamId":                   "123",
		"cursorAuth/cachedTeam":               `{"teamId":123}`,
		"cursorAuth/cachedScopedProfile":      `{"displayName":"Old"}`,
		"cursorAuth/stripeMembershipType":     "enterprise",
		"cursorAuth/stripeMembershipAuthId":   "auth0|user_OLD",
		"cursorAuth/stripeSubscriptionStatus": "active",
	} {
		if _, err := db.Exec(`INSERT INTO ItemTable(key, value) VALUES (?, ?)`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	token := typedJWT(t, "session", "auth0|user_01NEW")
	credential, err := ParseIDELoginCredential(token, "new@example.com", "", time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteIDELoginCredential(dbPath, credential); err != nil {
		t.Fatal(err)
	}

	db, err = sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	read := func(key string) string {
		t.Helper()
		var value string
		_ = db.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, key).Scan(&value)
		return value
	}
	if read("cursorAuth/accessToken") != token || read("cursorAuth/refreshToken") != token {
		t.Fatal("complete Cursor 3.19 token pair was not written")
	}
	if read("cursorAuth/cachedEmail") != "new@example.com" || read("cursorAuth/cachedUserID") != "user_01NEW" {
		t.Fatal("canonical IDE identity keys were not written")
	}
	for _, key := range []string{"cursorAuth/isAuthenticated", "cursorAuth/isAuthorized", "cursorAuth/isLoggedIn"} {
		if read(key) != "true" {
			t.Fatalf("%s = %q, want true", key, read(key))
		}
	}
	for _, key := range []string{"cursorAuth/teamId", "cursorAuth/cachedTeam", "cursorAuth/cachedScopedProfile", "cursorAuth/stripeMembershipType", "cursorAuth/stripeMembershipAuthId", "cursorAuth/stripeSubscriptionStatus"} {
		if value := read(key); value != "" {
			t.Fatalf("stale %s was not cleared: %q", key, value)
		}
	}
}
