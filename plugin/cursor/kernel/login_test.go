package kernel

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// timeNow / timeMinute are thin aliases so the tests can build
// with the standard time package without shadowing it.
func timeNow() time.Time        { return time.Now() }
func timeMinute() time.Duration { return time.Minute }

// clearLoginSessions wipes the package-level sync.Map so tests do
// not leak state into each other.
func clearLoginSessions(t *testing.T) {
	t.Helper()
	loginSessions.Range(func(k, _ any) bool {
		loginSessions.Delete(k)
		return true
	})
}

// startLoginBody builds the JSON payload for auth.login.start with
// the provided metadata attached.
func startLoginBody(t *testing.T, metadata map[string]any) []byte {
	t.Helper()
	buf, err := json.Marshal(map[string]any{
		"Provider": "cursor",
		"Metadata": metadata,
	})
	if err != nil {
		t.Fatalf("marshal start body: %v", err)
	}
	return buf
}

// pollLoginBody builds the JSON payload for auth.login.poll.
func pollLoginBody(t *testing.T, state string, metadata map[string]any) []byte {
	t.Helper()
	buf, err := json.Marshal(map[string]any{
		"Provider": "cursor",
		"State":    state,
		"Metadata": metadata,
	})
	if err != nil {
		t.Fatalf("marshal poll body: %v", err)
	}
	return buf
}

// unmarshalStartResult decodes an ok envelope + start response.
func unmarshalStartResult(t *testing.T, raw []byte) authLoginStartResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, string(raw))
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var out authLoginStartResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("unmarshal start response: %v", err)
	}
	return out
}

// unmarshalPollResult decodes an ok envelope + poll response.
func unmarshalPollResult(t *testing.T, raw []byte) authLoginPollResponse {
	t.Helper()
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal envelope: %v (raw=%s)", err, string(raw))
	}
	if !env.OK {
		t.Fatalf("envelope not OK: %+v", env.Error)
	}
	var out authLoginPollResponse
	if err := json.Unmarshal(env.Result, &out); err != nil {
		t.Fatalf("unmarshal poll response: %v", err)
	}
	return out
}

// TestAuthLoginStart_OAuth ensures oauth mode returns a valid state,
// non-empty URL, and metadata carrying mode + verifier.
func TestAuthLoginStart_OAuth(t *testing.T) {
	clearLoginSessions(t)
	raw, rc := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode": "oauth",
	}))
	if rc != 0 {
		t.Fatalf("rc = %d, envelope=%s", rc, string(raw))
	}
	resp := unmarshalStartResult(t, raw)
	if resp.State == "" {
		t.Fatal("State is empty")
	}
	if resp.URL == "" {
		t.Fatal("URL is empty")
	}
	if !strings.Contains(resp.URL, "loginDeepControl") {
		t.Errorf("URL should point at cursor login page, got %q", resp.URL)
	}
	if resp.Metadata["mode"] != "oauth" {
		t.Errorf("Metadata.mode = %v, want oauth", resp.Metadata["mode"])
	}
	if v, ok := resp.Metadata["verifier"].(string); !ok || v == "" {
		t.Errorf("Metadata.verifier missing: %v", resp.Metadata["verifier"])
	}
	if resp.ExpiresAt.IsZero() {
		t.Errorf("ExpiresAt is zero")
	}
}

// TestAuthLoginStart_DefaultsToOAuth ensures a missing mode defaults
// to the oauth flow instead of returning an error.
func TestAuthLoginStart_DefaultsToOAuth(t *testing.T) {
	clearLoginSessions(t)
	raw, rc := dispatch("auth.login.start", startLoginBody(t, nil))
	if rc != 0 {
		t.Fatalf("rc = %d, envelope=%s", rc, string(raw))
	}
	resp := unmarshalStartResult(t, raw)
	if resp.Metadata["mode"] != "oauth" {
		t.Errorf("default mode = %v, want oauth", resp.Metadata["mode"])
	}
}

