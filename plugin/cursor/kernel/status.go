// Rich per-account status helpers used by the management.* handlers.
//
// This file provides the pool-status half of the Cursor plugin. It
// does NOT touch executor.execute / execute_stream / count_tokens —
// those are being finished in a sibling worktree and their case
// blocks in plugin/cursor/main.go must remain untouched.
//
// Data flow:
//
//	CPA admin panel
//	    │  HTTP /v0/management/cli-proxy-api/cursor/accounts
//	    ▼
//	pluginhost -> management.handle (ABI)
//	    │  managementHandle() in this package
//	    ▼
//	status registry + FetchAccountStatus()  ── usage.Client.Fetch ──▶ Cursor api2
//
// The registry keeps one AccountStatus per account with a 30s TTL so
// a full pool listing does not fan out an /GetMe + /GetCurrentPeriodUsage
// call on every browser refresh.
package kernel

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
	"github.com/router-for-me/cursor-proto/usage"
)

// TokenBreakdown is one window's worth of prompt / response / cache
// token counts, mirroring usage.TokenUsage. Cache tokens matter for
// Cursor: cache_read is effectively free, cache_write is billable
// but at a reduced rate.
type TokenBreakdown struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// AccountStatus is the rich per-account view CPA's admin panel needs.
//
// All monetary values are cents. Time fields serialise to RFC3339
// (or null when zero) so the panel can render them directly.
type AccountStatus struct {
	// Identity
	Email   string `json:"email"`
	UserID  string `json:"user_id,omitempty"`
	AuthID  string `json:"auth_id,omitempty"`
	Country string `json:"country,omitempty"`

	// Token
	JwtExpiresAt time.Time     `json:"jwt_expires_at,omitempty"`
	JwtExpiresIn time.Duration `json:"jwt_expires_in_ns,omitempty"`
	Refreshable  bool          `json:"refreshable"`

	// Cursor plan & quota — the three progress bars Cursor's own
	// dashboard renders (Total / Auto+Composer / API) plus the
	// windowed spend counters. Fields prefixed AutoSpend / APISpend
	// / TotalPercent were added when v0.2.4 landed
	// usage-snapshot-categorized-spend; keep this struct in lockstep
	// with usage.Snapshot so admin-panel changes need only one edit.
	Plan             string  `json:"plan"`
	SignUpType       string  `json:"sign_up_type,omitempty"`
	SpendCents       int64   `json:"spend_cents"`
	LimitCents       int64   `json:"limit_cents"`
	RemainingCents   int64   `json:"remaining_cents"`
	Spend24hCents    int64   `json:"spend_24h_cents"`
	Spend7dCents     int64   `json:"spend_7d_cents"`
	Spend30dCents    int64   `json:"spend_30d_cents"`
	AutoSpendCents   int64   `json:"auto_spend_cents"`
	AutoLimitCents   int64   `json:"auto_limit_cents"`
	AutoPercentUsed  float64 `json:"auto_percent_used"`
	APISpendCents    int64   `json:"api_spend_cents"`
	APILimitCents    int64   `json:"api_limit_cents"`
	APIPercentUsed   float64 `json:"api_percent_used"`
	TotalPercentUsed float64 `json:"total_percent_used"`

	// Windowed token breakdown, one bucket per window. Same fields
	// downstream saw appear on /v1/usage in v0.2.7 — surfacing them
	// on the admin panel here so per-account drilldown gets the
	// same detail. Zero values are legitimate ("no traffic in
	// this window").
	Tokens24h TokenBreakdown `json:"tokens_24h"`
	Tokens7d  TokenBreakdown `json:"tokens_7d"`
	Tokens30d TokenBreakdown `json:"tokens_30d"`

	// Rate / slow pool
	InSlowPool        bool      `json:"in_slow_pool"`
	SlowReason        string    `json:"slow_reason,omitempty"`
	RateLimitResetAt  time.Time `json:"rate_limit_reset_at,omitempty"`
	RateLimitDaysLeft int32     `json:"rate_limit_days_left,omitempty"`
	HardLimitCents    int64     `json:"hard_limit_cents"`

	// Operational — filled in by executor recording (out of scope for
	// this pass). Kept in the shape so the JSON contract is stable.
	LastRequestAt time.Time `json:"last_request_at,omitempty"`
	RequestCount  int64     `json:"request_count"`
	FailureCount  int64     `json:"failure_count"`
	LastErrorCode string    `json:"last_error_code,omitempty"`

	// Compatibility hint — computed from Country + Plan.
	CanCallClaude   bool     `json:"can_call_claude"`
	CanCallComposer bool     `json:"can_call_composer"`
	Models          []string `json:"models,omitempty"`

	// FetchedAt marks when Snapshot data was pulled. Used to gate the
	// cache; not part of the strict admin-panel contract but harmless.
	FetchedAt time.Time `json:"fetched_at,omitempty"`
}

