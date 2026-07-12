package kernel

// Cursor magic-code (OTP) login flow, driven entirely from HTTP.
//
// The Python reference (muxhub/scripts/login-hub/helpers/cursor.py)
// runs a real Camoufox browser through eight steps. We shortcut most
// of them by talking to authenticator.cursor.sh directly:
//
//   1. StartLogin/otp:
//      a. GET https://authenticator.cursor.sh/?client_id=...&redirect_uri=...&state=...&authorization_session_id=<fresh-uuid>
//         Save WorkOSSession cookie; extract Turnstile sitekey and
//         Next.js server-action id from the HTML.
//      b. Ask YesCaptcha to solve the Turnstile challenge (~15-40s).
//      c. POST the same URL with a multipart body encoding an
//         email + intent=magic-code + the Turnstile token + a
//         fingerprint signals blob. Cursor answers 303 to
//         /magic-code?authentication_challenge_id=<id>.
//      d. Store cookies, challenge id, action id, and IMAP config
//         in the sync.Map keyed by the returned state uuid. Return
//         State + Metadata{mode:otp, otp_pending:true, magic_state}.
//
//   2. PollLogin/otp:
//      a. Either use metadata.otp literally, or poll the IMAP inbox
//         for a fresh cursor.sh email until the 6-digit code arrives.
//      b. Solve a fresh Turnstile (magic-code page has its own
//         challenge).
//      c. POST /magic-code with the code — Cursor answers 303 back to
//         /? which then 303s to cursor.com/api/auth/callback?code=...
//      d. Follow the callback chain manually so we can pluck the
//         final `code` param.
//      e. Call cursor.com/api/auth/me with the resulting session
//         cookie to harvest accessToken/refreshToken.
//      f. Materialise a full AuthData.

