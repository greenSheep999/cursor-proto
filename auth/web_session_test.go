package auth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func typedJWT(t *testing.T, tokenType, sub string) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"type": tokenType,
		"sub":  sub,
		"exp":  1_900_000_000,
	})
	if err != nil {
		t.Fatalf("marshal JWT: %v", err)
	}
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".sig"
}

func TestParseWebSessionCredential(t *testing.T) {
	token := typedJWT(t, "web", "auth0|user_abc")
	credential, err := ParseWebSessionCredential("user_abc%3A%3A" + token)
	if err != nil {
		t.Fatalf("ParseWebSessionCredential: %v", err)
	}
	if credential.UserID != "user_abc" || credential.JWT != token {
		t.Fatalf("unexpected credential: %+v", credential)
	}
}

func TestParseWebSessionCredentialRejectsSessionJWT(t *testing.T) {
	token := typedJWT(t, "session", "auth0|user_abc")
	if _, err := ParseWebSessionCredential("user_abc::" + token); err == nil {
		t.Fatal("expected session JWT to be rejected")
	}
}

func TestAuthorizeWithWebSessionPreservesCompanionCookies(t *testing.T) {
	token := typedJWT(t, "web", "auth0|user_abc")
	credential, err := ParseWebSessionCredential("user_abc::" + token)
	if err != nil {
		t.Fatal(err)
	}

	var gotCookie string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCookie = r.Header.Get("Cookie")
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["uuid"] != "uuid-1" || body["challenge"] != "challenge-1" {
			t.Errorf("unexpected body: %v", body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer server.Close()

	session := &LoginSession{
		UUID:             "uuid-1",
		PKCE:             &PKCEPair{Challenge: "challenge-1"},
		LoginURL:         "https://cursor.com/loginDeepControl?uuid=uuid-1",
		HTTP:             server.Client(),
		LoginCallbackURL: server.URL,
	}
	base := "workos-device=normal-session; WorkosCursorSessionToken=old; other=value"
	if err := session.AuthorizeWithWebSession(context.Background(), credential, base); err != nil {
		t.Fatalf("AuthorizeWithWebSession: %v", err)
	}
	if !strings.Contains(gotCookie, "workos-device=normal-session") {
		t.Fatalf("companion cookie missing: %s", gotCookie)
	}
	if strings.Contains(gotCookie, "WorkosCursorSessionToken=old") {
		t.Fatalf("old target cookie was not replaced: %s", gotCookie)
	}
	if !strings.Contains(gotCookie, "WorkosCursorSessionToken=user_abc%3A%3A") {
		t.Fatalf("new target cookie missing: %s", gotCookie)
	}
}

func TestAuthorizeWithWebSessionRequiresCompanionWorkOSCookie(t *testing.T) {
	token := typedJWT(t, "web", "auth0|user_abc")
	credential, err := ParseWebSessionCredential("user_abc::" + token)
	if err != nil {
		t.Fatal(err)
	}
	session := &LoginSession{UUID: "uuid", PKCE: &PKCEPair{Challenge: "challenge"}}
	err = session.AuthorizeWithWebSession(context.Background(), credential, "other=value")
	if err == nil || !strings.Contains(err.Error(), "companion WorkOS cookie") {
		t.Fatalf("unexpected error: %v", err)
	}
}