// claudeCountryAllowlist is the set of ISO-3166 country codes where
// Cursor accounts are believed to be able to call Anthropic Claude
// models. Cursor gates Claude access via geo, and we err on the
// permissive side — accounts outside the list fall back to composer
// models. Keep the list explicit so operators can audit it.
var claudeCountryAllowlist = map[string]bool{
	"US": true, "CA": true, "UK": true, "GB": true, "IE": true,
	"AU": true, "NZ": true,
	"DE": true, "FR": true, "IT": true, "ES": true, "NL": true, "BE": true,
	"SE": true, "NO": true, "DK": true, "FI": true, "IS": true,
	"AT": true, "CH": true, "PT": true, "PL": true, "CZ": true,
	"JP": true, "KR": true, "SG": true, "TW": true, "HK": true,
	"IL": true, "AE": true,
}

// modelsForAccount returns the model subset an account is likely to be
// able to invoke, based on Country + Plan. This is a hint for the UI —
// it is not enforced by the executor.
func modelsForAccount(s *AccountStatus) []string {
	// Everyone gets Composer + Gemini + Grok. Claude and GPT are the
	// two families that Cursor typically gates.
	base := []string{
		"composer-2.5",
		"composer-2",
		"gemini-2.5-pro",
		"gemini-2.5-flash",
		"grok-code",
		"cursor-small",
	}
	if s.CanCallClaude {
		base = append(base,
			"claude-4.5-sonnet",
			"claude-4.5-haiku",
			"claude-opus-4.1",
		)
	}
	// GPT is generally available everywhere Cursor operates.
	base = append(base,
		"gpt-5",
		"gpt-5-mini",
		"gpt-5-codex",
	)
	sort.Strings(base)
	return base
}

// SnapshotFetcher is the seam between the plugin and the usage
// package. Test code overrides it; production wires
// defaultSnapshotFetcher which calls usage.New(executor.NewClient).
type SnapshotFetcher func(ctx context.Context, acc *auth.Account) (*usage.Snapshot, error)

// defaultSnapshotFetcher is the wiring used in production.
func defaultSnapshotFetcher(ctx context.Context, acc *auth.Account) (*usage.Snapshot, error) {
	client := usage.New(executor.NewClient(acc))
	return client.Fetch(ctx)
}

// FetchAccountStatus builds an AccountStatus from a Cursor Account and
// (optionally) a live usage snapshot. It never returns nil on success;
// on partial failure the returned status carries whatever fields we
// managed to derive from the token itself.
func FetchAccountStatus(ctx context.Context, acc *auth.Account) (*AccountStatus, error) {
	return fetchAccountStatusWith(ctx, acc, defaultSnapshotFetcher)
}

// fetchAccountStatusWith is the testable form of FetchAccountStatus.
func fetchAccountStatusWith(ctx context.Context, acc *auth.Account, fetch SnapshotFetcher) (*AccountStatus, error) {
	if acc == nil {
		return nil, errors.New("nil account")
	}
	s := &AccountStatus{
		Email:           acc.Email,
		UserID:          acc.UserID,
		AuthID:          acc.AuthID,
		Refreshable:     strings.TrimSpace(acc.RefreshToken) != "",
		CanCallComposer: true,
		FetchedAt:       time.Now().UTC(),
	}
	if exp, ok := decodeJWTExpiry(acc.AccessToken); ok {
		s.JwtExpiresAt = exp
		s.JwtExpiresIn = time.Until(exp)
	}
	if fetch != nil {
		snap, err := fetch(ctx, acc)
		if err == nil && snap != nil {
			applySnapshot(s, snap)
		} else if err != nil {
			// Surface the error but keep the partial status — the
			// admin panel is more useful with a stub row than a
			// missing account.
			return s, fmt.Errorf("fetch snapshot: %w", err)
		}
	}
	if s.Country != "" {
		s.CanCallClaude = claudeCountryAllowlist[strings.ToUpper(s.Country)]
	}
	if s.Plan == "" {
		s.Plan = "unknown"
	}
	s.Models = modelsForAccount(s)
	return s, nil
}