import (
	"context"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Cursor authenticator + callback endpoints. These have been stable
// for years; if Cursor ever changes them the plugin will surface
// clean HTTP errors before hitting anything else.
const (
	cursorAuthHost      = "authenticator.cursor.sh"
	cursorAuthURLScheme = "https"
	cursorClientID      = "client_01GS6W3C96KW4WRS6Z93JCE2RJ"
	cursorRedirectURI   = "https://cursor.com/api/auth/callback"
	cursorAuthMeURL     = "https://cursor.com/api/auth/me"

	// Chrome UA — matches the signals fixture. Cursor's WAF is more
	// permissive of desktop-Chrome shapes than of curl/Go defaults.
	otpUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
)

// fallbackNextAction is the last-known-good Next.js server-action hash
// for authenticator.cursor.sh. Extracted from a captured request.
// We prefer to scrape a fresh value out of the HTML on every login —
// this is the safety net if the regex misses.
const fallbackNextAction = "fef846a39073c935bea71b63308b177b113269b7"

// otpState is the per-login blob we keep in sync.Map.
type otpState struct {
	Email          string
	YesCaptchaKey  string
	Client         *http.Client
	NextAction     string
	AuthURL        string // /?client_id=…&state=… (the / endpoint)
	State          string // WorkOS state (URL-encoded JSON we sent)
	SessionID      string // authorization_session_id
	ChallengeID    string // returned by /magic-code redirect
	IMAP           *imapConfig
	LiteralOTP     string // if operator passed metadata.otp
	Since          int64  // epoch of Start; only accept mails newer than this
	AuthMeEndpoint string // "" → default cursorAuthMeURL; tests override
	AuthBase       string // "https://authenticator.cursor.sh" or test override
	YesCaptchaBase string // "" → default; tests override
}

// startCursorOTP runs steps 1a-1d and returns the challenge id + the
// otpState that Poll will consume.
func startCursorOTP(ctx context.Context, email string, yescaptchaKey string, imap *imapConfig, literalOTP string, opts otpStartOptions) (*otpState, error) {
	authBase := opts.AuthBase
	if authBase == "" {
		authBase = cursorAuthURLScheme + "://" + cursorAuthHost
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("cookiejar: %w", err)
	}
	client := &http.Client{
		Jar: jar,
		// Do NOT follow redirects — we need to inspect Location for the
		// challenge id, and for the eventual callback we need the raw
		// hops so we can lift the ?code=… parameter.
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: opts.Transport,
	}

	sessionID := newUUID()
	stateJSON := fmt.Sprintf(`{"returnTo":"https://cursor.com/dashboard","nonce":"%s"}`, newUUID())
	stateEnc := url.QueryEscape(stateJSON)

	authURL := fmt.Sprintf("%s/?client_id=%s&redirect_uri=%s&state=%s&authorization_session_id=%s",
		authBase, cursorClientID, url.QueryEscape(cursorRedirectURI), stateEnc, sessionID)

	// 1a. GET the entry page, extract sitekey + next-action id, keep cookies.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, authURL, nil)
	if err != nil {
		return nil, err
	}
	setBrowserHeaders(req)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET authenticator: %w", err)
	}
	body, err := drainBody(resp)
	if err != nil {
		return nil, fmt.Errorf("read authenticator body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("authenticator returned HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))
	}

	sitekey := extractSitekey(string(body))
	if sitekey == "" {
		return nil, fmt.Errorf("could not find Turnstile sitekey in authenticator HTML (Cursor may have changed the page — first 200 chars: %s)", truncate(string(body), 200))
	}
	nextAction := extractNextAction(string(body))
	if nextAction == "" {
		nextAction = fallbackNextAction
	}

	// 1b. Solve Turnstile via YesCaptcha.
	solver := &yescaptchaSolver{
		APIKey:  yescaptchaKey,
		BaseURL: opts.YesCaptchaBase,
	}
	token, err := solver.solve(ctx, authURL, sitekey)
	if err != nil {
		return nil, fmt.Errorf("turnstile solve: %w", err)
	}

	// 1c. POST the multipart form with intent=magic-code.
	multipartBody, contentType, err := buildAuthMultipart(authForm{
		Token:       token,
		Email:       email,
		Password:    "",
		Intent:      "magic-code",
		State:       stateEnc,
		SessionID:   sessionID,
		RedirectURI: cursorRedirectURI,
		Signals:     buildSignals(time.Now()),
	})
	if err != nil {
		return nil, fmt.Errorf("build multipart: %w", err)
	}

	post, err := http.NewRequestWithContext(ctx, http.MethodPost, authURL, strings.NewReader(multipartBody))
	if err != nil {
		return nil, err
	}
	setBrowserHeaders(post)
	post.Header.Set("Content-Type", contentType)
	post.Header.Set("Next-Action", nextAction)
	post.Header.Set("Origin", authBase)
	post.Header.Set("Referer", authURL)

	postResp, err := client.Do(post)
	if err != nil {
		return nil, fmt.Errorf("POST authenticator: %w", err)
	}
	postBody, _ := drainBody(postResp)

	// Cursor answers with a 303 to /magic-code?authentication_challenge_id=...
	// or, on error, a 200 with a JSON error body embedded in Next.js output.
	challengeID := ""
	if postResp.StatusCode >= 300 && postResp.StatusCode < 400 {
		loc := postResp.Header.Get("Location")
		if loc == "" {
			// Newer Next.js server actions redirect via x-action-redirect
			// header — accept either.
			loc = postResp.Header.Get("X-Action-Redirect")
		}
		challengeID = extractChallengeID(loc)
	}
	if challengeID == "" {
		challengeID = extractChallengeID(string(postBody))
	}
	if challengeID == "" {
		if postResp.StatusCode >= 400 {
			return nil, fmt.Errorf("authenticator rejected form submit — likely fingerprint blocked (HTTP %d, raw body: %s)",
				postResp.StatusCode, truncate(string(postBody), 200))
		}
		return nil, fmt.Errorf("authenticator did not return magic-code challenge (HTTP %d, raw body: %s)",
			postResp.StatusCode, truncate(string(postBody), 200))
	}

	authMeEndpoint := opts.AuthMeEndpoint
	if authMeEndpoint == "" {
		authMeEndpoint = cursorAuthMeURL
	}

	return &otpState{
		Email:          email,
		YesCaptchaKey:  yescaptchaKey,
		Client:         client,
		NextAction:     nextAction,
		AuthURL:        authURL,
		State:          stateEnc,
		SessionID:      sessionID,
		ChallengeID:    challengeID,
		IMAP:           imap,
		LiteralOTP:     literalOTP,
		Since:          time.Now().Unix(),
		AuthMeEndpoint: authMeEndpoint,
		AuthBase:       authBase,
		YesCaptchaBase: opts.YesCaptchaBase,
	}, nil
}

