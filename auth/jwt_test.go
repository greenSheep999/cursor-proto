package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"
)

// makeJWT synthesises a token with a specific iat/exp payload. The
// signature segment is bogus — good enough because DecodeJWTClaims does
// not verify it.
func makeJWT(t *testing.T, iat, exp int64) string {
	t.Helper()
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	body, err := json.Marshal(struct {
		Iat int64 `json:"iat,omitempty"`
		Exp int64 `json:"exp,omitempty"`
	}{Iat: iat, Exp: exp})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	payload := base64.RawURLEncoding.EncodeToString(body)
	sig := base64.RawURLEncoding.EncodeToString([]byte("fake"))
	return head + "." + payload + "." + sig
}

func TestDecodeJWTClaims(t *testing.T) {
	tok := makeJWT(t, 1_700_000_000, 1_700_003_600)
	claims, err := DecodeJWTClaims(tok)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if claims.Iat != 1_700_000_000 || claims.Exp != 1_700_003_600 {
		t.Errorf("unexpected claims: %+v", claims)
	}
}

func TestIssuedAtFromJWT(t *testing.T) {
	tok := makeJWT(t, 1_700_000_000, 0)
	got := IssuedAtFromJWT(tok)
	want := time.Unix(1_700_000_000, 0).UTC()
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if !IssuedAtFromJWT("not-a-jwt").IsZero() {
		t.Errorf("expected zero time for non-JWT input")
	}
}

func TestExpiresAtFromJWT(t *testing.T) {
	tok := makeJWT(t, 0, 1_700_003_600)
	got := ExpiresAtFromJWT(tok)
	want := time.Unix(1_700_003_600, 0).UTC()
	if !got.Equal(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestTokenType(t *testing.T) {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"type":"web"}`))
	if got := TokenType(head + "." + payload + ".sig"); got != "web" {
		t.Fatalf("TokenType = %q, want web", got)
	}
}