// applySnapshot copies the interesting fields from a usage.Snapshot
// onto an AccountStatus. Kept separate so tests can construct a
// Snapshot literal and verify the mapping.
func applySnapshot(s *AccountStatus, snap *usage.Snapshot) {
	if snap.Email != "" && s.Email == "" {
		s.Email = snap.Email
	}
	s.Country = snap.Country
	s.SignUpType = snap.SignUpType
	s.SpendCents = snap.TotalSpend
	s.LimitCents = snap.Limit
	s.RemainingCents = snap.Remaining
	s.Spend24hCents = snap.Spend24h
	s.Spend7dCents = snap.Spend7d
	s.Spend30dCents = snap.Spend30d
	// Categorized spend (v0.2.4 addition on the wire; surfaced here
	// so the admin panel matches Cursor's own dashboard three-bar
	// view). Percent* are proto3 doubles precomputed by the backend
	// — use them verbatim so downstream matches byte-for-byte.
	s.AutoSpendCents = snap.AutoSpend
	s.AutoLimitCents = snap.AutoLimit
	s.AutoPercentUsed = snap.AutoPercentUsed
	s.APISpendCents = snap.APISpend
	s.APILimitCents = snap.APILimit
	s.APIPercentUsed = snap.APIPercentUsed
	s.TotalPercentUsed = snap.TotalPercentUsed
	// Windowed token breakdown (v0.2.7). All four counters × three
	// windows come from the same GetAggregatedUsageEvents RPC that
	// already filled Spend24h/7d/30d — zero extra upstream calls.
	s.Tokens24h = tokenBreakdownFrom(snap.Tokens24h)
	s.Tokens7d = tokenBreakdownFrom(snap.Tokens7d)
	s.Tokens30d = tokenBreakdownFrom(snap.Tokens30d)
	s.InSlowPool = snap.InSlowPool
	s.SlowReason = snap.SlowReason
	if snap.RateLimitResetAt != nil {
		s.RateLimitResetAt = *snap.RateLimitResetAt
	}
	s.RateLimitDaysLeft = snap.RateLimitResetDaysRemaining
	s.HardLimitCents = snap.HardLimit
	s.Plan = derivePlan(snap)
}

// tokenBreakdownFrom converts a usage.TokenUsage into the plugin's
// admin-panel-facing TokenBreakdown. Separate helper so
// applySnapshot stays a straight assignment list.
func tokenBreakdownFrom(t usage.TokenUsage) TokenBreakdown {
	return TokenBreakdown{
		Input:      t.Input,
		Output:     t.Output,
		CacheRead:  t.CacheRead,
		CacheWrite: t.CacheWrite,
	}
}

// derivePlan inspects the Snapshot fields to produce a human-readable
// plan label. Cursor does not expose stripe_subscription_status in
// this Snapshot shape, so we fall back to heuristics on Limit + Hard
// limit + PremiumRequests. "unknown" is the safe default.
func derivePlan(snap *usage.Snapshot) string {
	if snap == nil {
		return "unknown"
	}
	switch strings.ToLower(strings.TrimSpace(snap.SignUpType)) {
	case "business", "team", "enterprise":
		return "Team"
	}
	// Pro accounts typically have a positive included spend limit
	// and premium requests enabled. Free accounts show a $0 limit.
	if snap.UsageBasedPremiumRequestsEnabled || snap.Limit > 0 {
		return "Pro"
	}
	if snap.Fetched.CurrentPeriodUsage && snap.Limit == 0 {
		return "Free"
	}
	return "unknown"
}

// decodeJWTExpiry pulls the "exp" claim off a JWT-style access token.
// Cursor tokens are opaque JWTs; we do NOT verify the signature (we
// have no way to). Only used to render "expires in N minutes" in the UI.
func decodeJWTExpiry(token string) (time.Time, bool) {
	token = strings.TrimSpace(token)
	if token == "" {
		return time.Time{}, false
	}
	// Some Cursor storages prefix tokens with an identity segment
	// like "auth0|user_xxx::eyJhbG...". Everything before the last
	// "::" is not a JWT.
	if i := strings.LastIndex(token, "::"); i >= 0 {
		token = token[i+2:]
	}
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Some encoders leave padding on; try the padded decoder.
		raw, err = base64.StdEncoding.DecodeString(parts[1])
		if err != nil {
			return time.Time{}, false
		}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return time.Time{}, false
	}
	if claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0).UTC(), true
}

// authRegistry tracks Cursor accounts that the plugin has observed.
//
// Accounts appear in the registry from two sources:
//  1. auth.parse / auth.refresh calls — CPA hands us the storage JSON.
//  2. discoverAuthDir() — falls back to reading files off disk if the
//     admin caller wants a snapshot before CPA has issued a refresh.
type authRegistry struct {
	mu       sync.RWMutex
	accounts map[string]*auth.Account // keyed by email
	cache    map[string]cacheEntry    // keyed by email
	ttl      time.Duration
	fetch    SnapshotFetcher
	authDir  string // discovered lazily; may be empty
}

type cacheEntry struct {
	status  *AccountStatus
	fetched time.Time
}

// globalRegistry is the process-wide singleton. Multiple goroutines
// (management.handle calls) share it, so all methods are mutex-protected.
var globalRegistry = newAuthRegistry(30 * time.Second)

