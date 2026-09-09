// Package sand contains the HTTP surface used by Cursor's Sand (Grok Bot)
// dashboard. It deliberately keeps REST cookie authentication separate from
// the bearer-authenticated ConnectRPC usage call.
package sand

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	usagepb "github.com/router-for-me/cursor-proto/usage/pb"
)

const (
	DefaultBaseURL   = "https://cursor.com"
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64)"
	DefaultTimeout   = 20 * time.Second

	pathAuthMe       = "/api/auth/me"
	pathDashboardMe  = "/api/dashboard/get-me"
	pathAccessStatus = "/api/dashboard/get-sand-access-status"
	pathStartTrial   = "/api/dashboard/start-sand-trial"
	pathTeamAccess   = "/api/dashboard/request-sand-team-access"
	pathTeamOnboard  = "/api/dashboard/update-team-sand-onboarding-completed"
)

// Outcome is the result of a Sand claim attempt.
type Outcome string

const (
	OutcomeAlready      Outcome = "already"
	OutcomeTeamOK       Outcome = "team_ok"
	OutcomeActivated    Outcome = "activated"
	OutcomeCardRequired Outcome = "card_required"
	OutcomeFailed       Outcome = "failed"
)

// ProbeState is the result of GET /api/auth/me.
type ProbeState string

const (
	ProbeAlive   ProbeState = "alive"
	ProbeDead    ProbeState = "dead"
	ProbeUnknown ProbeState = "unknown"
)

// Usage is the small Sand-specific projection needed by claim/status. The
// raw protobuf remains available through usage.Client for callers that need
// every dashboard field.
type Usage struct {
	Unlocked                bool       `json:"unlocked"`
	HasAvailableUsage       bool       `json:"has_available_usage"`
	UsagePercent            float64    `json:"usage_percent"`
	PlanLabel               string     `json:"plan_label,omitempty"`
	NextResetAt             *time.Time `json:"next_reset_at,omitempty"`
	CurrentPeriodStart      *time.Time `json:"current_period_start,omitempty"`
	TrialExpiresAt          *time.Time `json:"trial_expires_at,omitempty"`
	IncludedLimitZero       bool       `json:"included_limit_zero"`
	HasNonZeroIncludedLimit bool       `json:"has_non_zero_included_limit"`
}

// Identity is the subset of dashboard/get-me used by the Sand flow.
type Identity struct {
	Email  string `json:"email,omitempty"`
	TeamID string `json:"team_id,omitempty"`
	UserID string `json:"user_id,omitempty"`
}

// AccessStatus is the JSON qualification response from cursor.com.
type AccessStatus struct {
	State                           string   `json:"state,omitempty"`
	PurchaseChannel                 string   `json:"purchaseChannel,omitempty"`
	BlockReason                     string   `json:"blockReason,omitempty"`
	PurchasableTiers                []string `json:"purchasableTiers,omitempty"`
	IsPaidTrialPlan                 bool     `json:"isPaidTrialPlan,omitempty"`
	UnpaidAdminNeedsPaidSeat        bool     `json:"unpaidAdminNeedsPaidSeat,omitempty"`
	PrivacyDisclaimerRequired       bool     `json:"privacyDisclaimerRequired,omitempty"`
	CanSkipOnboarding               bool     `json:"canSkipOnboarding,omitempty"`
	ProAndSuperGrokPlansGrantAccess bool     `json:"proAndSuperGrokPlansGrantAccess,omitempty"`
}

// Granted handles both the current enum spelling and the short spelling used
// by a few older dashboard responses.
func (s *AccessStatus) Granted() bool {
	if s == nil {
		return false
	}
	state := strings.ToUpper(strings.TrimSpace(s.State))
	return state == "GRANTED" || strings.HasSuffix(state, "_GRANTED")
}