// TestAuthLoginStart_UnknownMode returns a bad_request error with a
// message enumerating valid modes.
func TestAuthLoginStart_UnknownMode(t *testing.T) {
	clearLoginSessions(t)
	raw, rc := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode": "totally-made-up",
	}))
	if rc == 0 {
		t.Fatalf("expected non-zero rc for unknown mode, got envelope=%s", string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK {
		t.Fatal("expected OK=false")
	}
	if env.Error == nil || env.Error.Code != "bad_request" {
		t.Fatalf("expected bad_request, got %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "oauth") ||
		!strings.Contains(env.Error.Message, "ide") ||
		!strings.Contains(env.Error.Message, "otp") {
		t.Errorf("error message should list valid modes: %q", env.Error.Message)
	}
}

// TestAuthLoginStart_OTP returns pending mode with a login-hub hint
// URL, since the plugin cannot drive Turnstile from inside itself.
func TestAuthLoginStart_OTP(t *testing.T) {
	clearLoginSessions(t)
	raw, rc := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode": "otp",
	}))
	if rc != 0 {
		t.Fatalf("rc = %d, envelope=%s", rc, string(raw))
	}
	resp := unmarshalStartResult(t, raw)
	if resp.State == "" {
		t.Fatal("State is empty")
	}
	if resp.URL == "" || !strings.Contains(resp.URL, "login-hub") {
		t.Errorf("expected URL to point at login-hub docs, got %q", resp.URL)
	}
	if resp.Metadata["mode"] != "otp" {
		t.Errorf("Metadata.mode = %v, want otp", resp.Metadata["mode"])
	}
	if v, ok := resp.Metadata["manual_workflow"].(bool); !ok || !v {
		t.Errorf("Metadata.manual_workflow = %v, want true", resp.Metadata["manual_workflow"])
	}

	// Poll should keep returning pending with the same login-hub hint.
	rawPoll, rcPoll := dispatch("auth.login.poll", pollLoginBody(t, resp.State, nil))
	if rcPoll != 0 {
		t.Fatalf("poll rc = %d, envelope=%s", rcPoll, string(rawPoll))
	}
	poll := unmarshalPollResult(t, rawPoll)
	if poll.Status != authLoginStatusPending {
		t.Errorf("Status = %q, want pending", poll.Status)
	}
	if !strings.Contains(poll.Message, "login-hub") {
		t.Errorf("Message should reference login-hub: %q", poll.Message)
	}
}

// TestAuthLoginPoll_UnknownState returns an error envelope.
func TestAuthLoginPoll_UnknownState(t *testing.T) {
	clearLoginSessions(t)
	raw, rc := dispatch("auth.login.poll", pollLoginBody(t, "not-a-real-state", nil))
	if rc == 0 {
		t.Fatalf("expected non-zero rc for unknown state, got envelope=%s", string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK {
		t.Fatal("expected OK=false")
	}
	if env.Error == nil || env.Error.Code != "unknown_state" {
		t.Errorf("expected unknown_state, got %+v", env.Error)
	}
}

// TestAuthLoginPoll_OAuthPending exercises the wire path: after
// Start, a Poll that hits a fake api2 endpoint returning 204 (no
// content) should map to a pending status.
func TestAuthLoginPoll_OAuthPending(t *testing.T) {
	clearLoginSessions(t)
	// api2 stub that always returns non-200 → PollResult == nil.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	rawStart, rcStart := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode":          "oauth",
		"poll_url_base": srv.URL,
	}))
	if rcStart != 0 {
		t.Fatalf("start rc = %d, envelope=%s", rcStart, string(rawStart))
	}
	start := unmarshalStartResult(t, rawStart)

	rawPoll, rcPoll := dispatch("auth.login.poll", pollLoginBody(t, start.State, nil))
	if rcPoll != 0 {
		t.Fatalf("poll rc = %d, envelope=%s", rcPoll, string(rawPoll))
	}
	poll := unmarshalPollResult(t, rawPoll)
	if poll.Status != authLoginStatusPending {
		t.Errorf("Status = %q, want pending", poll.Status)
	}
	if !strings.Contains(poll.Message, "waiting") {
		t.Errorf("Message should mention waiting: %q", poll.Message)
	}
}

