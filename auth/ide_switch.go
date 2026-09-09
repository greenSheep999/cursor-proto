package auth

import (
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var (
	ideSessionJWTRE = regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+`)
	ideEmailRE      = regexp.MustCompile(`[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
)

// IDELoginCredential is the complete identity tuple Cursor needs in
// state.vscdb. Cursor 3.19 considers a user authenticated only when both the
// access and refresh values are non-empty, even though ordinary API requests
// can succeed with the access token alone.
type IDELoginCredential struct {
	Email        string
	UserID       string
	AuthID       string
	AuthType     string
	AccessToken  string
	RefreshToken string
}

// ParseIDELoginCredential accepts a bare session JWT, user_id::JWT, or one of
// the exported delimiter-separated account lines used by Cursor account
// tools. When no separate refresh token is available, a session JWT is also
// used as the refresh value. Cursor itself has persisted this shape, and it is
// required for Cursor 3.19's accessToken && refreshToken login predicate.
func ParseIDELoginCredential(raw, emailOverride, rawRefresh string, now time.Time) (*IDELoginCredential, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, fmt.Errorf("empty IDE login credential")
	}
	if decoded, err := url.QueryUnescape(value); err == nil {
		value = decoded
	}
	access := findSessionJWT(value)
	if access == "" {
		return nil, fmt.Errorf("credential does not contain a Cursor session JWT")
	}
	claims, err := DecodeJWTClaims(access)
	if err != nil {
		return nil, fmt.Errorf("decode session JWT: %w", err)
	}
	if tokenType := strings.ToLower(strings.TrimSpace(claims.Type)); tokenType != "session" {
		return nil, fmt.Errorf("IDE login JWT has type %q, want %q", tokenType, "session")
	}
	if claims.Exp > 0 && !now.IsZero() && claims.Exp <= now.Unix() {
		return nil, fmt.Errorf("IDE login JWT expired at %s", time.Unix(claims.Exp, 0).UTC().Format(time.RFC3339))
	}
	authID := strings.TrimSpace(claims.Sub)
	if authID == "" {
		return nil, fmt.Errorf("IDE login JWT has no sub claim")
	}
	userID := authID
	if i := strings.LastIndexByte(userID, '|'); i >= 0 {
		userID = userID[i+1:]
	}
	if userID == "" {
		return nil, fmt.Errorf("IDE login JWT has an empty user id")
	}

	refresh := strings.TrimSpace(rawRefresh)
	if refresh != "" {
		if decoded, decodeErr := url.QueryUnescape(refresh); decodeErr == nil {
			refresh = decoded
		}
		if parsed := findSessionJWT(refresh); parsed != "" {
			refresh = parsed
		}
	} else {
		// Cursor 3.19's isAuthenticated() is literally accessToken &&
		// refreshToken. A session access token is accepted as the fallback
		// refresh credential and matches login states produced by Cursor.
		refresh = access
	}

	email := strings.TrimSpace(emailOverride)
	if email == "" {
		email = ideEmailRE.FindString(value)
	}
	if email == "" {
		return nil, fmt.Errorf("email is required when the credential does not contain one")
	}

	return &IDELoginCredential{
		Email:        email,
		UserID:       userID,
		AuthID:       authID,
		AuthType:     ideAuthType(authID),
		AccessToken:  access,
		RefreshToken: refresh,
	}, nil
}

func findSessionJWT(value string) string {
	for _, token := range ideSessionJWTRE.FindAllString(value, -1) {
		if TokenType(token) == "session" {
			return token
		}
	}
	return ""
}

func ideAuthType(authID string) string {
	switch {
	case strings.HasPrefix(strings.ToLower(authID), "auth0|"):
		return "Auth_0"
	case strings.HasPrefix(strings.ToLower(authID), "workos|"):
		return "WorkOS"
	default:
		return "unknown"
	}
}

// WriteIDELoginCredential transactionally replaces the Cursor IDE identity.
// The caller must stop Cursor before calling this function; otherwise the
// running workbench can overwrite the new rows from its in-memory state.
func WriteIDELoginCredential(dbPath string, credential *IDELoginCredential) error {
	if credential == nil {
		return fmt.Errorf("nil IDE login credential")
	}
	if strings.TrimSpace(credential.AccessToken) == "" || strings.TrimSpace(credential.RefreshToken) == "" {
		return fmt.Errorf("both access and refresh tokens are required by Cursor 3.19")
	}
	if strings.TrimSpace(credential.Email) == "" || strings.TrimSpace(credential.UserID) == "" || strings.TrimSpace(credential.AuthID) == "" {
		return fmt.Errorf("email, user id, and auth id are required")
	}

	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return fmt.Errorf("open IDE state database: %w", err)
	}
	defer db.Close()
	if _, err := db.Exec(`PRAGMA busy_timeout = 10000`); err != nil {
		return fmt.Errorf("configure IDE state database: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin IDE account switch: %w", err)
	}
	defer tx.Rollback()

	rows := map[string]string{
		"cursorAuth/accessToken":      credential.AccessToken,
		"cursorAuth/refreshToken":     credential.RefreshToken,
		"cursorAuth/cachedEmail":      credential.Email,
		"cursorAuth/email":            credential.Email,
		"cursorAuth/cachedUserID":     credential.UserID,
		"cursorAuth/userId":           credential.AuthID,
		"cursorAuth/cachedUserId":     credential.AuthID,
		"cursorAuth/authId":           credential.AuthID,
		"cursorAuth/cachedSignUpType": credential.AuthType,
		"cursorAuth/isAuthenticated":  "true",
		"cursorAuth/isAuthorized":     "true",
		"cursorAuth/isLoggedIn":       "true",
		"cursor.accessToken":          credential.AccessToken,
		"cursor.email":                credential.Email,
	}
	for key, value := range rows {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO ItemTable(key, value) VALUES (?, ?)`, key, value); err != nil {
			return fmt.Errorf("write IDE login key %s: %w", key, err)
		}
	}

	// These values belong to the previous identity. Cursor will repopulate
	// them from the authenticated dashboard after restart.
	stale := []string{
		"cursorAuth/teamId",
		"cursorAuth/cachedTeam",
		"cursorAuth/cachedScopedProfile",
		"cursorAuth/stripeCustomerId",
		"cursorAuth/stripeMembershipType",
		"cursorAuth/stripeMembershipAuthId",
		"cursorAuth/stripeSubscriptionStatus",
	}
	for _, key := range stale {
		if _, err := tx.Exec(`DELETE FROM ItemTable WHERE key = ?`, key); err != nil {
			return fmt.Errorf("clear stale IDE login key %s: %w", key, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit IDE account switch: %w", err)
	}
	return nil
}