// Status is a partial, operator-facing Sand snapshot. A failed optional REST
// call is recorded in Errors so one inaccessible dashboard endpoint does not
// hide a successfully fetched usage bar.
type Status struct {
	Usage  *Usage            `json:"usage,omitempty"`
	Access *AccessStatus     `json:"access,omitempty"`
	Me     *Identity         `json:"me,omitempty"`
	Errors map[string]string `json:"errors,omitempty"`
}

// ClaimResult describes a claim attempt. Server-side failures are represented
// as OutcomeFailed with Detail; Go errors are reserved for local validation,
// transport, and preflight failures.
type ClaimResult struct {
	Outcome    Outcome `json:"outcome"`
	Email      string  `json:"email,omitempty"`
	TeamID     string  `json:"team_id,omitempty"`
	Percent    float64 `json:"percent,omitempty"`
	Detail     string  `json:"detail,omitempty"`
	CardURL    string  `json:"card_url,omitempty"`
	HTTPStatus int     `json:"http_status,omitempty"`
}

// HTTPError preserves the status and a bounded response body for callers
// that need to distinguish an expired credential from a transient failure.
type HTTPError struct {
	StatusCode int
	Body       string
}

func (e *HTTPError) Error() string {
	if e == nil {
		return ""
	}
	if e.Body == "" {
		return fmt.Sprintf("cursor.com HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("cursor.com HTTP %d: %s", e.StatusCode, e.Body)
}

// UsageFetcher is injectable so claim/status logic can be tested without
// making a real ConnectRPC call. A nil fetcher uses the client's executor.
type UsageFetcher func(context.Context) (*Usage, error)

// Client talks to cursor.com REST and the Sand usage ConnectRPC for one
// account. The Account access token is used as a Bearer token for ConnectRPC
// and as user_id::JWT in the WorkosCursorSessionToken cookie for REST.
type Client struct {
	Account    *auth.Account
	Exec       *executor.Client
	HTTP       *http.Client
	BaseURL    string
	UserAgent  string
	FetchUsage UsageFetcher
}

// Option configures a Client.
type Option func(*Client)

func WithHTTPClient(client *http.Client) Option {
	return func(c *Client) {
		if client != nil {
			c.HTTP = client
		}
	}
}

func WithBaseURL(baseURL string) Option {
	return func(c *Client) {
		if strings.TrimSpace(baseURL) != "" {
			c.BaseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
		}
	}
}

func WithExecutor(exec *executor.Client) Option {
	return func(c *Client) {
		if exec != nil {
			c.Exec = exec
		}
	}
}

func WithUsageFetcher(fetcher UsageFetcher) Option {
	return func(c *Client) { c.FetchUsage = fetcher }
}

// New creates a Sand client with the production endpoints.
func New(account *auth.Account, opts ...Option) *Client {
	c := &Client{
		Account:   account,
		HTTP:      &http.Client{Timeout: DefaultTimeout},
		BaseURL:   DefaultBaseURL,
		UserAgent: DefaultUserAgent,
	}
	if account != nil {
		c.Exec = executor.NewClient(account)
	}
	for _, opt := range opts {
		if opt != nil {
			opt(c)
		}
	}
	return c
}

// GetUsage fetches the Sand/Grok Bot allowance over ConnectRPC.
func (c *Client) GetUsage(ctx context.Context) (*Usage, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c == nil {
		return nil, errors.New("sand: nil client")
	}
	if c.FetchUsage != nil {
		return c.FetchUsage(ctx)
	}
	if c.Exec == nil {
		return nil, errors.New("sand: nil executor")
	}
	resp := &usagepb.GetSandUsageStatusResponse{}
	if err := c.Exec.UnaryCall("aiserver.v1.DashboardService", "GetSandUsageStatus", &usagepb.GetSandUsageStatusRequest{}, resp); err != nil {
		return nil, err
	}
	return usageFromProto(resp), nil
}

// GetMe fetches the website dashboard identity used to choose the team or
// personal claim path.
func (c *Client) GetMe(ctx context.Context) (*Identity, error) {
	status, body, err := c.request(ctx, http.MethodPost, pathDashboardMe, []byte("{}"), true)
	if err != nil {
		return nil, err
	}
	if err := responseError(status, body); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("decode dashboard/get-me: %w", err)
	}
	return &Identity{
		Email:  rawString(raw, "email"),
		TeamID: rawID(raw, "teamId", "team_id"),
		UserID: rawID(raw, "userId", "user_id"),
	}, nil
}

// GetAccessStatus checks the website qualification state. It is read-only,
// but Cursor still requires Origin because the endpoint is CSRF-protected.
func (c *Client) GetAccessStatus(ctx context.Context) (*AccessStatus, error) {
	status, body, err := c.request(ctx, http.MethodPost, pathAccessStatus, []byte("{}"), true)
	if err != nil {
		return nil, err
	}
	if err := responseError(status, body); err != nil {
		return nil, err
	}
	var out AccessStatus
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("decode get-sand-access-status: %w", err)
	}
	return &out, nil
}