func newAuthRegistry(ttl time.Duration) *authRegistry {
	return &authRegistry{
		accounts: map[string]*auth.Account{},
		cache:    map[string]cacheEntry{},
		ttl:      ttl,
		fetch:    defaultSnapshotFetcher,
	}
}

// Register stores or updates an account entry. Callers pass the
// fully-materialised *auth.Account (with machine ids and session
// defaults filled in).
func (r *authRegistry) Register(a *auth.Account) {
	if a == nil || strings.TrimSpace(a.Email) == "" {
		return
	}
	r.mu.Lock()
	r.accounts[a.Email] = a
	// Invalidate cache on re-register — the token may have changed.
	delete(r.cache, a.Email)
	r.mu.Unlock()
}

// Get returns the tracked account and whether it exists.
func (r *authRegistry) Get(email string) (*auth.Account, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	acc, ok := r.accounts[email]
	return acc, ok
}

// List returns a stable, alphabetical slice of accounts.
func (r *authRegistry) List() []*auth.Account {
	r.mu.RLock()
	out := make([]*auth.Account, 0, len(r.accounts))
	emails := make([]string, 0, len(r.accounts))
	for k := range r.accounts {
		emails = append(emails, k)
	}
	r.mu.RUnlock()
	sort.Strings(emails)
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, e := range emails {
		if acc := r.accounts[e]; acc != nil {
			out = append(out, acc)
		}
	}
	return out
}

// Invalidate removes a cached AccountStatus so the next Status() call
// will fetch fresh from Cursor. Used by the /probe endpoint.
func (r *authRegistry) Invalidate(email string) {
	r.mu.Lock()
	delete(r.cache, email)
	r.mu.Unlock()
}

// Status returns an AccountStatus for email, using a 30s cache to
// avoid hammering Cursor. Force=true bypasses the cache entirely.
func (r *authRegistry) Status(ctx context.Context, email string, force bool) (*AccountStatus, error) {
	r.mu.RLock()
	acc, ok := r.accounts[email]
	entry, cached := r.cache[email]
	fetch := r.fetch
	ttl := r.ttl
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("account not registered: %s", email)
	}
	if !force && cached && time.Since(entry.fetched) < ttl && entry.status != nil {
		return entry.status, nil
	}
	status, err := fetchAccountStatusWith(ctx, acc, fetch)
	if status != nil {
		r.mu.Lock()
		r.cache[email] = cacheEntry{status: status, fetched: time.Now()}
		r.mu.Unlock()
	}
	return status, err
}

// SetFetcher swaps out the SnapshotFetcher — used by tests to inject
// mock data.
func (r *authRegistry) SetFetcher(f SnapshotFetcher) {
	r.mu.Lock()
	r.fetch = f
	r.mu.Unlock()
}

// SetAuthDir points the registry at a directory of cursor-*.json
// auth files. Used to seed the registry on the first management
// call so operators do not need to wait for CPA to hand us auths.
func (r *authRegistry) SetAuthDir(dir string) {
	r.mu.Lock()
	r.authDir = dir
	r.mu.Unlock()
}

// LoadFromDisk walks the configured auth directory and registers
// every cursor-*.json it can parse. Missing / unset dir is a no-op.
// Returns the number of accounts registered.
func (r *authRegistry) LoadFromDisk() (int, error) {
	r.mu.RLock()
	dir := r.authDir
	r.mu.RUnlock()
	if strings.TrimSpace(dir) == "" {
		return 0, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read auth dir: %w", err)
	}
	count := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "cursor-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(dir, name)
		buf, errRead := os.ReadFile(path)
		if errRead != nil {
			continue
		}
		file, errParse := cpaformat.Unmarshal(buf)
		if errParse != nil || file.Type != cpaformat.ProviderType {
			continue
		}
		acc := accountFromAuthFile(file)
		if acc == nil {
			continue
		}
		r.Register(acc)
		count++
	}
	return count, nil
}

// accountFromAuthFile converts a cpaformat.AuthFile into an
// auth.Account so the executor / usage clients can use it.
func accountFromAuthFile(file *cpaformat.AuthFile) *auth.Account {
	if file == nil || strings.TrimSpace(file.AccessToken) == "" {
		return nil
	}
	issued, _ := cpaformat.ParseTime(file.IssuedAt)
	expired, _ := cpaformat.ParseTime(file.Expired)
	acc := &auth.Account{
		Email:        file.Email,
		UserID:       file.UserID,
		AccessToken:  file.AccessToken,
		RefreshToken: file.RefreshToken,
		AuthID:       file.AuthID,
		AuthType:     file.AuthKind,
		IssuedAt:     issued,
		ExpiresAt:    expired,
		MachineID:    file.MachineID,
		MacMachineID: file.MacMachineID,
	}
	acc.FillSessionDefaults(time.Now())
	return acc
}
