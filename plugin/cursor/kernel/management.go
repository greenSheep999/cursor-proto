// management.register / management.handle for the Cursor plugin.
//
// This file wires the admin-panel routes that expose pool status to
// CPA. It does NOT touch executor.execute* — those live in main.go
// (guarded by a sibling worktree). All the state used here lives in
// status.go (authRegistry + AccountStatus).
package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// managementBasePath is the CPA prefix under which every plugin
// management route sits. Kept in sync with pluginhost/management.go.
const managementBasePath = "/v0/management"

// routePrefix is our slice of the management namespace. Every route
// this plugin declares lives under it so a single lookup can decide
// whether to hand the request to us.
const routePrefix = "/cli-proxy-api/cursor"

// pluginMenuLabel appears in the CPA admin panel sidebar.
const pluginMenuLabel = "Cursor accounts"

// managementRegisterResult is the JSON payload returned by
// management.register. It advertises the routes we own so CPA can
// dispatch matching URLs to management.handle.
func managementRegisterResult() string {
	menu := pluginMenuLabel
	routes := []map[string]any{
		{
			"Method":      http.MethodGet,
			"Path":        routePrefix + "/accounts",
			"Menu":        menu,
			"Description": "List Cursor accounts CPA has registered, one status object per account.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        routePrefix + "/account",
			"Description": "Return one account's rich status (?email=...).",
		},
		{
			"Method":      http.MethodPost,
			"Path":        routePrefix + "/account/refresh",
			"Description": "Force a JWT refresh for a specific account (?email=...).",
		},
		{
			"Method":      http.MethodPost,
			"Path":        routePrefix + "/account/probe",
			"Description": "Bust the AccountStatus cache and re-fetch from Cursor (?email=...).",
		},
		{
			"Method": http.MethodGet,
			"Path":   routePrefix + "/account/events",
			"Description": "Paginated per-request event log for one account " +
				"(?email=&since=&page=&limit=&model=). Backs the per-request " +
				"table in the admin panel — same data as cursor-proxy's " +
				"/v1/usage/events endpoint (introduced in cursor3.11/v0.3.1).",
		},
		{
			"Method":      http.MethodGet,
			"Path":        routePrefix + "/pool-summary",
			"Description": "Aggregate view of the Cursor pool for the admin dashboard.",
		},
	}
	body := map[string]any{
		"routes": routes,
	}
	buf, _ := json.Marshal(body)
	return string(buf)
}

// managementRequest mirrors pluginapi.ManagementRequest on the wire.
type managementRequest struct {
	Method         string              `json:"Method"`
	Path           string              `json:"Path"`
	Headers        map[string][]string `json:"Headers,omitempty"`
	Query          map[string][]string `json:"Query,omitempty"`
	Body           []byte              `json:"Body,omitempty"`
	HostCallbackID string              `json:"host_callback_id,omitempty"`
}

// managementResponse mirrors pluginapi.ManagementResponse.
type managementResponse struct {
	StatusCode int                 `json:"StatusCode"`
	Headers    map[string][]string `json:"Headers,omitempty"`
	Body       []byte              `json:"Body,omitempty"`
}

// handleManagement is the entry point for the management.handle ABI
// method. It parses the request envelope, routes to the correct
// endpoint, and marshals a plugin-shaped response.
func handleManagement(payload []byte) ([]byte, int) {
	var req managementRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse management request: %v", err), false), 1
	}
	// Ensure the auth registry is seeded from disk on the first hit.
	if _, err := globalRegistry.LoadFromDisk(); err != nil {
		// Not fatal — accounts registered via auth.parse are still available.
		_ = err
	}
	resp := routeManagement(context.Background(), req)
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// routeManagement decides which handler answers the request. Paths
// are matched exactly (the pluginhost route table already narrowed
// URL to exactly the paths we declared).
func routeManagement(ctx context.Context, req managementRequest) managementResponse {
	path := strings.TrimSuffix(req.Path, "/")
	// The pluginhost rewrites relative paths, but we normalise defensively.
	if !strings.HasPrefix(path, managementBasePath) {
		path = managementBasePath + strings.TrimPrefix(path, "/")
	}
	suffix := strings.TrimPrefix(path, managementBasePath)
	if strings.HasPrefix(suffix, routePrefix) {
		suffix = strings.TrimPrefix(suffix, routePrefix)
	}
	// Query values are supplied by the host as map[string][]string; wrap
	// as url.Values for the standard .Get() helper.
	q := url.Values(req.Query)
	method := strings.ToUpper(req.Method)

	switch {
	case method == http.MethodGet && suffix == "/accounts":
		return handleListAccounts(ctx)
	case method == http.MethodGet && suffix == "/account":
		return handleAccountDetail(ctx, q.Get("email"))
	case method == http.MethodPost && suffix == "/account/refresh":
		return handleAccountRefresh(ctx, q.Get("email"), req.Body)
	case method == http.MethodPost && suffix == "/account/probe":
		return handleAccountProbe(ctx, q.Get("email"))
	case method == http.MethodGet && suffix == "/account/events":
		return handleAccountEvents(ctx, q)
	case method == http.MethodGet && suffix == "/pool-summary":
		return handlePoolSummary(ctx)
	default:
		return jsonErrorResponse(http.StatusNotFound, "unknown_route",
			fmt.Sprintf("no cursor plugin route for %s %s", method, path))
	}
}