// ProbeTokenAlive uses the website's auth/me endpoint without changing any
// state. A 401/403 is a definite dead token; network and other statuses are
// unknown and returned with a diagnostic error.
func (c *Client) ProbeTokenAlive(ctx context.Context) (ProbeState, error) {
	status, body, err := c.request(ctx, http.MethodGet, pathAuthMe, nil, false)
	if err != nil {
		return ProbeUnknown, err
	}
	switch {
	case status >= 200 && status < 300:
		return ProbeAlive, nil
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return ProbeDead, nil
	default:
		return ProbeUnknown, responseError(status, body)
	}
}

// Status collects the usage bar plus website qualification/identity. It is
// intentionally partial: callers get all successful fields and per-call
// errors instead of losing the whole snapshot to one endpoint.
func (c *Client) Status(ctx context.Context) (*Status, error) {
	if c == nil || c.Account == nil {
		return nil, errors.New("sand: account is required")
	}
	out := &Status{Errors: map[string]string{}}
	if value, err := c.GetUsage(ctx); err != nil {
		out.Errors["usage"] = err.Error()
	} else {
		out.Usage = value
	}
	if value, err := c.GetMe(ctx); err != nil {
		out.Errors["me"] = err.Error()
	} else {
		out.Me = value
	}
	if value, err := c.GetAccessStatus(ctx); err != nil {
		out.Errors["access"] = err.Error()
	} else {
		out.Access = value
	}
	if len(out.Errors) == 0 {
		out.Errors = nil
	}
	return out, nil
}

// Claim follows the SandClaimer decision order while failing closed on
// preflight errors: an unknown identity or allowance must not accidentally
// trigger a personal trial request.
func (c *Client) Claim(ctx context.Context) (ClaimResult, error) {
	if c == nil || c.Account == nil {
		return ClaimResult{Outcome: OutcomeFailed, Detail: "account is required"}, errors.New("sand: account is required")
	}
	me, err := c.GetMe(ctx)
	if err != nil {
		return ClaimResult{Outcome: OutcomeFailed, Detail: "get-me: " + err.Error()}, err
	}
	email := strings.TrimSpace(me.Email)
	if email == "" {
		email = strings.TrimSpace(c.Account.Email)
	}
	resultBase := ClaimResult{Email: email, TeamID: me.TeamID}
	usage, err := c.GetUsage(ctx)
	if err != nil {
		resultBase.Outcome = OutcomeFailed
		resultBase.Detail = "get Sand usage: " + err.Error()
		return resultBase, err
	}
	if usage.Unlocked {
		resultBase.Outcome = OutcomeAlready
		resultBase.Percent = usage.UsagePercent
		resultBase.Detail = "Sand is already enabled"
		return resultBase, nil
	}
	access, err := c.GetAccessStatus(ctx)
	if err != nil {
		resultBase.Outcome = OutcomeFailed
		resultBase.Detail = "get Sand access status: " + err.Error()
		return resultBase, err
	}
	if access.Granted() {
		resultBase.Outcome = OutcomeAlready
		resultBase.Detail = "Sand access is already granted"
		return resultBase, nil
	}
	if strings.TrimSpace(me.TeamID) != "" {
		return c.claimTeam(ctx, resultBase, me.TeamID)
	}
	return c.claimPersonal(ctx, resultBase)
}

