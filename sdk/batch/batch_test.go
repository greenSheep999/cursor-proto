package batch

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func TestReadRowsCSV(t *testing.T) {
	dir := t.TempDir()
	csv := "email,access_token,refresh_token\n" +
		"a@icloud.com,acc-a,ref-a\n" +
		"b@gmail.com,acc-b,\n"
	p := writeFile(t, dir, "tokens.csv", csv)

	rows, err := ReadRows(p)
	if err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(rows))
	}
	if rows[0].Email != "a@icloud.com" || rows[0].AccessToken != "acc-a" || rows[0].RefreshToken != "ref-a" {
		t.Errorf("row 0 = %+v", rows[0])
	}
	if rows[1].RefreshToken != "" {
		t.Errorf("row 1 refresh should be empty: %+v", rows[1])
	}
}

func TestReadRowsJSON(t *testing.T) {
	dir := t.TempDir()
	blob := `[{"email":"c@example.com","access_token":"tt","refresh_token":"rr","auth_id":"auth0|user_x","auth_type":"Auth_0"}]`
	p := writeFile(t, dir, "tokens.json", blob)

	rows, err := ReadRows(p)
	if err != nil {
		t.Fatalf("ReadRows: %v", err)
	}
	if len(rows) != 1 || rows[0].AuthID != "auth0|user_x" {
		t.Fatalf("unexpected rows: %+v", rows)
	}
}

func TestReadRowsRejectsUnknownExt(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "tokens.txt", "no")
	if _, err := ReadRows(p); err == nil {
		t.Fatal("expected error for .txt")
	}
}

func TestReadEmailsFile(t *testing.T) {
	dir := t.TempDir()
	p := writeFile(t, dir, "emails.txt", "a@example.com\n# comment\nb@example.com\n\n  c@example.com  \n")
	got, err := ReadEmailsFile(p)
	if err != nil {
		t.Fatalf("ReadEmailsFile: %v", err)
	}
	want := []string{"a@example.com", "b@example.com", "c@example.com"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("index %d: %q vs %q", i, got[i], want[i])
		}
	}
}

func makeJWT(t *testing.T, iat, exp int64) string {
	t.Helper()
	body, err := json.Marshal(struct {
		Iat int64 `json:"iat,omitempty"`
		Exp int64 `json:"exp,omitempty"`
	}{Iat: iat, Exp: exp})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(body) + "." +
		base64.RawURLEncoding.EncodeToString([]byte("sig"))
}

func makeTypedJWT(t *testing.T, tokenType string) string {
	t.Helper()
	body, err := json.Marshal(struct {
		Type string `json:"type"`
	}{Type: tokenType})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(body) + ".sig"
}

func TestAccountFromRow_DerivesFromJWT(t *testing.T) {
	tok := makeJWT(t, 1_700_000_000, 1_700_003_600)
	row := InputRow{
		Email:        "d@example.com",
		AccessToken:  tok,
		RefreshToken: "r",
		AuthID:       "auth0|user_y",
	}
	acc, err := AccountFromRow(row, "machine", "mac")
	if err != nil {
		t.Fatalf("AccountFromRow: %v", err)
	}
	if acc.UserID != "user_y" {
		t.Errorf("UserID = %q, want user_y", acc.UserID)
	}
	if !acc.Refreshable {
		t.Errorf("Refreshable should be true (row has refresh_token)")
	}
	if acc.IssuedAt.Unix() != 1_700_000_000 {
		t.Errorf("IssuedAt not derived from JWT: %v", acc.IssuedAt)
	}
	if acc.ExpiresAt.Unix() != 1_700_003_600 {
		t.Errorf("ExpiresAt not derived from JWT: %v", acc.ExpiresAt)
	}
}

func TestAccountFromRow_MarksNonRefreshable(t *testing.T) {
	row := InputRow{Email: "e@example.com", AccessToken: "tok"}
	acc, err := AccountFromRow(row, "", "")
	if err != nil {
		t.Fatalf("AccountFromRow: %v", err)
	}
	if acc.Refreshable {
		t.Fatal("expected Refreshable=false when refresh token is absent")
	}
}

func TestAccountFromRowRejectsWebToken(t *testing.T) {
	row := InputRow{Email: "web@example.com", AccessToken: makeTypedJWT(t, "web")}
	if _, err := AccountFromRow(row, "", ""); err == nil {
		t.Fatal("expected type=web token to be rejected")
	}
}