// handleListAccounts returns the full AccountStatus for every
// registered account, honouring the 30s cache.
func handleListAccounts(ctx context.Context) managementResponse {
	accs := globalRegistry.List()
	statuses := make([]*AccountStatus, 0, len(accs))
	for _, a := range accs {
		s, err := globalRegistry.Status(ctx, a.Email, false)
		if err != nil && s == nil {
			s = &AccountStatus{Email: a.Email, LastErrorCode: sanitiseErrCode(err)}
		}
		statuses = append(statuses, s)
	}
	body := map[string]any{
		"accounts":   statuses,
		"count":      len(statuses),
		"fetched_at": time.Now().UTC(),
	}
	return jsonResponse(http.StatusOK, body)
}

// handleAccountDetail returns one account's status.
func handleAccountDetail(ctx context.Context, email string) managementResponse {
	email = strings.TrimSpace(email)
	if email == "" {
		return jsonErrorResponse(http.StatusBadRequest, "missing_email", "?email= is required")
	}
	if _, ok := globalRegistry.Get(email); !ok {
		return jsonErrorResponse(http.StatusNotFound, "unknown_account",
			fmt.Sprintf("no cursor account tracked for %s", email))
	}
	s, err := globalRegistry.Status(ctx, email, false)
	if err != nil && s == nil {
		return jsonErrorResponse(http.StatusBadGateway, "fetch_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, s)
}

// handleAccountRefresh forces a JWT refresh by rerunning handleAuthRefresh
// against the stored auth material. The response mirrors what
// auth.refresh would produce so operators can inspect the new expiry.
func handleAccountRefresh(ctx context.Context, email string, _ []byte) managementResponse {
	_ = ctx
	email = strings.TrimSpace(email)
	if email == "" {
		return jsonErrorResponse(http.StatusBadRequest, "missing_email", "?email= is required")
	}
	acc, ok := globalRegistry.Get(email)
	if !ok {
		return jsonErrorResponse(http.StatusNotFound, "unknown_account",
			fmt.Sprintf("no cursor account tracked for %s", email))
	}
	// Rebuild the CPA auth file shape and route through handleAuthRefresh
	// so the passthrough behaviour matches auth.refresh exactly. When the
	// real refresh path lands the same logic will kick in here.
	file := &cpaformat.AuthFile{
		CursorTokenStorage: cpaformat.CursorTokenStorage{
			Type:         cpaformat.ProviderType,
			AccessToken:  acc.AccessToken,
			RefreshToken: acc.RefreshToken,
			Email:        acc.Email,
			UserID:       acc.UserID,
			AuthID:       acc.AuthID,
			AuthKind:     acc.AuthType,
			MachineID:    acc.MachineID,
			MacMachineID: acc.MacMachineID,
			IssuedAt:     cpaformat.FormatTime(acc.IssuedAt),
			LastRefresh:  cpaformat.FormatTime(time.Now()),
			Expired:      cpaformat.FormatTime(acc.ExpiresAt),
		},
	}
	storage, err := json.Marshal(file)
	if err != nil {
		return jsonErrorResponse(http.StatusInternalServerError, "marshal_storage", err.Error())
	}
	refreshReq := authRefreshRequest{
		AuthID:       email,
		AuthProvider: pluginName,
		StorageJSON:  storage,
	}
	body, err := json.Marshal(refreshReq)
	if err != nil {
		return jsonErrorResponse(http.StatusInternalServerError, "marshal_refresh_request", err.Error())
	}
	envRaw, rc := handleAuthRefresh(body)
	if rc != 0 {
		return jsonResponse(http.StatusBadGateway, envelopeToMap(envRaw))
	}
	globalRegistry.Invalidate(email)
	if exp, ok := decodeJWTExpiry(acc.AccessToken); ok {
		return jsonResponse(http.StatusOK, map[string]any{
			"email":            email,
			"jwt_expires_at":   exp,
			"jwt_expires_in":   time.Until(exp).String(),
			"refresh_response": envelopeToMap(envRaw),
		})
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"email":            email,
		"refresh_response": envelopeToMap(envRaw),
	})
}

// handleAccountProbe invalidates the cache and returns a freshly
// fetched AccountStatus.
func handleAccountProbe(ctx context.Context, email string) managementResponse {
	email = strings.TrimSpace(email)
	if email == "" {
		return jsonErrorResponse(http.StatusBadRequest, "missing_email", "?email= is required")
	}
	if _, ok := globalRegistry.Get(email); !ok {
		return jsonErrorResponse(http.StatusNotFound, "unknown_account",
			fmt.Sprintf("no cursor account tracked for %s", email))
	}
	globalRegistry.Invalidate(email)
	s, err := globalRegistry.Status(ctx, email, true)
	if err != nil && s == nil {
		return jsonErrorResponse(http.StatusBadGateway, "probe_failed", err.Error())
	}
	return jsonResponse(http.StatusOK, s)
}

// handlePoolSummary aggregates the tracked accounts into a single view.
func handlePoolSummary(ctx context.Context) managementResponse {
	accs := globalRegistry.List()
	total := len(accs)
	active := 0
	slowPool := 0
	expiringSoon := 0
	countryBreakdown := map[string]int{}
	planBreakdown := map[string]int{}
	now := time.Now()
	statuses := make([]*AccountStatus, 0, total)
	for _, a := range accs {
		s, _ := globalRegistry.Status(ctx, a.Email, false)
		if s == nil {
			s = &AccountStatus{Email: a.Email}
		}
		statuses = append(statuses, s)
		if s.InSlowPool {
			slowPool++
		} else {
			active++
		}
		if !s.JwtExpiresAt.IsZero() && s.JwtExpiresAt.Sub(now) < 24*time.Hour {
			expiringSoon++
		}
		if s.Country != "" {
			countryBreakdown[strings.ToUpper(s.Country)]++
		}
		if s.Plan != "" {
			planBreakdown[s.Plan]++
		}
	}
	body := map[string]any{
		"total":             total,
		"active":            active,
		"slow_pool":         slowPool,
		"expiring_soon":     expiringSoon,
		"country_breakdown": countryBreakdown,
		"plan_breakdown":    planBreakdown,
		"generated_at":      now.UTC(),
	}
	return jsonResponse(http.StatusOK, body)
}

// jsonResponse marshals v into a managementResponse with content-type
// application/json.
func jsonResponse(status int, v any) managementResponse {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return jsonErrorResponse(http.StatusInternalServerError, "marshal_body", err.Error())
	}
	return managementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type": {"application/json; charset=utf-8"},
		},
		Body: buf,
	}
}