// otpStartOptions is the tests-and-callers knob bundle. In production
// callers leave all fields zero.
type otpStartOptions struct {
	AuthBase       string            // override authenticator base URL
	AuthMeEndpoint string            // override auth/me endpoint
	YesCaptchaBase string            // override YesCaptcha base URL
	Transport      http.RoundTripper // shared transport (tests)
}

// pollCursorOTP performs steps 2a-2f. It returns an authFile when the
// login succeeds, or (nil, otpPending, nil) when we need to be polled
// again (e.g. the mail hasn't arrived yet).
type otpPollOutcome int

const (
	otpOutcomeSuccess otpPollOutcome = iota
	otpOutcomePending
	otpOutcomeError
)

type otpPollResult struct {
	Outcome      otpPollOutcome
	Message      string
	AccessToken  string
	RefreshToken string
	UserID       string
	AuthID       string
	AuthKind     string
	Email        string
}

func pollCursorOTP(ctx context.Context, s *otpState) *otpPollResult {
	// 2a. Get the OTP code.
	code, outcome, msg := resolveOTPCode(s)
	if outcome != otpOutcomeSuccess {
		return &otpPollResult{Outcome: outcome, Message: msg}
	}

	// 2b. Solve a fresh Turnstile for /magic-code.
	magicURL := fmt.Sprintf("%s/magic-code?authentication_challenge_id=%s", s.AuthBase, url.QueryEscape(s.ChallengeID))

	// Fetch the /magic-code page to re-extract sitekey (may differ).
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, magicURL, nil)
	if err != nil {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("build magic-code GET: %v", err)}
	}
	setBrowserHeaders(req)
	resp, err := s.Client.Do(req)
	if err != nil {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("GET /magic-code: %v", err)}
	}
	body, _ := drainBody(resp)
	if resp.StatusCode >= 400 {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("GET /magic-code HTTP %d: %s", resp.StatusCode, truncate(string(body), 200))}
	}
	sitekey := extractSitekey(string(body))
	nextAction := extractNextAction(string(body))
	if nextAction == "" {
		nextAction = s.NextAction
	}

	token := ""
	if sitekey != "" {
		solver := &yescaptchaSolver{
			APIKey:  s.YesCaptchaKey,
			BaseURL: s.YesCaptchaBase,
		}
		tok, err := solver.solve(ctx, magicURL, sitekey)
		if err != nil {
			return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("turnstile solve (magic-code): %v", err)}
		}
		token = tok
	}

	// 2c. POST the code.
	postBody, contentType, err := buildMagicCodeMultipart(code, token, s.State, s.ChallengeID, s.SessionID, s.Email)
	if err != nil {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("build magic-code multipart: %v", err)}
	}
	post, err := http.NewRequestWithContext(ctx, http.MethodPost, magicURL, strings.NewReader(postBody))
	if err != nil {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("build magic-code POST: %v", err)}
	}
	setBrowserHeaders(post)
	post.Header.Set("Content-Type", contentType)
	post.Header.Set("Next-Action", nextAction)
	post.Header.Set("Origin", s.AuthBase)
	post.Header.Set("Referer", magicURL)

	callbackCode, err := followUntilCallback(ctx, s.Client, post, 6)
	if err != nil {
		return &otpPollResult{Outcome: otpOutcomeError, Message: err.Error()}
	}
	if callbackCode == "" {
		return &otpPollResult{Outcome: otpOutcomeError, Message: "magic-code POST did not lead to cursor.com/api/auth/callback?code=…"}
	}

	// 2e. Ask cursor.com/api/auth/me for the tokens. The cookie jar
	// has the WorkOS session set on cursor.com after the callback
	// GET, so this request authenticates transparently.
	meReq, err := http.NewRequestWithContext(ctx, http.MethodGet, s.AuthMeEndpoint, nil)
	if err != nil {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("build auth/me: %v", err)}
	}
	setBrowserHeaders(meReq)
	meResp, err := s.Client.Do(meReq)
	if err != nil {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("GET auth/me: %v", err)}
	}
	meBody, _ := drainBody(meResp)
	if meResp.StatusCode >= 400 {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("auth/me HTTP %d: %s", meResp.StatusCode, truncate(string(meBody), 200))}
	}

	access, refresh, uid, authID, authKind, err := parseAuthMe(meBody)
	if err != nil {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("parse auth/me: %v", err)}
	}
	if access == "" {
		return &otpPollResult{Outcome: otpOutcomeError, Message: fmt.Sprintf("auth/me returned no access token (body: %s)", truncate(string(meBody), 200))}
	}
	return &otpPollResult{
		Outcome:      otpOutcomeSuccess,
		AccessToken:  access,
		RefreshToken: refresh,
		UserID:       uid,
		AuthID:       authID,
		AuthKind:     authKind,
		Email:        s.Email,
	}
}