func (c *Client) claimTeam(ctx context.Context, base ClaimResult, teamID string) (ClaimResult, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(teamID), 10, 64)
	if err != nil || id <= 0 {
		base.Outcome = OutcomeFailed
		base.Detail = fmt.Sprintf("invalid team id %q", teamID)
		return base, fmt.Errorf("sand: invalid team id %q", teamID)
	}
	body, err := json.Marshal(struct {
		TeamID int64 `json:"teamId"`
	}{TeamID: id})
	if err != nil {
		return base, err
	}
	status, response, err := c.request(ctx, http.MethodPost, pathTeamAccess, body, true)
	if err != nil {
		base.Outcome = OutcomeFailed
		base.Detail = err.Error()
		return base, err
	}
	if responseErr := responseError(status, response); responseErr != nil {
		base.Outcome = OutcomeFailed
		base.HTTPStatus = status
		base.Detail = responseErr.Error()
		return base, nil
	}
	// This endpoint is documented as an idempotent helper. Its failure does
	// not invalidate a successful team access request.
	if onboardStatus, onboardBody, onboardErr := c.request(ctx, http.MethodPost, pathTeamOnboard, body, true); onboardErr != nil {
		base.Detail = "team access requested; onboarding update failed: " + onboardErr.Error()
	} else if onboardErr := responseError(onboardStatus, onboardBody); onboardErr != nil {
		base.Detail = "team access requested; onboarding update failed: " + onboardErr.Error()
	}
	if base.Detail == "" {
		base.Detail = "team Sand access requested"
	}
	base.Outcome = OutcomeTeamOK
	return base, nil
}

func (c *Client) claimPersonal(ctx context.Context, base ClaimResult) (ClaimResult, error) {
	status, body, err := c.request(ctx, http.MethodPost, pathStartTrial, []byte("{}"), true)
	if err != nil {
		base.Outcome = OutcomeFailed
		base.Detail = err.Error()
		return base, err
	}
	base.HTTPStatus = status
	if status < 200 || status >= 300 {
		base.Outcome = OutcomeFailed
		base.Detail = responseDetail(status, body)
		return base, nil
	}
	if isCardVerificationRequired(body) {
		base.Outcome = OutcomeCardRequired
		base.Detail = "personal Sand trial requires card verification"
		base.CardURL = findCheckoutURL(body)
		return base, nil
	}
	base.Outcome = OutcomeActivated
	base.Detail = "personal Sand trial activated"
	return base, nil
}

func (c *Client) request(ctx context.Context, method, path string, body []byte, origin bool) (int, []byte, error) {
	if c == nil || c.Account == nil {
		return 0, nil, errors.New("sand: account is required")
	}
	userID, jwt, err := credential(c.Account)
	if err != nil {
		return 0, nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint := strings.TrimRight(c.BaseURL, "/") + path
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return 0, nil, err
	}
	ua := c.UserAgent
	if ua == "" {
		ua = DefaultUserAgent
	}
	req.Header.Set("Cookie", "WorkosCursorSessionToken="+url.QueryEscape(userID+"::"+jwt))
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", ua)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if origin {
		req.Header.Set("Origin", DefaultBaseURL)
	}
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	return resp.StatusCode, responseBody, nil
}