// TestAuthLoginPoll_OAuthSuccess feeds a fake successful api2 response
// through and confirms we materialise a full AuthData envelope.
func TestAuthLoginPoll_OAuthSuccess(t *testing.T) {
	clearLoginSessions(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
            "accessToken": "cursor-access-token",
            "refreshToken": "cursor-refresh-token",
            "authId": "auth0|user_TEST123",
            "authType": "Auth_0"
        }`))
	}))
	defer srv.Close()

	rawStart, rcStart := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode":          "oauth",
		"poll_url_base": srv.URL,
	}))
	if rcStart != 0 {
		t.Fatalf("start rc = %d, envelope=%s", rcStart, string(rawStart))
	}
	start := unmarshalStartResult(t, rawStart)

	rawPoll, rcPoll := dispatch("auth.login.poll", pollLoginBody(t, start.State, nil))
	if rcPoll != 0 {
		t.Fatalf("poll rc = %d, envelope=%s", rcPoll, string(rawPoll))
	}
	poll := unmarshalPollResult(t, rawPoll)
	if poll.Status != authLoginStatusSuccess {
		t.Fatalf("Status = %q, want success (message=%q)", poll.Status, poll.Message)
	}
	if poll.Auth.Provider != "cursor" {
		t.Errorf("Auth.Provider = %q, want cursor", poll.Auth.Provider)
	}
	if len(poll.Auth.StorageJSON) == 0 {
		t.Error("Auth.StorageJSON is empty")
	}
	// Storage should round-trip through cpaformat.
	var storage map[string]any
	if err := json.Unmarshal(poll.Auth.StorageJSON, &storage); err != nil {
		t.Fatalf("unmarshal storage: %v", err)
	}
	if storage["type"] != "cursor" {
		t.Errorf("storage.type = %v, want cursor", storage["type"])
	}
	if storage["access_token"] != "cursor-access-token" {
		t.Errorf("storage.access_token = %v", storage["access_token"])
	}
	if storage["auth_id"] != "auth0|user_TEST123" {
		t.Errorf("storage.auth_id = %v", storage["auth_id"])
	}
	if storage["user_id"] != "user_TEST123" {
		t.Errorf("storage.user_id = %v, want user_TEST123", storage["user_id"])
	}
	// A second poll on the same state should error (state has been consumed).
	rawPoll2, _ := dispatch("auth.login.poll", pollLoginBody(t, start.State, nil))
	var env envelope
	if err := json.Unmarshal(rawPoll2, &env); err != nil {
		t.Fatalf("unmarshal second poll: %v", err)
	}
	if env.OK {
		t.Errorf("expected second poll to fail after state consumed, got OK envelope")
	}
}

// seedIDEState writes a minimal state.vscdb that mirrors the schema
// Cursor IDE persists to globalStorage. Copied from
// cmd/cursor-export/main_test.go so this package remains self-contained.
func seedIDEState(t *testing.T, rows map[string]string) string {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=rwc")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`CREATE TABLE ItemTable (key TEXT PRIMARY KEY, value TEXT)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	for k, v := range rows {
		if _, err := db.Exec(`INSERT INTO ItemTable(key, value) VALUES(?, ?)`, k, v); err != nil {
			t.Fatalf("insert %s: %v", k, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return dbPath
}

// TestAuthLoginStart_IDE_WithFakeDB happy-path: point Start at a
// hand-built state.vscdb, expect the auth to resolve synchronously
// and Poll to hand it back as a success record.
func TestAuthLoginStart_IDE_WithFakeDB(t *testing.T) {
	clearLoginSessions(t)
	dbPath := seedIDEState(t, map[string]string{
		"cursorAuth/accessToken":      "access-abc",
		"cursorAuth/refreshToken":     "refresh-xyz",
		"cursorAuth/cachedEmail":      "someone@example.com",
		"cursorAuth/cachedSignUpType": "Auth_0",
		"cursorAuth/cachedUserID":     "user_01ABC",
		"cursorAuth/authId":           "auth0|user_01ABC",
	})

	rawStart, rcStart := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode":    "ide",
		"db_path": dbPath,
	}))
	if rcStart != 0 {
		t.Fatalf("start rc = %d, envelope=%s", rcStart, string(rawStart))
	}
	start := unmarshalStartResult(t, rawStart)
	if start.State == "" {
		t.Fatal("State is empty")
	}
	if start.Metadata["mode"] != "ide" {
		t.Errorf("Metadata.mode = %v, want ide", start.Metadata["mode"])
	}
	if start.Metadata["email"] != "someone@example.com" {
		t.Errorf("Metadata.email = %v", start.Metadata["email"])
	}

	rawPoll, rcPoll := dispatch("auth.login.poll", pollLoginBody(t, start.State, nil))
	if rcPoll != 0 {
		t.Fatalf("poll rc = %d, envelope=%s", rcPoll, string(rawPoll))
	}
	poll := unmarshalPollResult(t, rawPoll)
	if poll.Status != authLoginStatusSuccess {
		t.Fatalf("Status = %q, want success (message=%q)", poll.Status, poll.Message)
	}
	if poll.Auth.Label != "someone@example.com" {
		t.Errorf("Auth.Label = %q", poll.Auth.Label)
	}
	if poll.Auth.FileName != "cursor-someone_at_example.com.json" {
		t.Errorf("Auth.FileName = %q", poll.Auth.FileName)
	}
	var storage map[string]any
	if err := json.Unmarshal(poll.Auth.StorageJSON, &storage); err != nil {
		t.Fatalf("unmarshal storage: %v", err)
	}
	if storage["access_token"] != "access-abc" {
		t.Errorf("storage.access_token = %v", storage["access_token"])
	}
	if storage["refreshable"] != true {
		t.Errorf("storage.refreshable = %v, want true", storage["refreshable"])
	}
}

