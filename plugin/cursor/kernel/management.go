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

// managementBasePath is the CPA prefix authenticated management API
// routes sit under. Kept in sync with pluginhost/management.go.
const managementBasePath = "/v0/management"

// resourceBasePath is the CPA prefix under which GET routes decorated
// with a Menu field get auto-migrated. CPA's pluginhost treats them
// as browser-navigable, unauthenticated resources — see the
// `routeDeclaresLegacyMenuResource` branch in CPA's management.go.
// Our /accounts and /pool-summary routes carry a Menu, so CPA
// dispatches them here rather than under managementBasePath.
const resourceBasePath = "/v0/resource/plugins/" + pluginName

// legacyPluginPrefix and barePluginPrefix are older CPA path shapes
// still accepted by upstream pluginhost. Matching them keeps this
// router working across CPA versions without extra coordination.
const legacyPluginPrefix = "/plugins/" + pluginName
const barePluginPrefix = "/" + pluginName

// routePrefix is our slice of the management namespace. Every
// management-authenticated route this plugin declares lives under
// it so a single lookup can decide whether to hand the request to us.
const routePrefix = "/cli-proxy-api/cursor"

// pluginMenuLabel appears in the CPA admin panel sidebar under the
// Plugins section and is what the user clicks to open our panel.
// The Menu-tagged route pointing at it is /login (HTML panel), so
// keep this label describing the destination — "Add Cursor account"
// mirrors the sibling kiro-proto plugin ("Add Kiro account").
const pluginMenuLabel = "Add Cursor account"

// managementRegisterResult is the JSON payload returned by
// management.register. It advertises the routes we own so CPA can
// dispatch matching URLs to management.handle.
func managementRegisterResult() string {
	routes := []map[string]any{
		// Menu-tagged GET routes get auto-promoted by CPA's pluginhost
		// to the /v0/resource/plugins/cursor/ namespace and appear in
		// the panel sidebar under the plugin section. /login is the
		// one we want the user to click — it serves the HTML picker.
		{
			"Method":      http.MethodGet,
			"Path":        routePrefix + "/login",
			"Menu":        pluginMenuLabel,
			"Description": "Interactive login for Cursor (OAuth / IDE import / email-OTP). Sub-actions dispatched via POST /login/start and /login/poll.",
		},
		// Data endpoints — no Menu, stay under /v0/management/... and
		// are called by the panel JS with the operator's mgmt session.
		{
			"Method":      http.MethodGet,
			"Path":        routePrefix + "/accounts",
			"Description": "List Cursor accounts CPA has registered, one status object per account. Backs the panel's Registered accounts card.",
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
		{
			"Method":      http.MethodPost,
			"Path":        routePrefix + "/login/start",
			"Description": "Start a login flow — thin panel wrapper over auth.login.start. Body: {Provider, Metadata:{mode,email,...}}.",
		},
		{
			"Method":      http.MethodPost,
			"Path":        routePrefix + "/login/poll",
			"Description": "Poll a running login flow — thin panel wrapper over auth.login.poll. Body: {Provider, State, Metadata}.",
		},
		{
			"Method":      http.MethodGet,
			"Path":        routePrefix + "/quota-rows",
			"Description": "Data endpoint for the /quota page's plugin-quota slot. Returns one row per registered account in the shape declared by the top-level 'quota_slot' manifest field.",
		},
	}
	body := map[string]any{
		"routes":     routes,
		"quota_slot": json.RawMessage(mustMarshalQuotaSlot()),
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
//
// The request Path arrives verbatim from CPA's plugin host, and
// depending on how CPA classified the registered route it may sit
// under one of four namespaces:
//
//	/v0/management/cli-proxy-api/cursor/...   — authenticated mgmt API
//	/v0/resource/plugins/cursor/...           — browser-navigable
//	                                            resource (menu-decorated
//	                                            GET routes auto-migrate
//	                                            here in newer CPA)
//	/plugins/cursor/...                       — legacy prefix
//	/cursor/...                               — bare (older CPA hosts)
//
// We strip whichever prefix arrived, then strip our own routePrefix
// if present, then match the trailing suffix. Same shape as the
// sibling cpa-login-hub plugin so behaviour stays uniform across
// plugins on the same CPA.
func routeManagement(ctx context.Context, req managementRequest) managementResponse {
	suffix := stripCPAPathPrefixes(req.Path)
	// Query values are supplied by the host as map[string][]string; wrap
	// as url.Values for the standard .Get() helper.
	q := url.Values(req.Query)
	method := strings.ToUpper(req.Method)

	switch {
	case method == http.MethodGet && suffix == "/login":
		return serveLoginPage()
	case method == http.MethodPost && suffix == "/login/start":
		return handleLoginStartHTTP(req.Body)
	case method == http.MethodPost && suffix == "/login/poll":
		return handleLoginPollHTTP(req.Body)
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
	case method == http.MethodGet && suffix == "/quota-rows":
		return handleQuotaRows(ctx)
	default:
		return jsonErrorResponse(http.StatusNotFound, "unknown_route",
			fmt.Sprintf("no cursor plugin route for %s %s", method, req.Path))
	}
}

// stripCPAPathPrefixes reduces an incoming request Path to the
// plugin-relative suffix ("/accounts", "/account/events", ...) that
// routeManagement's switch matches on. It tries every namespace
// prefix CPA might send us under, in order of specificity, then
// strips our internal routePrefix if present. Returns an empty
// string for a path that doesn't map into our namespace at all,
// which the caller reports as unknown_route.
func stripCPAPathPrefixes(rawPath string) string {
	p := strings.TrimSuffix(rawPath, "/")
	if p == "" {
		return ""
	}
	// Try each CPA-side namespace in order. Longest-prefix-first so
	// e.g. "/v0/management/..." doesn't get truncated by the bare
	// "/cursor/..." branch. break on first match — a real path only
	// belongs to one namespace at a time.
	for _, prefix := range []string{
		managementBasePath,
		resourceBasePath,
		legacyPluginPrefix,
		barePluginPrefix,
	} {
		if strings.HasPrefix(p, prefix+"/") || p == prefix {
			p = strings.TrimPrefix(p, prefix)
			break
		}
	}
	// After stripping the CPA namespace, our plugin-relative path
	// may still carry the internal routePrefix ("/cli-proxy-api/cursor")
	// — this is the form the management-API routes register with.
	// Resource routes register without it, so trimming is optional.
	if strings.HasPrefix(p, routePrefix+"/") || p == routePrefix {
		p = strings.TrimPrefix(p, routePrefix)
	}
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return p
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