// resolveOTPCode returns (code, success, ""), or (empty, pending/error, msg).
// A literal OTP short-circuits everything; otherwise we retry the
// inbox for up to otpInboxPollWindow within the current poll.
const otpInboxPollWindow = 60 * time.Second
const otpInboxPollInterval = 4 * time.Second

func resolveOTPCode(s *otpState) (string, otpPollOutcome, string) {
	if s.LiteralOTP != "" {
		return s.LiteralOTP, otpOutcomeSuccess, ""
	}
	if s.IMAP == nil || s.IMAP.Host == "" {
		return "", otpOutcomeError, "otp mode requires either metadata.otp or IMAP config (mail_host, mail_user, mail_pass)"
	}
	deadline := time.Now().Add(otpInboxPollWindow)
	var lastErr error
	for {
		code, fatal, err := fetchOTPFromInbox(*s.IMAP, s.Since-120) // 2-min slop
		if code != "" {
			return code, otpOutcomeSuccess, ""
		}
		if fatal {
			return "", otpOutcomeError, fmt.Sprintf("imap auth failed: %v", err)
		}
		if err != nil {
			lastErr = err
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return "", otpOutcomePending, fmt.Sprintf("waiting for cursor magic-code email (last imap error: %v)", lastErr)
			}
			return "", otpOutcomePending, "waiting for cursor magic-code email"
		}
		time.Sleep(otpInboxPollInterval)
	}
}

// buildAuthMultipart renders the multipart body that authenticator.cursor.sh
// expects for the initial password page action.
type authForm struct {
	Token       string
	Email       string
	Password    string
	Intent      string
	State       string
	SessionID   string
	RedirectURI string
	Signals     []byte
}