// TestAuthLoginStart_IDE_MissingDB returns an ide_unavailable
// error rather than crashing. This is the common case when CPA
// runs in a Linux container without a Cursor IDE install.
func TestAuthLoginStart_IDE_MissingDB(t *testing.T) {
	clearLoginSessions(t)
	// A path that definitely does not exist.
	missing := filepath.Join(t.TempDir(), "does", "not", "exist.vscdb")
	if _, err := os.Stat(missing); err == nil {
		t.Fatalf("test setup failed: %s exists", missing)
	}
	raw, rc := dispatch("auth.login.start", startLoginBody(t, map[string]any{
		"mode":    "ide",
		"db_path": missing,
	}))
	if rc == 0 {
		t.Fatalf("expected non-zero rc for missing IDE db, envelope=%s", string(raw))
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.OK {
		t.Fatal("expected OK=false")
	}
	if env.Error == nil || env.Error.Code != "ide_unavailable" {
		t.Errorf("expected ide_unavailable, got %+v", env.Error)
	}
	if !strings.Contains(env.Error.Message, "state.vscdb") {
		t.Errorf("error message should mention state.vscdb: %q", env.Error.Message)
	}
}

// TestPruneLoginSessions_ExpiresOldEntries verifies the prune helper
// evicts entries past their ExpiresAt.
func TestPruneLoginSessions_ExpiresOldEntries(t *testing.T) {
	clearLoginSessions(t)
	now := timeNow()
	oldState := "expired-state"
	freshState := "fresh-state"

	loginSessions.Store(oldState, &loginEntry{
		Mode:      loginModeOTP,
		ExpiresAt: now.Add(-1 * timeMinute()),
	})
	loginSessions.Store(freshState, &loginEntry{
		Mode:      loginModeOTP,
		ExpiresAt: now.Add(1 * timeMinute()),
	})
	pruneLoginSessions(now)

	if _, ok := loginSessions.Load(oldState); ok {
		t.Errorf("expired entry %q was not pruned", oldState)
	}
	if _, ok := loginSessions.Load(freshState); !ok {
		t.Errorf("fresh entry %q was incorrectly pruned", freshState)
	}
	if entry := loadLoginEntry(oldState); entry != nil {
		t.Errorf("loadLoginEntry should return nil for expired state, got %+v", entry)
	}
}
