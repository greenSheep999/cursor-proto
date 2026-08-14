package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// Cursor's OAuth-style device login flow, extracted from
// reference/js-src/cursorLogin.js and verified against a live capture and IDE
// behaviour.
//
// Flow:
//  1. Client generates a PKCE (verifier, challenge) pair and a UUID.
//  2. Client opens
//     https://www.cursor.com/loginDeepControl?challenge=<c>&uuid=<u>&mode=login
//     in the user's browser.
//  3. User authenticates on cursor.com (email/OTP/etc), authorizing the login.
//  4. Client polls https://api2.cursor.sh/auth/poll?uuid=&verifier= until it
//     returns 200 with { accessToken, refreshToken, authId }.

const (
	LoginPageURL = "https://www.cursor.com/loginDeepControl"
	PollURL      = "https://api2.cursor.sh/auth/poll"

	loginUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
		"(KHTML, like Gecko) Cursor/0.48.6 Chrome/132.0.6834.210 " +
		"Electron/34.3.4 Safari/537.36"
)

// PKCEPair holds a PKCE verifier/challenge tuple.
type PKCEPair struct {
	Verifier  string
	Challenge string
}

// NewPKCEPair generates a fresh PKCE pair (43 bytes → 58-char base64url
// verifier, sha256+base64url challenge).
func NewPKCEPair() (*PKCEPair, error) {
	buf := make([]byte, 43)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	return &PKCEPair{Verifier: verifier, Challenge: challenge}, nil
}

// LoginSession represents an in-flight login attempt.
type LoginSession struct {
	UUID     string
	PKCE     *PKCEPair
	LoginURL string
	HTTP     *http.Client
	// PollURLBase overrides the api2 poll endpoint. Empty means "use the
	// default PollURL". Useful for testing the batch login CLI against a
	// mock server.
	PollURLBase string
	// LoginCallbackURL overrides the cursor.com callback endpoint used by
	// AuthorizeWithWebSession. Empty means the production endpoint.
	LoginCallbackURL string
}

// StartLogin creates a fresh login session. Present LoginURL to the user in a
// browser, then call Poll or WaitForLogin.
func StartLogin() (*LoginSession, error) {
	pkce, err := NewPKCEPair()
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	url := fmt.Sprintf("%s?challenge=%s&uuid=%s&mode=login", LoginPageURL, pkce.Challenge, id)
	return &LoginSession{
		UUID:     id,
		PKCE:     pkce,
		LoginURL: url,
		HTTP:     &http.Client{Timeout: 15 * time.Second},
	}, nil
}

// PollResult is the successful poll response body from api2.cursor.sh.
type PollResult struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	AuthID       string `json:"authId"`   // "auth0|user_..." or "workos|user_..."
	Type         string `json:"authType"` // "Auth_0" etc
}

// Poll performs a single non-blocking check.
// Returns (nil, nil) if not ready, (result, nil) on success.
func (s *LoginSession) Poll(ctx context.Context) (*PollResult, error) {
	base := s.PollURLBase
	if base == "" {
		base = PollURL
	}
	url := fmt.Sprintf("%s?uuid=%s&verifier=%s", base, s.UUID, s.PKCE.Verifier)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", loginUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, nil
	}
	var pr PollResult
	if err := json.Unmarshal(body, &pr); err != nil {
		return nil, fmt.Errorf("decode poll response: %w (body=%s)", err, string(body))
	}
	if pr.AccessToken == "" {
		return nil, nil
	}
	return &pr, nil
}

// WaitForLogin polls every `interval` up to `timeout`.
// Returns the completed poll response or an error.
func (s *LoginSession) WaitForLogin(ctx context.Context, interval, timeout time.Duration) (*PollResult, error) {
	deadline := time.Now().Add(timeout)
	tick := time.NewTicker(interval)
	defer tick.Stop()

	// Prime with an immediate poll
	if r, err := s.Poll(ctx); err != nil {
		return nil, err
	} else if r != nil {
		return r, nil
	}

	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-tick.C:
			if time.Now().After(deadline) {
				return nil, fmt.Errorf("login timeout after %s", timeout)
			}
			r, err := s.Poll(ctx)
			if err != nil {
				return nil, err
			}
			if r != nil {
				return r, nil
			}
		}
	}
}
