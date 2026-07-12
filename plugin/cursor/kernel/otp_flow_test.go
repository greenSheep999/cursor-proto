package kernel

// Unit tests for the OTP login flow.
//
// The real endpoints (authenticator.cursor.sh, api.yescaptcha.com,
// cursor.com/api/auth/me) cost credit and leave audit trails when
// hit. We drive the code against httptest servers instead. See
// docs/plugin-cursor-otp-manual-test.md for end-to-end verification.

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mockYesCaptcha spins up a tiny YesCaptcha stand-in that returns a
// predictable token after N /getTaskResult polls.
func mockYesCaptcha(t *testing.T, token string, pollsToReady int) *httptest.Server {
	t.Helper()
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/createTask":
			_, _ = io.WriteString(w, `{"errorId":0,"taskId":42}`)
		case "/getTaskResult":
			n := atomic.AddInt32(&polls, 1)
			if int(n) < pollsToReady {
				_, _ = io.WriteString(w, `{"errorId":0,"status":"processing"}`)
				return
			}
			_, _ = fmt.Fprintf(w, `{"errorId":0,"status":"ready","solution":{"token":%q}}`, token)
		default:
			http.Error(w, "unknown path", http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// mockAuthenticator serves the two endpoints the plugin talks to on
// authenticator.cursor.sh: GET / (serves an HTML page with an
// extractable sitekey + next-action) and POST / (returns a 303 to
// /magic-code?authentication_challenge_id=...).
//
// It also serves /magic-code GET/POST for the poll leg. behavior
// controls how the mock reacts.
type authenticatorBehavior struct {
	StartHTML       string // HTML returned on GET /
	StartPostStatus int    // 200 (fail path) or 303 (happy path)
	ChallengeID     string // included in the Location header on the 303
	MagicHTML       string // HTML returned on GET /magic-code
	FailPostBody    string // body of POST / when StartPostStatus is 400+
	OTPPostStatus   int    // 303 on happy path, 400 on wrong OTP
	CallbackCode    string // final ?code= we redirect to (cursor.com/api/auth/callback?code=...)
	AuthMeBody      string // response body for /api/auth/me
}

func mockAuthenticator(t *testing.T, b *authenticatorBehavior) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, b.StartHTML)
				return
			}
			if r.Method == http.MethodPost {
				status := b.StartPostStatus
				if status == 0 {
					status = http.StatusSeeOther
				}
				if status >= 300 && status < 400 {
					w.Header().Set("Location", "/magic-code?authentication_challenge_id="+b.ChallengeID)
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, b.FailPostBody)
				return
			}
		case "/magic-code":
			if r.Method == http.MethodGet {
				w.Header().Set("Content-Type", "text/html")
				_, _ = io.WriteString(w, b.MagicHTML)
				return
			}
			if r.Method == http.MethodPost {
				status := b.OTPPostStatus
				if status == 0 {
					status = http.StatusSeeOther
				}
				if status >= 300 && status < 400 {
					// The real /magic-code answers with a 303 to
					// something like /? which then 303s to cursor.com/api/auth/callback?code=….
					// Collapse to a direct redirect for the test.
					w.Header().Set("Location", "https://cursor.com/api/auth/callback?code="+b.CallbackCode)
					w.WriteHeader(status)
					return
				}
				w.WriteHeader(status)
				_, _ = io.WriteString(w, "otp rejected")
				return
			}
		}
		http.Error(w, "unknown endpoint: "+r.URL.Path, http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// captureRoundTripper redirects requests for authenticator.cursor.sh
// and cursor.com onto the mock httptest servers.
type captureRoundTripper struct {
	authTarget   string // e.g. srv.URL for the authenticator mock
	cursorTarget string // for cursor.com/api/auth/me
	lastRequests []*http.Request
}

func (c *captureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	c.lastRequests = append(c.lastRequests, req)
	newHost := ""
	switch req.URL.Host {
	case cursorAuthHost:
		newHost = strings.TrimPrefix(c.authTarget, "http://")
	case "cursor.com":
		newHost = strings.TrimPrefix(c.cursorTarget, "http://")
	}
	if newHost != "" {
		req = req.Clone(req.Context())
		req.URL.Scheme = "http"
		req.URL.Host = newHost
		req.Host = ""
	}
	return http.DefaultTransport.RoundTrip(req)
}

// TestOTPFlow_Happy end-to-end: Start GETs authenticator, extracts
// sitekey and next-action, solves Turnstile, POSTs and receives a
// challenge id. Poll uses a literal OTP, hits /magic-code, follows
// the callback, and lifts tokens from /api/auth/me. Result: success.
func TestOTPFlow_Happy(t *testing.T) {
	yc := mockYesCaptcha(t, "turnstile-token-abc", 2)
	auth := mockAuthenticator(t, &authenticatorBehavior{
		StartHTML: `
			<html><body>
			<div class="cf-turnstile" data-sitekey="0x4AAAAAAA_SITE_KEY"></div>
			<script>$$ACTION_ID_fef846a39073c935bea71b63308b177b113269b7</script>
			</body></html>`,
		StartPostStatus: http.StatusSeeOther,
		ChallengeID:     "chal-xyz-9",
		MagicHTML:       `<html><body><input name="1_code"/></body></html>`,
		OTPPostStatus:   http.StatusSeeOther,
		CallbackCode:    "cb-code-777",
		AuthMeBody:      `{"accessToken":"AT.header.payload.sig","refreshToken":"RT","sub":"user_ABC","authId":"workos|user_ABC","authType":"email-otp"}`,
	})

	// A second mock stands in for cursor.com — serves /api/auth/callback
	// and /api/auth/me.
	cursor := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth/callback":
			w.WriteHeader(http.StatusOK)
		case "/api/auth/me":
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"accessToken":"AT.header.payload.sig","refreshToken":"RT","sub":"user_ABC","authId":"workos|user_ABC","authType":"email-otp"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(cursor.Close)

	rt := &captureRoundTripper{authTarget: auth.URL, cursorTarget: cursor.URL}

	opts := otpStartOptions{
		AuthBase:       auth.URL,
		AuthMeEndpoint: cursor.URL + "/api/auth/me",
		YesCaptchaBase: yc.URL,
		Transport:      rt,
	}
	ctx := t.Context()
	st, err := startCursorOTP(ctx, "someone@example.com", "yc-test-key", nil, "123456", opts)
	if err != nil {
		t.Fatalf("startCursorOTP: %v", err)
	}
	if st.ChallengeID != "chal-xyz-9" {
		t.Errorf("ChallengeID = %q, want chal-xyz-9", st.ChallengeID)
	}
	if st.LiteralOTP != "123456" {
		t.Errorf("LiteralOTP = %q, want 123456", st.LiteralOTP)
	}

	// Poll uses the literal OTP. We swap YesCaptcha for a fast-ready
	// version to keep the test snappy.
	yc2 := mockYesCaptcha(t, "second-turnstile", 1)
	st.YesCaptchaBase = yc2.URL

	res := pollCursorOTP(ctx, st)
	if res.Outcome != otpOutcomeSuccess {
		t.Fatalf("poll outcome = %v, msg = %q", res.Outcome, res.Message)
	}
	if res.AccessToken != "AT.header.payload.sig" {
		t.Errorf("AccessToken = %q", res.AccessToken)
	}
	if res.RefreshToken != "RT" {
		t.Errorf("RefreshToken = %q", res.RefreshToken)
	}
	if res.UserID != "user_ABC" {
		t.Errorf("UserID = %q", res.UserID)
	}
	if res.AuthKind != "email-otp" {
		t.Errorf("AuthKind = %q", res.AuthKind)
	}
}

// TestOTPFlow_TurnstileFailure short-circuits Start when the solver
// throws a hard error.
func TestOTPFlow_TurnstileFailure(t *testing.T) {
	// A YesCaptcha stub that always reports errorId != 0.
	yc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"errorId":1,"errorCode":"ERROR_KEY_DENIED","errorDescription":"invalid key"}`)
	}))
	t.Cleanup(yc.Close)

	auth := mockAuthenticator(t, &authenticatorBehavior{
		StartHTML: `<div class="cf-turnstile" data-sitekey="0x4AAAAAAA"></div>`,
	})

	opts := otpStartOptions{
		AuthBase:       auth.URL,
		YesCaptchaBase: yc.URL,
	}
	_, err := startCursorOTP(t.Context(), "someone@example.com", "bad-key", nil, "123456", opts)
	if err == nil {
		t.Fatal("expected error from Turnstile solver, got nil")
	}
	if !strings.Contains(err.Error(), "turnstile solve") {
		t.Errorf("error should mention turnstile solve, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "ERROR_KEY_DENIED") {
		t.Errorf("error should surface underlying code, got %q", err.Error())
	}
}

// TestOTPFlow_FormRejected covers the "authenticator rejected form
// submit" path — likely a fingerprint mismatch or invalid Turnstile
// token. Start should fail-fast with a descriptive error.
func TestOTPFlow_FormRejected(t *testing.T) {
	yc := mockYesCaptcha(t, "turnstile-token", 1)
	auth := mockAuthenticator(t, &authenticatorBehavior{
		StartHTML:       `<div class="cf-turnstile" data-sitekey="0x4AAAAAAA"></div>`,
		StartPostStatus: http.StatusForbidden,
		FailPostBody:    `{"error":"invalid_signals","message":"bot detected"}`,
	})

	opts := otpStartOptions{
		AuthBase:       auth.URL,
		YesCaptchaBase: yc.URL,
	}
	_, err := startCursorOTP(t.Context(), "someone@example.com", "yc-key", nil, "123456", opts)
	if err == nil {
		t.Fatal("expected error on 403 POST, got nil")
	}
	if !strings.Contains(err.Error(), "fingerprint blocked") {
		t.Errorf("error should call out fingerprint blocking, got %q", err.Error())
	}
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("error should include HTTP status, got %q", err.Error())
	}
}

// TestOTPFlow_WrongOTP covers the poll path where /magic-code POST
// returns a non-303 status. The plugin should surface an error, not
// a pending or a false success.
func TestOTPFlow_WrongOTP(t *testing.T) {
	yc := mockYesCaptcha(t, "tok", 1)
	auth := mockAuthenticator(t, &authenticatorBehavior{
		StartHTML:       `<div class="cf-turnstile" data-sitekey="0x4AAAAAAA"></div><script>$$ACTION_ID_fef846a39073c935bea71b63308b177b113269b7</script>`,
		StartPostStatus: http.StatusSeeOther,
		ChallengeID:     "chal-1",
		MagicHTML:       `<html></html>`, // no sitekey → skip second Turnstile solve
		OTPPostStatus:   http.StatusBadRequest,
	})

	opts := otpStartOptions{AuthBase: auth.URL, YesCaptchaBase: yc.URL}
	st, err := startCursorOTP(t.Context(), "someone@example.com", "yc-key", nil, "999999", opts)
	if err != nil {
		t.Fatalf("startCursorOTP: %v", err)
	}
	res := pollCursorOTP(t.Context(), st)
	if res.Outcome != otpOutcomeError {
		t.Fatalf("outcome = %v, want error (message=%q)", res.Outcome, res.Message)
	}
	if !strings.Contains(res.Message, "HTTP 400") {
		t.Errorf("message should mention 400, got %q", res.Message)
	}
}

// TestBuildSignals verifies the signals blob round-trips through
// base64 and JSON, and carries the two timestamps set per-request.
func TestBuildSignals(t *testing.T) {
	before := time.Now().UnixMilli()
	raw := buildSignals(time.Now())
	after := time.Now().UnixMilli()

	decoded, err := base64.StdEncoding.DecodeString(string(raw))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(decoded, &out); err != nil {
		t.Fatalf("json decode: %v (decoded=%s)", err, string(decoded))
	}
	// The timestamps are marshalled as float64 by encoding/json.
	submittedMs, ok := out["submittedAtMs"].(float64)
	if !ok {
		t.Fatalf("submittedAtMs missing or wrong type: %v", out["submittedAtMs"])
	}
	if int64(submittedMs) < before || int64(submittedMs) > after {
		t.Errorf("submittedAtMs = %v, expected between %d and %d", submittedMs, before, after)
	}
	createdMs, _ := out["createdAtMs"].(float64)
	if createdMs >= submittedMs {
		t.Errorf("createdAtMs (%v) should be < submittedAtMs (%v)", createdMs, submittedMs)
	}
	// Sanity: the fingerprint blob should have the fixed UA field.
	if ua, _ := out["userAgent"].(string); !strings.Contains(ua, "Chrome/") {
		t.Errorf("userAgent missing Chrome/: %v", ua)
	}
}

// TestBuildMultipartFieldNames checks that the multipart body carries
// all the field names Cursor expects, and no unexpected ones.
func TestBuildMultipartFieldNames(t *testing.T) {
	body, ct, err := buildAuthMultipart(authForm{
		Token:       "TOK",
		Email:       "someone@example.com",
		Intent:      "magic-code",
		State:       "state123",
		SessionID:   "sess123",
		RedirectURI: cursorRedirectURI,
		Signals:     []byte("SIGNALS"),
	})
	if err != nil {
		t.Fatalf("buildAuthMultipart: %v", err)
	}
	_, params, err := mime.ParseMediaType(ct)
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	mr := multipart.NewReader(strings.NewReader(body), params["boundary"])
	got := map[string]string{}
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		buf, _ := io.ReadAll(p)
		got[p.FormName()] = string(buf)
	}
	want := []string{
		"1_bot_detection_token",
		"1_signals",
		"1_email",
		"1_password",
		"1_intent",
		"1_redirect_uri",
		"1_authorization_session_id",
		"1_state",
		"0",
	}
	for _, k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("missing field %q (got: %v)", k, got)
		}
	}
	if got["1_bot_detection_token"] != "TOK" {
		t.Errorf("token field = %q", got["1_bot_detection_token"])
	}
	if got["1_intent"] != "magic-code" {
		t.Errorf("intent = %q", got["1_intent"])
	}
	if got["1_signals"] != "SIGNALS" {
		t.Errorf("signals = %q", got["1_signals"])
	}
}

// TestExtractSitekey / TestExtractNextAction / TestExtractChallengeID
// exercise the HTML/URL regexes with the shapes real Cursor pages
// produce.
func TestExtractSitekey(t *testing.T) {
	cases := map[string]string{
		`<div data-sitekey="0x4AAAAAAA">`:                                                 "0x4AAAAAAA",
		`<iframe src="https://challenges.cloudflare.com/turnstile/v0/api.js?k=SITEKEYX">`: "SITEKEYX",
		`{"sitekey":"0x4YYYY"}`:                                                           "0x4YYYY",
		`no widget here`:                                                                  "",
	}
	for html, want := range cases {
		got := extractSitekey(html)
		if got != want {
			t.Errorf("extractSitekey(%q) = %q, want %q", html, got, want)
		}
	}
}

func TestExtractNextAction(t *testing.T) {
	if got := extractNextAction(`... $$ACTION_ID_fef846a39073c935bea71b63308b177b113269b7 ...`); got != "fef846a39073c935bea71b63308b177b113269b7" {
		t.Errorf("inline pattern miss: %q", got)
	}
	if got := extractNextAction(`{"nextAction":"aabbccddeeff00112233445566778899aabbccdd"}`); got != "aabbccddeeff00112233445566778899aabbccdd" {
		t.Errorf("key pattern miss: %q", got)
	}
	if got := extractNextAction(`no hash here`); got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestExtractChallengeID(t *testing.T) {
	if got := extractChallengeID("/magic-code?authentication_challenge_id=abc-123"); got != "abc-123" {
		t.Errorf("miss: %q", got)
	}
	if got := extractChallengeID(""); got != "" {
		t.Errorf("empty case failed: %q", got)
	}
}

func TestExtractCallbackCode(t *testing.T) {
	if got := extractCallbackCode("https://cursor.com/api/auth/callback?code=XYZ&state=1"); got != "XYZ" {
		t.Errorf("miss: %q", got)
	}
	if got := extractCallbackCode("https://cursor.com/dashboard"); got != "" {
		t.Errorf("expected empty for dashboard, got %q", got)
	}
}

// TestParseAuthMe covers a couple of common cursor.com response shapes.
func TestParseAuthMe(t *testing.T) {
	flat := `{"accessToken":"AT","refreshToken":"RT","sub":"user_A","authId":"workos|user_A","authType":"email-otp"}`
	a, r, uid, aid, kind, err := parseAuthMe([]byte(flat))
	if err != nil {
		t.Fatalf("flat: %v", err)
	}
	if a != "AT" || r != "RT" || uid != "user_A" || aid != "workos|user_A" || kind != "email-otp" {
		t.Errorf("flat: got (%q, %q, %q, %q, %q)", a, r, uid, aid, kind)
	}
	nested := `{"session":{"accessToken":"AT2","refreshToken":"RT2"},"user":{"id":"user_B"}}`
	a, r, uid, aid, kind, err = parseAuthMe([]byte(nested))
	if err != nil {
		t.Fatalf("nested: %v", err)
	}
	if a != "AT2" || r != "RT2" || uid != "user_B" {
		t.Errorf("nested: got (%q, %q, %q, %q, %q)", a, r, uid, aid, kind)
	}
}

// TestStartLoginOTP_ManualFallback: when the operator does not supply
// an email, StartLogin(otp) falls back to the legacy login-hub hint
// path. Guards against accidentally breaking the old workflow.
func TestStartLoginOTP_ManualFallback(t *testing.T) {
	clearLoginSessions(t)
	raw, rc := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode": "otp",
	}))
	if rc != 0 {
		t.Fatalf("rc = %d, envelope=%s", rc, string(raw))
	}
	resp := unmarshalStartResult(t, raw)
	if v, ok := resp.Metadata["manual_workflow"].(bool); !ok || !v {
		t.Errorf("expected manual_workflow=true, got %v", resp.Metadata["manual_workflow"])
	}
}

// TestStartLoginOTP_MissingKey: with email + no YesCaptcha key, Start
// should return a clean error instead of hanging.
func TestStartLoginOTP_MissingKey(t *testing.T) {
	clearLoginSessions(t)
	// Ensure env is not accidentally set from the test host.
	t.Setenv("YESCAPTCHA_API_KEY", "")
	raw, rc := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode":  "otp",
		"email": "someone@example.com",
	}))
	if rc == 0 {
		t.Fatalf("expected non-zero rc, envelope=%s", string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK {
		t.Fatal("expected OK=false")
	}
	if env.Error == nil || env.Error.Code != "otp_config" {
		t.Errorf("expected otp_config error, got %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "YESCAPTCHA_API_KEY") {
		t.Errorf("message should mention YESCAPTCHA_API_KEY: %q", env.Error.Message)
	}
}

// TestStartLoginOTP_MissingOTPAndIMAP: email + key + no otp + no IMAP → error.
func TestStartLoginOTP_MissingOTPAndIMAP(t *testing.T) {
	clearLoginSessions(t)
	t.Setenv("YESCAPTCHA_API_KEY", "test-key")
	raw, rc := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode":  "otp",
		"email": "someone@example.com",
	}))
	if rc == 0 {
		t.Fatalf("expected non-zero rc, envelope=%s", string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Error == nil || env.Error.Code != "otp_config" {
		t.Errorf("expected otp_config error, got %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "IMAP") && !strings.Contains(env.Error.Message, "metadata.otp") {
		t.Errorf("message should suggest otp or imap: %q", env.Error.Message)
	}
}