func buildAuthMultipart(f authForm) (string, string, error) {
	var buf strings.Builder
	w := multipart.NewWriter(&stringWriter{&buf})
	// Order matters for some Next.js action decoders. Match the Playwright capture.
	fields := []struct{ K, V string }{
		{"1_bot_detection_token", f.Token},
		{"1_signals", string(f.Signals)},
		{"1_email", f.Email},
		{"1_password", f.Password},
		{"1_intent", f.Intent},
		{"1_redirect_uri", f.RedirectURI},
		{"1_authorization_session_id", f.SessionID},
		{"1_state", f.State},
		{"0", `["$K1"]`},
	}
	for _, kv := range fields {
		if err := w.WriteField(kv.K, kv.V); err != nil {
			return "", "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", "", err
	}
	return buf.String(), w.FormDataContentType(), nil
}

// buildMagicCodeMultipart is the /magic-code counterpart. Cursor
// expects the code + a fresh Turnstile token.
func buildMagicCodeMultipart(code, token, state, challengeID, sessionID, email string) (string, string, error) {
	var buf strings.Builder
	w := multipart.NewWriter(&stringWriter{&buf})
	fields := []struct{ K, V string }{
		{"1_code", code},
		{"1_bot_detection_token", token},
		{"1_signals", string(buildSignals(time.Now()))},
		{"1_authentication_challenge_id", challengeID},
		{"1_authorization_session_id", sessionID},
		{"1_email", email},
		{"1_state", state},
		{"0", `["$K1"]`},
	}
	for _, kv := range fields {
		if err := w.WriteField(kv.K, kv.V); err != nil {
			return "", "", err
		}
	}
	if err := w.Close(); err != nil {
		return "", "", err
	}
	return buf.String(), w.FormDataContentType(), nil
}

// stringWriter adapts strings.Builder to io.Writer for multipart.NewWriter.
type stringWriter struct{ b *strings.Builder }

func (s *stringWriter) Write(p []byte) (int, error) { return s.b.Write(p) }

// followUntilCallback walks the redirect chain manually until it lands
// on cursor.com/api/auth/callback?code=…, returning the code. Returns
// "" if the chain never reaches the callback within maxHops.
func followUntilCallback(ctx context.Context, client *http.Client, req *http.Request, maxHops int) (string, error) {
	cur := req
	for hop := 0; hop < maxHops; hop++ {
		resp, err := client.Do(cur)
		if err != nil {
			return "", fmt.Errorf("hop %d: %w", hop, err)
		}
		body, _ := drainBody(resp)
		loc := resp.Header.Get("Location")
		if loc == "" {
			loc = resp.Header.Get("X-Action-Redirect")
		}
		if resp.StatusCode >= 400 {
			return "", fmt.Errorf("hop %d HTTP %d: %s", hop, resp.StatusCode, truncate(string(body), 200))
		}
		if loc == "" && resp.StatusCode >= 300 && resp.StatusCode < 400 {
			return "", fmt.Errorf("hop %d: redirect without Location", hop)
		}
		if loc == "" {
			// terminal response — sniff body for callback URL
			if code := extractCallbackCode(string(body)); code != "" {
				return code, nil
			}
			return "", nil
		}
		if code := extractCallbackCode(loc); code != "" {
			// Make one more request to consume the callback so cookies
			// get set on cursor.com. Not strictly necessary if auth/me
			// accepts the WorkOS cookie via the redirect, but tests
			// find it clarifying.
			next, err := http.NewRequestWithContext(ctx, http.MethodGet, loc, nil)
			if err == nil {
				setBrowserHeaders(next)
				if finalResp, err := client.Do(next); err == nil {
					_, _ = drainBody(finalResp)
				}
			}
			return code, nil
		}
		next, err := http.NewRequestWithContext(ctx, http.MethodGet, absURL(cur.URL, loc), nil)
		if err != nil {
			return "", fmt.Errorf("hop %d build request: %w", hop, err)
		}
		setBrowserHeaders(next)
		cur = next
	}
	return "", fmt.Errorf("too many redirects")
}

// absURL resolves ref against base if ref is not already absolute.
func absURL(base *url.URL, ref string) string {
	u, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	if u.IsAbs() {
		return ref
	}
	return base.ResolveReference(u).String()
}

// extractSitekey pulls a Turnstile sitekey out of an HTML page. Cursor's
// server-rendered page includes a <div class="cf-turnstile" data-sitekey="0x4A…">
// widget. As a fallback we also search for the sitekey embedded in the
// Cloudflare iframe URL (challenges.cloudflare.com/turnstile/v0/…?k=<sitekey>).
var (
	sitekeyDataRE   = regexp.MustCompile(`data-sitekey\s*=\s*["']([0-9a-zA-Z_-]+)["']`)
	sitekeyIframeRE = regexp.MustCompile(`challenges\.cloudflare\.com/turnstile[^"']*[?&]k=([0-9a-zA-Z_-]+)`)
	sitekeyJSRE     = regexp.MustCompile(`["']sitekey["']\s*:\s*["']([0-9a-zA-Z_-]+)["']`)
)

func extractSitekey(html string) string {
	if m := sitekeyDataRE.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	if m := sitekeyIframeRE.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	if m := sitekeyJSRE.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// extractNextAction scans a Next.js RSC bundle for the server-action
// id we need to send in the Next-Action header. Actions are embedded
// as either `$$ACTION_ID_<hex40>` in the JS bundle or as a bare
// `"nextAction":"<hex40>"` string in the initial payload.
var (
	nextActionInlineRE = regexp.MustCompile(`\$\$ACTION_ID_([0-9a-f]{40})`)
	nextActionKeyRE    = regexp.MustCompile(`(?i)["']next[-_]?action["']\s*:\s*["']([0-9a-f]{40})["']`)
)

func extractNextAction(html string) string {
	if m := nextActionInlineRE.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	if m := nextActionKeyRE.FindStringSubmatch(html); m != nil {
		return m[1]
	}
	return ""
}

// extractChallengeID pulls the authentication_challenge_id off a URL,
// Location header, or a raw HTML/JS blob referencing it.
var challengeIDRE = regexp.MustCompile(`authentication_challenge_id=([A-Za-z0-9_-]+)`)

func extractChallengeID(s string) string {
	if s == "" {
		return ""
	}
	if m := challengeIDRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	return ""
}

// extractCallbackCode pulls the OAuth code parameter off a
// cursor.com/api/auth/callback URL.
var callbackCodeRE = regexp.MustCompile(`cursor\.com/api/auth/callback[^"'\s]*[?&]code=([A-Za-z0-9_.\-]+)`)

func extractCallbackCode(s string) string {
	if s == "" {
		return ""
	}
	if m := callbackCodeRE.FindStringSubmatch(s); m != nil {
		return m[1]
	}
	// Fallback: parse as a URL.
	if u, err := url.Parse(s); err == nil {
		if strings.HasSuffix(u.Host, "cursor.com") && strings.HasPrefix(u.Path, "/api/auth/callback") {
			if code := u.Query().Get("code"); code != "" {
				return code
			}
		}
	}
	return ""
}

// setBrowserHeaders stamps the request with the same headers a real
// Chrome would send. Cursor's WAF drops requests that look like curl.
func setBrowserHeaders(req *http.Request) {
	req.Header.Set("User-Agent", otpUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("Sec-Fetch-User", "?1")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
}

// drainBody reads and closes resp.Body, returning the body bytes.
func drainBody(resp *http.Response) ([]byte, error) {
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil {
			_ = cerr
		}
	}()
	return io.ReadAll(resp.Body)
}

// newUUID returns a short UUIDv4 without importing google/uuid twice —
// the package is already in scope in login.go so we defer to it.
func newUUID() string { return newUUIDImpl() }

// Indirection so tests could stub if they ever needed to. Points at
// google/uuid via the package-level variable below.
var newUUIDImpl = func() string { return uuidNewString() }

// Wrapper to keep the imports of google/uuid confined to login.go /
// this helper. Because otp_flow.go is in the same package, we can call
// uuid.NewString by name via a helper defined near the imports.
// (See otp_flow_helpers.go, which lives in the same package.)

// parseAuthMe extracts the four fields we care about from a cursor.com
// /api/auth/me response. The endpoint is JSON-shaped in production but
// has changed keys over time; we accept a few variants.
func parseAuthMe(body []byte) (access, refresh, uid, authID, authKind string, err error) {
	if len(body) == 0 {
		return "", "", "", "", "", fmt.Errorf("empty body")
	}
	m, err := decodeAuthMeJSON(body)
	if err != nil {
		return "", "", "", "", "", err
	}
	access = firstString(m, "accessToken", "access_token")
	if access == "" {
		if s, ok := m["session"].(map[string]any); ok {
			access = firstString(s, "accessToken", "access_token")
			refresh = firstString(s, "refreshToken", "refresh_token")
		}
	}
	if refresh == "" {
		refresh = firstString(m, "refreshToken", "refresh_token")
	}
	uid = firstString(m, "sub", "id", "userId", "user_id")
	if uid == "" {
		if s, ok := m["user"].(map[string]any); ok {
			uid = firstString(s, "id", "sub", "userId", "user_id")
		}
	}
	authID = firstString(m, "authId", "auth_id")
	authKind = firstString(m, "authType", "auth_type", "authKind", "auth_kind")
	if authKind == "" {
		authKind = "email-otp"
	}
	return
}

// firstString returns the first non-empty string value in m for the
// given keys, or "".
func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return s
			}
		}
	}
	return ""
}

// tcpDialForTest is used by unit tests to inject a plain-TCP dial
// helper. Production always dials real TLS.
var tcpDialForTest func(host string, port int) (net.Conn, error)
