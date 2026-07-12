package kernel

// Cursor login ABI handlers.
//
// CPA's management panel drives account onboarding for every
// provider through the auth.login.start / auth.login.poll RPCs.
// This file implements those handlers for the Cursor plugin, so
// operators can add accounts through the CPA UI just like they
// already do for Anthropic, Codex, Gemini CLI, Kimi, xAI, and
// Antigravity.
//
// The three modes are selected by the caller through
// Metadata.mode:
//
//   - "oauth": Cursor's PKCE + api2.cursor.sh/auth/poll device
//     flow. The plugin returns the LoginURL for the operator to
//     open in a browser; PollLogin returns pending until the
//     browser side finishes, at which point we hand back the
//     resulting AuthData.
//
//   - "ide": read $HOME/…/state.vscdb directly and return an
//     AuthData synchronously. Handy when CPA runs on the same
//     host as a signed-in Cursor IDE. Fails cleanly when the
//     database does not exist (which is the common case when
//     CPA runs in a Linux container).
//
//   - "otp": Cursor's email-OTP + Turnstile flow depends on a
//     real Chromium (Camoufox) instance and an IMAP client.
//     Running that from inside a c-shared plugin is not viable
//     (subprocess lifetime, cgo stack issues, IPC). Instead the
//     plugin returns a pending status with a message asking the
//     operator to run the login-hub Python service and paste
//     the resulting CPA JSON back through /v0/management/oauth-callback.
//     The pluginapi.AuthLoginStatus enum only supports
//     pending/success/error, so we lean on pending + a
//     descriptive Message.
//
// State handling: an in-flight login is keyed by an opaque State
// string in a package-level sync.Map. OAuth uses the PKCE UUID as
// state; the other modes generate a fresh UUID. Entries expire
// after loginSessionTTL and prune lazily on every Start.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// loginSessionTTL bounds how long a Start/Poll pair may span before
// the state is forgotten. Matches Cursor's own poll window (~5 min)
// plus a healthy operator buffer.
const loginSessionTTL = 15 * time.Minute

// loginModeOAuth / loginModeIDE / loginModeOTP are the accepted
// Metadata.mode values. Anything else returns a structured error.
const (
	loginModeOAuth = "oauth"
	loginModeIDE   = "ide"
	loginModeOTP   = "otp"
)

// otpLoginHubURL is the doc URL surfaced to operators when they ask
// for OTP mode. The plugin does not run Chromium itself; login-hub
// (the muxhub Python service) does that and posts the resulting
// CPA JSON back through the standard callback.
const otpLoginHubURL = "https://github.com/router-for-me/CLIProxyAPI/blob/main/docs/cursor-otp-login-hub.md"

// authLoginStatus mirrors pluginapi.AuthLoginStatus. The three
// values are the only ones CPA understands (see
// sdk/pluginapi/types.go). "manual_required" is not a valid value;
// OTP mode uses pending + Message.
type authLoginStatus string

const (
	authLoginStatusPending authLoginStatus = "pending"
	authLoginStatusSuccess authLoginStatus = "success"
	authLoginStatusError   authLoginStatus = "error"
)

// authLoginStartRequest mirrors pluginapi.AuthLoginStartRequest for
// the ABI JSON wire format. HTTPClient is transported as a callback
// handle by CPA and is not JSON-visible; we ignore it here.
type authLoginStartRequest struct {
	Provider string                 `json:"Provider"`
	BaseURL  string                 `json:"BaseURL"`
	Metadata map[string]any         `json:"Metadata,omitempty"`
	Host     map[string]interface{} `json:"Host,omitempty"`
}

type authLoginStartResponse struct {
	Provider  string         `json:"Provider"`
	URL       string         `json:"URL"`
	State     string         `json:"State"`
	ExpiresAt time.Time      `json:"ExpiresAt"`
	Metadata  map[string]any `json:"Metadata,omitempty"`
}

type authLoginPollRequest struct {
	Provider string                 `json:"Provider"`
	State    string                 `json:"State"`
	Metadata map[string]any         `json:"Metadata,omitempty"`
	Host     map[string]interface{} `json:"Host,omitempty"`
}

type authLoginPollResponse struct {
	Status  authLoginStatus `json:"Status"`
	Message string          `json:"Message,omitempty"`
	Auth    authData        `json:"Auth"`
}