// jsonErrorResponse renders {code,message} with the given status.
func jsonErrorResponse(status int, code, message string) managementResponse {
	buf, _ := json.MarshalIndent(map[string]any{
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	}, "", "  ")
	return managementResponse{
		StatusCode: status,
		Headers: map[string][]string{
			"Content-Type": {"application/json; charset=utf-8"},
		},
		Body: buf,
	}
}

// envelopeToMap decodes an ABI envelope to a map for embedding in
// another JSON body (used by the /refresh endpoint).
func envelopeToMap(env []byte) map[string]any {
	var out map[string]any
	if err := json.Unmarshal(env, &out); err != nil {
		return map[string]any{"raw": string(env)}
	}
	return out
}

// sanitiseErrCode strips whitespace and turns the error into a short
// error-code style string. Falls back to "error" when empty.
func sanitiseErrCode(err error) string {
	if err == nil {
		return ""
	}
	s := strings.TrimSpace(err.Error())
	if s == "" {
		return "error"
	}
	if i := strings.Index(s, ":"); i > 0 {
		s = s[:i]
	}
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "_")
	return s
}

// registerAccountFromParse hooks into handleAuthParse to teach the
// registry about accounts as they flow through the plugin. Called
// from main.go's dispatch after a successful parse.
func registerAccountFromParse(rawJSON []byte) {
	file, err := cpaformat.Unmarshal(rawJSON)
	if err != nil || file.Type != cpaformat.ProviderType {
		return
	}
	acc := accountFromAuthFile(file)
	if acc != nil {
		globalRegistry.Register(acc)
	}
}

// summariseAccounts is a helper for the cursor-pool-summary CLI. It
// takes an already-fetched pool-summary JSON response and formats it
// as an ordered table for terminal output.
func summariseAccounts(raw []byte) (string, error) {
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", fmt.Errorf("decode summary: %w", err)
	}
	countries, _ := body["country_breakdown"].(map[string]any)
	plans, _ := body["plan_breakdown"].(map[string]any)
	kv := func(m map[string]any) []string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make([]string, 0, len(keys))
		for _, k := range keys {
			out = append(out, fmt.Sprintf("%s=%v", k, m[k]))
		}
		return out
	}
	var b strings.Builder
	fmt.Fprintf(&b, "  %-22s %v\n", "total", body["total"])
	fmt.Fprintf(&b, "  %-22s %v\n", "active", body["active"])
	fmt.Fprintf(&b, "  %-22s %v\n", "slow_pool", body["slow_pool"])
	fmt.Fprintf(&b, "  %-22s %v\n", "expiring_soon", body["expiring_soon"])
	fmt.Fprintf(&b, "  %-22s %s\n", "country_breakdown", strings.Join(kv(countries), " "))
	fmt.Fprintf(&b, "  %-22s %s\n", "plan_breakdown", strings.Join(kv(plans), " "))
	if ts, ok := body["generated_at"].(string); ok {
		fmt.Fprintf(&b, "  %-22s %s\n", "generated_at", ts)
	}
	return b.String(), nil
}
