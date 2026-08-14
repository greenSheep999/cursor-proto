package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const (
	WebSessionCookieName = "WorkosCursorSessionToken"
	LoginCallbackURL     = "https://cursor.com/api/auth/loginDeepCallbackControl"
)

// WebSessionCredential is Cursor's website session cookie split into the
// user id prefix and the JWT carried after "::".
type WebSessionCredential struct {
	UserID      string
	JWT         string
	CookieValue string
}

// ParseWebSessionCredential accepts raw or URL-encoded user_id::JWT values.
func ParseWebSessionCredential(raw string) (*WebSessionCredential, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return nil, fmt.Errorf("empty web session credential")
	}
	if decoded, err := url.QueryUnescape(value); err == nil {
		value = decoded
	}
	userID, token, ok := strings.Cut(value, "::")
	if !ok || strings.TrimSpace(userID) == "" || strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("web session credential must be user_id::JWT")
	}
	userID = strings.TrimSpace(userID)
	token = strings.TrimSpace(token)
	if TokenType(token) != "web" {
		return nil, fmt.Errorf("web session JWT has type %q, want %q", TokenType(token), "web")
	}
	claims, err := DecodeJWTClaims(token)
	if err != nil {
		return nil, err
	}
	if claims.Sub != "" && !strings.HasSuffix(claims.Sub, "|"+userID) {
		return nil, fmt.Errorf("web session user id %q does not match JWT sub %q", userID, claims.Sub)
	}
	return &WebSessionCredential{
		UserID:      userID,
		JWT:         token,
		CookieValue: userID + "::" + token,
	}, nil
}

// AuthorizeWithWebSession authorizes an existing PKCE login session using a
// Cursor website session. baseCookieHeader must come from a normally signed-in
// cursor.com browser session; the target WorkosCursorSessionToken is replaced
// while companion WorkOS/device cookies are preserved.
func (s *LoginSession) AuthorizeWithWebSession(ctx context.Context, credential *WebSessionCredential, baseCookieHeader string) error {
	if s == nil || s.PKCE == nil || s.UUID == "" {
		return fmt.Errorf("invalid login session")
	}
	if credential == nil {
		return fmt.Errorf("nil web session credential")
	}
	cookieHeader, err := replaceWebSessionCookie(baseCookieHeader, credential.CookieValue)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"uuid":      s.UUID,
		"challenge": s.PKCE.Challenge,
	})
	if err != nil {
		return fmt.Errorf("marshal login callback: %w", err)
	}
	endpoint := s.LoginCallbackURL
	if endpoint == "" {
		endpoint = LoginCallbackURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("build login callback: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("User-Agent", loginUserAgent)
	req.Header.Set("Origin", "https://cursor.com")
	req.Header.Set("Referer", s.LoginURL)
	req.Header.Set("Cookie", cookieHeader)

	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("authorize web session: %w", err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("authorize web session HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}
	return nil
}

func replaceWebSessionCookie(baseHeader, value string) (string, error) {
	baseHeader = strings.TrimSpace(baseHeader)
	if baseHeader == "" {
		return "", fmt.Errorf("base cookie header is required; sign in with a normal account first and export the cursor.com Cookie header")
	}

	var (
		cookies        []string
		hasCompanion   bool
		targetReplaced bool
	)
	for _, part := range strings.Split(baseHeader, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, _, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(name) == "" {
			continue
		}
		name = strings.TrimSpace(name)
		if strings.EqualFold(name, WebSessionCookieName) {
			cookies = append(cookies, WebSessionCookieName+"="+url.QueryEscape(value))
			targetReplaced = true
			continue
		}
		if strings.Contains(strings.ToLower(name), "workos") {
			hasCompanion = true
		}
		cookies = append(cookies, part)
	}
	if !hasCompanion {
		return "", fmt.Errorf("base cookie header has no companion WorkOS cookie; complete a normal cursor.com login before replacing %s", WebSessionCookieName)
	}
	if !targetReplaced {
		cookies = append(cookies, WebSessionCookieName+"="+url.QueryEscape(value))
	}
	return strings.Join(cookies, "; "), nil
}