// loginEntry is the per-in-flight state we keep between Start and Poll.
type loginEntry struct {
	Mode        string
	OAuthSess   *auth.LoginSession
	Preresolved *authData // populated for ide mode; poll returns this immediately
	OTPState    *otpState // populated for otp mode; carries cookies+challenge
	Message     string    // sticky Message returned by Poll (e.g. OTP hint)
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

var loginSessions sync.Map // state (string) -> *loginEntry

// storeLoginEntry saves an entry and drops any expired peers so the
// map does not grow unbounded. Called on every Start.
func storeLoginEntry(state string, entry *loginEntry) {
	pruneLoginSessions(time.Now())
	loginSessions.Store(state, entry)
}

// loadLoginEntry fetches an entry, expiring it lazily if past its
// TTL. Returns nil when unknown or expired.
func loadLoginEntry(state string) *loginEntry {
	v, ok := loginSessions.Load(state)
	if !ok {
		return nil
	}
	entry := v.(*loginEntry)
	if !entry.ExpiresAt.IsZero() && time.Now().After(entry.ExpiresAt) {
		loginSessions.Delete(state)
		return nil
	}
	return entry
}

// pruneLoginSessions drops every entry whose ExpiresAt is in the past.
func pruneLoginSessions(now time.Time) {
	loginSessions.Range(func(k, v any) bool {
		entry, ok := v.(*loginEntry)
		if !ok {
			loginSessions.Delete(k)
			return true
		}
		if !entry.ExpiresAt.IsZero() && now.After(entry.ExpiresAt) {
			loginSessions.Delete(k)
		}
		return true
	})
}

// handleAuthLoginStart implements the auth.login.start ABI method.
func handleAuthLoginStart(payload []byte) ([]byte, int) {
	var req authLoginStartRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse login start request: %v", err), false), 1
	}
	mode := strings.ToLower(strings.TrimSpace(metadataString(req.Metadata, "mode")))
	if mode == "" {
		mode = loginModeOAuth
	}

	switch mode {
	case loginModeOAuth:
		return startLoginOAuth(&req)
	case loginModeIDE:
		return startLoginIDE(&req)
	case loginModeOTP:
		return startLoginOTP(&req)
	default:
		return errorEnvelope("bad_request", fmt.Sprintf(
			"unknown cursor login mode %q (expected one of: oauth, ide, otp)", mode), false), 1
	}
}

// handleAuthLoginPoll implements the auth.login.poll ABI method.
func handleAuthLoginPoll(payload []byte) ([]byte, int) {
	var req authLoginPollRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse login poll request: %v", err), false), 1
	}
	state := strings.TrimSpace(req.State)
	if state == "" {
		return errorEnvelope("bad_request", "State is required", false), 1
	}
	entry := loadLoginEntry(state)
	if entry == nil {
		return errorEnvelope("unknown_state", "no in-flight cursor login for that state (expired or never started)", false), 1
	}

	switch entry.Mode {
	case loginModeOAuth:
		return pollLoginOAuth(state, entry)
	case loginModeIDE:
		return pollLoginPreresolved(state, entry)
	case loginModeOTP:
		return pollLoginOTP(state, entry)
	default:
		return errorEnvelope("bad_state", fmt.Sprintf("unknown mode %q on stored entry", entry.Mode), false), 1
	}
}