func credential(account *auth.Account) (userID, jwt string, err error) {
	if account == nil {
		return "", "", errors.New("sand: account is required")
	}
	raw := strings.TrimSpace(account.AccessToken)
	raw = strings.TrimPrefix(raw, "Bearer ")
	if strings.HasPrefix(strings.ToLower(raw), strings.ToLower(auth.WebSessionCookieName)+"=") {
		raw = strings.TrimSpace(raw[len(auth.WebSessionCookieName)+1:])
	}
	if decoded, decodeErr := url.QueryUnescape(raw); decodeErr == nil {
		raw = decoded
	}
	if i := strings.LastIndex(raw, "::"); i > 0 {
		if strings.TrimSpace(account.UserID) == "" {
			account.UserID = strings.TrimSpace(raw[:i])
		}
		raw = strings.TrimSpace(raw[i+2:])
	}
	jwt = strings.TrimSpace(raw)
	userID = strings.TrimSpace(account.UserID)
	if userID == "" {
		claims, decodeErr := auth.DecodeJWTClaims(jwt)
		if decodeErr == nil && claims != nil {
			userID = claims.Sub
			if i := strings.LastIndex(userID, "|"); i >= 0 {
				userID = userID[i+1:]
			}
		}
	}
	if userID == "" || jwt == "" {
		return "", "", errors.New("sand: access token must include a user id and JWT")
	}
	return userID, jwt, nil
}

func usageFromProto(resp *usagepb.GetSandUsageStatusResponse) *Usage {
	if resp == nil {
		return nil
	}
	out := &Usage{
		Unlocked:                !resp.GetIncludedLimitZero() && resp.GetHasNonZeroIncludedLimit(),
		HasAvailableUsage:       resp.GetHasAvailableUsage(),
		UsagePercent:            resp.GetUsagePercent(),
		PlanLabel:               strings.TrimSpace(resp.GetGrokPlanLabel()),
		IncludedLimitZero:       resp.GetIncludedLimitZero(),
		HasNonZeroIncludedLimit: resp.GetHasNonZeroIncludedLimit(),
	}
	out.NextResetAt = timestamp(resp.GetNextResetTimestampUtc())
	out.CurrentPeriodStart = timestamp(resp.GetCurrentPeriodStart())
	out.TrialExpiresAt = timestamp(resp.GetSandTrialExpiresAt())
	return out
}

func timestamp(value *usagepb.SandTimestamp) *time.Time {
	if value == nil || value.GetSeconds() <= 0 {
		return nil
	}
	t := time.Unix(value.GetSeconds(), int64(value.GetNanos())).UTC()
	return &t
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}

func responseError(status int, body []byte) error {
	if status >= 200 && status < 300 {
		return nil
	}
	return &HTTPError{StatusCode: status, Body: responseText(body)}
}

func responseDetail(status int, body []byte) string {
	if text := responseText(body); text != "" {
		return fmt.Sprintf("cursor.com HTTP %d: %s", status, text)
	}
	return fmt.Sprintf("cursor.com HTTP %d", status)
}

func responseText(body []byte) string {
	text := strings.TrimSpace(string(body))
	if len(text) > 320 {
		text = text[:320] + "..."
	}
	return text
}

func rawString(values map[string]json.RawMessage, key string) string {
	var value string
	if raw, ok := values[key]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return strings.TrimSpace(value)
}

func rawID(values map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := values[key]
		if !ok {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) == nil && strings.TrimSpace(text) != "" {
			return strings.TrimSpace(text)
		}
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&number) == nil && number.String() != "" {
			return number.String()
		}
	}
	return ""
}

func isCardVerificationRequired(body []byte) bool {
	text := strings.ToLower(string(body))
	return strings.Contains(text, "cardverificationrequired") ||
		strings.Contains(text, "card_verification") ||
		strings.Contains(text, "card verification required")
}

var checkoutURLRE = regexp.MustCompile(`https://[^\s"<>]*(?:checkout|stripe)[^\s"<>]*`)

func findCheckoutURL(body []byte) string {
	return checkoutURLRE.FindString(string(body))
}
