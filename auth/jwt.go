package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// JWTClaims is a minimal subset of the JWT payload we care about: issued-at
// and expiration timestamps. Cursor's access tokens are Auth0-issued JWTs
// carrying at least "iat", "exp" and often "sub".
type JWTClaims struct {
	Sub             string `json:"sub,omitempty"`
	Type            string `json:"type,omitempty"`
	Iat             int64  `json:"iat,omitempty"`
	Nbf             int64  `json:"nbf,omitempty"`
	Exp             int64  `json:"exp,omitempty"`
	WorkOSSessionID string `json:"workosSessionId,omitempty"`
}

// TokenType returns Cursor's token-purpose claim (for example "web" or
// "session"). An empty string means the token is malformed or has no type.
func TokenType(token string) string {
	claims, err := DecodeJWTClaims(token)
	if err != nil || claims == nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(claims.Type))
}

// DecodeJWTClaims parses the payload segment of a JWT-shaped access token.
// The signature is not validated — callers only use this to surface human
// hints (issued-at, expiration). Returns an error if the token has fewer
// than three segments or the payload is not valid base64url JSON.
func DecodeJWTClaims(token string) (*JWTClaims, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("empty token")
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("token is not a JWT (only %d segment(s))", len(parts))
	}
	seg := parts[1]
	// Base64url with optional padding.
	if pad := len(seg) % 4; pad != 0 {
		seg += strings.Repeat("=", 4-pad)
	}
	raw, err := base64.URLEncoding.DecodeString(seg)
	if err != nil {
		// Try again with the un-padded variant.
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(seg, "="))
		if err != nil {
			return nil, fmt.Errorf("decode payload: %w", err)
		}
	}
	var claims JWTClaims
	if err := json.Unmarshal(raw, &claims); err != nil {
		return nil, fmt.Errorf("parse payload json: %w", err)
	}
	return &claims, nil
}

// IssuedAtFromJWT returns the token's iat/nbf as a Go time, or the zero
// value when neither is present. Uses iat when set, nbf as fallback.
func IssuedAtFromJWT(token string) time.Time {
	c, err := DecodeJWTClaims(token)
	if err != nil || c == nil {
		return time.Time{}
	}
	switch {
	case c.Iat > 0:
		return time.Unix(c.Iat, 0).UTC()
	case c.Nbf > 0:
		return time.Unix(c.Nbf, 0).UTC()
	}
	return time.Time{}
}

// ExpiresAtFromJWT returns the token's exp claim as a Go time, or the zero
// value when unset or the token is malformed.
func ExpiresAtFromJWT(token string) time.Time {
	c, err := DecodeJWTClaims(token)
	if err != nil || c == nil {
		return time.Time{}
	}
	if c.Exp > 0 {
		return time.Unix(c.Exp, 0).UTC()
	}
	return time.Time{}
}