// startLoginOAuth kicks off Cursor's PKCE-based device login flow.
// The operator opens the returned URL, authenticates, and Cursor's
// api2 endpoint then serves the tokens to a follow-up poll.
func startLoginOAuth(req *authLoginStartRequest) ([]byte, int) {
	sess, err := auth.StartLogin()
	if err != nil {
		return errorEnvelope("login_start", fmt.Sprintf("start cursor login session: %v", err), true), 1
	}
	// Allow tests / batch harness to redirect the poll endpoint.
	if base := strings.TrimSpace(metadataString(req.Metadata, "poll_url_base")); base != "" {
		sess.PollURLBase = base
	}

	now := time.Now()
	entry := &loginEntry{
		Mode:      loginModeOAuth,
		OAuthSess: sess,
		CreatedAt: now,
		ExpiresAt: now.Add(loginSessionTTL),
	}
	storeLoginEntry(sess.UUID, entry)

	resp := authLoginStartResponse{
		Provider:  pluginName,
		URL:       sess.LoginURL,
		State:     sess.UUID,
		ExpiresAt: entry.ExpiresAt,
		Metadata: map[string]any{
			"mode":     loginModeOAuth,
			"verifier": sess.PKCE.Verifier,
			"uuid":     sess.UUID,
		},
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// pollLoginOAuth performs one non-blocking poll against Cursor's
// api2 endpoint. Returns pending on the empty response, success +
// AuthData on the completed one.
func pollLoginOAuth(state string, entry *loginEntry) ([]byte, int) {
	sess := entry.OAuthSess
	if sess == nil {
		return errorEnvelope("bad_state", "oauth session missing", false), 1
	}
	// Bounded context: the api2 poll call itself has a short client
	// timeout (see auth.StartLogin), this is a belt-and-braces cap
	// that keeps a stalled RPC from hanging forever.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	pr, err := sess.Poll(ctx)
	if err != nil {
		return finalizeErrorPoll(fmt.Sprintf("poll cursor: %v", err))
	}
	if pr == nil {
		return finalizePendingPoll("waiting for cursor login to complete in browser")
	}

	authFile := buildAuthFileFromPoll(pr)
	authRecord, err := buildAuthData(authFile, state)
	if err != nil {
		return finalizeErrorPoll(fmt.Sprintf("build auth data: %v", err))
	}
	loginSessions.Delete(state)
	return finalizeSuccessPoll(authRecord)
}

// pollLoginPreresolved returns the pre-computed AuthData populated
// by the corresponding Start handler. Used for ide mode, which
// resolves synchronously.
func pollLoginPreresolved(state string, entry *loginEntry) ([]byte, int) {
	if entry.Preresolved == nil {
		return finalizeErrorPoll("pre-resolved auth data is missing")
	}
	a := *entry.Preresolved
	loginSessions.Delete(state)
	return finalizeSuccessPoll(&a)
}

// pollLoginOTP drives the second half of the Cursor magic-code flow:
// wait for the OTP email (or use a literal), submit the code, follow
// the callback chain, and materialise a full AuthData. If the OTP has
// not yet arrived we return a pending status so CPA calls us again.
func pollLoginOTP(state string, entry *loginEntry) ([]byte, int) {
	// Legacy manual-workflow session? Return the sticky pending
	// message so old CPA panels keep working during a rollout.
	if entry.OTPState == nil {
		msg := entry.Message
		if msg == "" {
			msg = otpPendingMessage()
		}
		return finalizePendingPoll(msg)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	res := pollCursorOTP(ctx, entry.OTPState)
	switch res.Outcome {
	case otpOutcomePending:
		msg := res.Message
		if msg == "" {
			msg = "waiting for cursor magic-code email"
		}
		return finalizePendingPoll(msg)
	case otpOutcomeError:
		return finalizeErrorPoll(res.Message)
	}

	// Success — turn the tokens into an AuthData record.
	authFile := buildAuthFileFromOTP(res)
	authRecord, err := buildAuthData(authFile, state)
	if err != nil {
		return finalizeErrorPoll(fmt.Sprintf("build auth data: %v", err))
	}
	loginSessions.Delete(state)
	return finalizeSuccessPoll(authRecord)
}

// buildAuthFileFromOTP packages the tokens from cursor.com/api/auth/me
// into the CPA on-disk shape. Mirrors buildAuthFileFromPoll but for
// OTP mode: we already have the email upfront (unlike OAuth, where we
// have to lift it out of the JWT sub claim).
func buildAuthFileFromOTP(res *otpPollResult) *cpaformat.AuthFile {
	now := time.Now()
	email := res.Email
	if email == "" {
		email = extractEmailFromJWT(res.AccessToken)
	}
	userID := res.UserID
	if userID == "" {
		userID = extractUserIDFromAuthID(res.AuthID)
	}
	kind := res.AuthKind
	if kind == "" {
		kind = "email-otp"
	}
	return &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:         cpaformat.ProviderType,
			AccessToken:  res.AccessToken,
			RefreshToken: res.RefreshToken,
			Email:        email,
			UserID:       userID,
			AuthID:       res.AuthID,
			AuthKind:     kind,
			IssuedAt:     cpaformat.FormatTime(now),
			LastRefresh:  cpaformat.FormatTime(now),
			Expired:      cpaformat.FormatTime(auth.ExpiresAtFromJWT(res.AccessToken)),
			Refreshable:  res.RefreshToken != "",
		},
	}
}

// startLoginIDE reads the local Cursor IDE state.vscdb and packages
// it as pre-resolved AuthData. When the file does not exist we
// return a structured error rather than a stack trace.
func startLoginIDE(req *authLoginStartRequest) ([]byte, int) {
	dbPath := strings.TrimSpace(metadataString(req.Metadata, "db_path"))
	if dbPath == "" {
		p, err := defaultIDEStatePath()
		if err != nil {
			return errorEnvelope("ide_unavailable", err.Error(), false), 1
		}
		dbPath = p
	}
	if _, err := os.Stat(dbPath); err != nil {
		return errorEnvelope("ide_unavailable", fmt.Sprintf(
			"state.vscdb not found at %s — is Cursor IDE installed and signed in on this host? (%v)",
			dbPath, err), false), 1
	}
	authFile, err := loadIDEAccount(dbPath)
	if err != nil {
		return errorEnvelope("ide_read_failed", err.Error(), false), 1
	}
	state := uuid.NewString()
	authRecord, err := buildAuthData(authFile, state)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	now := time.Now()
	entry := &loginEntry{
		Mode:        loginModeIDE,
		Preresolved: authRecord,
		CreatedAt:   now,
		ExpiresAt:   now.Add(loginSessionTTL),
	}
	storeLoginEntry(state, entry)

	resp := authLoginStartResponse{
		Provider:  pluginName,
		URL:       "",
		State:     state,
		ExpiresAt: entry.ExpiresAt,
		Metadata: map[string]any{
			"mode":    loginModeIDE,
			"db_path": dbPath,
			"email":   authFile.Email,
		},
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// startLoginOTP performs the first half of the Cursor magic-code
// flow: hit /? to lift the Turnstile sitekey + Next.js action id,
// solve Turnstile via YesCaptcha, POST intent=magic-code and stash
// the returned challenge id in the sync.Map. Poll picks up from
// there.
//
// The legacy "manual login-hub" mode is preserved for backwards
// compatibility: pass metadata.manual=true (or leave metadata.email
// blank) and Start returns the old pending message.
func startLoginOTP(req *authLoginStartRequest) ([]byte, int) {
	email := strings.TrimSpace(metadataString(req.Metadata, "email"))
	manual := metadataBool(req.Metadata, "manual")

	// Legacy path: no email or explicit manual flag → return the
	// login-hub instruction and stay pending forever.
	if email == "" || manual {
		return startLoginOTPManual(req)
	}

	yescaptchaKey := strings.TrimSpace(metadataString(req.Metadata, "yescaptcha_key"))
	if yescaptchaKey == "" {
		yescaptchaKey = strings.TrimSpace(os.Getenv("YESCAPTCHA_API_KEY"))
	}
	if yescaptchaKey == "" {
		return errorEnvelope("otp_config", "otp mode requires YESCAPTCHA_API_KEY env or metadata.yescaptcha_key", false), 1
	}

	imapCfg := imapConfigFromMetadata(req.Metadata, email)
	literalOTP := strings.TrimSpace(metadataString(req.Metadata, "otp"))
	if literalOTP == "" && imapCfg == nil {
		return errorEnvelope("otp_config",
			"otp mode requires either metadata.otp (literal 6-digit code) or IMAP config (mail_host, mail_user, mail_pass)",
			false), 1
	}

	// Test-only overrides — production callers do not set these.
	opts := otpStartOptions{
		AuthBase:       strings.TrimSpace(metadataString(req.Metadata, "auth_base_override")),
		AuthMeEndpoint: strings.TrimSpace(metadataString(req.Metadata, "auth_me_override")),
		YesCaptchaBase: strings.TrimSpace(metadataString(req.Metadata, "yescaptcha_base_override")),
	}

	// Bound the Start call so a stuck upstream doesn't stall CPA.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	otpSt, err := startCursorOTP(ctx, email, yescaptchaKey, imapCfg, literalOTP, opts)
	if err != nil {
		return errorEnvelope("otp_start", err.Error(), false), 1
	}

	state := uuid.NewString()
	now := time.Now()
	entry := &loginEntry{
		Mode:      loginModeOTP,
		OTPState:  otpSt,
		CreatedAt: now,
		ExpiresAt: now.Add(loginSessionTTL),
	}
	storeLoginEntry(state, entry)

	resp := authLoginStartResponse{
		Provider:  pluginName,
		URL:       "",
		State:     state,
		ExpiresAt: entry.ExpiresAt,
		Metadata: map[string]any{
			"mode":         loginModeOTP,
			"otp_pending":  true,
			"email":        email,
			"magic_state":  otpSt.ChallengeID,
			"inbox_source": describeInboxSource(imapCfg, literalOTP),
		},
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// startLoginOTPManual is the legacy branch — returns the login-hub
// hint URL and stays pending. Selected when the operator passes
// metadata.manual=true or omits the email (which is the shape old CPA
// callers used).
func startLoginOTPManual(req *authLoginStartRequest) ([]byte, int) {
	state := uuid.NewString()
	now := time.Now()
	msg := otpPendingMessage()
	entry := &loginEntry{
		Mode:      loginModeOTP,
		Message:   msg,
		CreatedAt: now,
		ExpiresAt: now.Add(loginSessionTTL),
	}
	storeLoginEntry(state, entry)

	resp := authLoginStartResponse{
		Provider:  pluginName,
		URL:       otpLoginHubURL,
		State:     state,
		ExpiresAt: entry.ExpiresAt,
		Metadata: map[string]any{
			"mode":            loginModeOTP,
			"login_hub_hint":  otpLoginHubURL,
			"instruction":     msg,
			"manual_workflow": true,
		},
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// imapConfigFromMetadata builds an imapConfig from the metadata bag,
// or returns nil if no IMAP credentials were provided.
func imapConfigFromMetadata(m map[string]any, email string) *imapConfig {
	host := strings.TrimSpace(metadataString(m, "mail_host"))
	user := strings.TrimSpace(metadataString(m, "mail_user"))
	pass := strings.TrimSpace(metadataString(m, "mail_pass"))
	if host == "" && user == "" && pass == "" {
		return nil
	}
	if host == "" {
		host = "outlook.office365.com"
	}
	if user == "" {
		user = email
	}
	if pass == "" {
		return nil
	}
	port := 993
	if p := metadataInt(m, "mail_port"); p > 0 {
		port = p
	}
	return &imapConfig{Host: host, Port: port, Username: user, Password: pass}
}

// describeInboxSource reports how Poll will fetch the OTP — used in
// the Start response for operator visibility.
func describeInboxSource(cfg *imapConfig, literal string) string {
	if literal != "" {
		return "literal"
	}
	if cfg != nil {
		return "imap:" + cfg.Host
	}
	return "unknown"
}

// metadataBool returns the boolean value at key in m. Accepts real
// bools and stringly "true"/"1"/"yes" values.
func metadataBool(m map[string]any, key string) bool {
	if v, ok := m[key]; ok {
		switch t := v.(type) {
		case bool:
			return t
		case string:
			switch strings.ToLower(strings.TrimSpace(t)) {
			case "true", "yes", "1", "on":
				return true
			}
		}
	}
	return false
}

// metadataInt returns the int value at key in m, accepting numeric
// and stringly forms.
func metadataInt(m map[string]any, key string) int {
	if v, ok := m[key]; ok {
		switch t := v.(type) {
		case int:
			return t
		case int64:
			return int(t)
		case float64:
			return int(t)
		case string:
			var n int
			if _, err := fmt.Sscanf(strings.TrimSpace(t), "%d", &n); err == nil {
				return n
			}
		}
	}
	return 0
}

// otpPendingMessage returns the constant instructional message
// used by both Start and Poll for OTP mode.
func otpPendingMessage() string {
	return "Cursor OTP login requires a real browser (Turnstile + Camoufox) " +
		"and cannot run inside the plugin. Run the login-hub service " +
		"(see " + otpLoginHubURL + ") and import the resulting CPA JSON " +
		"through /v0/management/oauth-callback."
}

// finalizeSuccessPoll marshals a success poll response with a
// populated Auth field.
func finalizeSuccessPoll(a *authData) ([]byte, int) {
	resp := authLoginPollResponse{
		Status:  authLoginStatusSuccess,
		Message: "",
		Auth:    *a,
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// finalizePendingPoll marshals a poll response that keeps the client polling.
func finalizePendingPoll(message string) ([]byte, int) {
	resp := authLoginPollResponse{
		Status:  authLoginStatusPending,
		Message: message,
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// finalizeErrorPoll marshals an error poll response but stays inside
// the ok envelope, matching how other providers signal login errors.
func finalizeErrorPoll(message string) ([]byte, int) {
	resp := authLoginPollResponse{
		Status:  authLoginStatusError,
		Message: message,
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// buildAuthFileFromPoll folds a Cursor PollResult into the CPA
// on-disk auth shape. Mirrors what cmd/cursor-login does when it
// writes the JSON to disk, minus the operator knobs.
func buildAuthFileFromPoll(pr *auth.PollResult) *cpaformat.AuthFile {
	now := time.Now()
	email := extractEmailFromJWT(pr.AccessToken)
	userID := extractUserIDFromAuthID(pr.AuthID)
	machineID, _ := auth.GetMachineID()
	macMachineID, _ := auth.GetMacMachineID()

	return &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:         cpaformat.ProviderType,
			AccessToken:  pr.AccessToken,
			RefreshToken: pr.RefreshToken,
			Email:        email,
			UserID:       userID,
			AuthID:       pr.AuthID,
			AuthKind:     pr.Type,
			MachineID:    machineID,
			MacMachineID: macMachineID,
			IssuedAt:     cpaformat.FormatTime(now),
			LastRefresh:  cpaformat.FormatTime(now),
			Expired:      cpaformat.FormatTime(auth.ExpiresAtFromJWT(pr.AccessToken)),
			Refreshable:  pr.RefreshToken != "",
		},
	}
}

// buildAuthData packages a CPA-shape auth file into the plugin's
// authData response type. Emulates handleAuthParse so both entry
// points produce identical records.
func buildAuthData(authFile *cpaformat.AuthFile, fallbackID string) (*authData, error) {
	if authFile == nil {
		return nil, fmt.Errorf("nil auth file")
	}
	if err := authFile.Validate(); err != nil {
		return nil, err
	}
	storage, err := json.Marshal(authFile)
	if err != nil {
		return nil, fmt.Errorf("marshal storage: %w", err)
	}
	label := strings.TrimSpace(authFile.Email)
	if label == "" {
		label = pluginName
	}
	metadata := map[string]any{
		"type":       cpaformat.ProviderType,
		"email":      authFile.Email,
		"user_id":    authFile.UserID,
		"auth_id":    authFile.AuthID,
		"expired":    authFile.Expired,
		"machine_id": authFile.MachineID,
	}
	attributes := map[string]string{
		"provider": pluginName,
	}
	if authFile.MachineID != "" {
		attributes["machine_id"] = authFile.MachineID
	}
	if authFile.MacMachineID != "" {
		attributes["mac_machine_id"] = authFile.MacMachineID
	}
	recordID := fallbackID
	if authFile.Email != "" {
		recordID = "cursor-" + cpaformat.SanitizeEmail(authFile.Email)
	}
	return &authData{
		Provider:    pluginName,
		ID:          recordID,
		FileName:    authFile.FileName(),
		Label:       label,
		Prefix:      authFile.Prefix,
		ProxyURL:    authFile.ProxyURL,
		Disabled:    authFile.Disabled,
		StorageJSON: storage,
		Metadata:    metadata,
		Attributes:  attributes,
	}, nil
}

// extractEmailFromJWT reads the "sub" claim off a JWT-like access
// token. Cursor's tokens sometimes carry an email in sub, sometimes
// an auth0 user id. We return the email-shaped value or "" so
// downstream label logic falls back to the provider name.
func extractEmailFromJWT(token string) string {
	claims, err := auth.DecodeJWTClaims(token)
	if err != nil || claims == nil {
		return ""
	}
	if claims.Sub == "" {
		return ""
	}
	if strings.Contains(claims.Sub, "@") {
		return claims.Sub
	}
	return ""
}

// extractUserIDFromAuthID pulls the trailing user id from an
// "auth0|user_..." / "workos|user_..." string, matching what
// auth.extractUserID does for the OAuth CLI.
func extractUserIDFromAuthID(authID string) string {
	if authID == "" {
		return ""
	}
	if i := strings.Index(authID, "|"); i >= 0 {
		return authID[i+1:]
	}
	return authID
}

// metadataString returns the first string value for key in m, or "".
func metadataString(m map[string]any, key string) string {
	if len(m) == 0 {
		return ""
	}
	v, ok := m[key]
	if !ok {
		return ""
	}
	switch s := v.(type) {
	case string:
		return s
	case fmt.Stringer:
		return s.String()
	}
	return ""
}
