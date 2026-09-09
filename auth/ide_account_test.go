package auth

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLoadAccountFromIDEDBUsesCachedServerConfigVersion(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{
		"cursorAuth/accessToken": "opaque-session-token",
		"cursorai/serverConfig":  `{"configVersion":"server-config-v7","nested":{"configVersion":"wrong-nested-value"}}`,
	} {
		if _, err := db.Exec(`INSERT INTO ItemTable(key, value) VALUES (?, ?)`, key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	account, err := LoadAccountFromIDEDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if account.ConfigVersion != "server-config-v7" {
		t.Fatalf("config version = %q, want server-config-v7", account.ConfigVersion)
	}
}

func TestServerConfigVersionRejectsMalformedOrMissingValues(t *testing.T) {
	for _, input := range []string{"", `{}`, `{broken`, `{"configVersion":7}`} {
		if got := serverConfigVersion(input); got != "" {
			t.Fatalf("serverConfigVersion(%q) = %q, want empty", input, got)
		}
	}
}
