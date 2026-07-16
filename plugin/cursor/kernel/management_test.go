package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/usage"
)

// TestStripCPAPathPrefixes locks in the four namespaces CPA can send
// requests under. The resource-path row is the critical one — CPA
// auto-migrates menu-decorated GET routes to /v0/resource/plugins/cursor/,
// and before we handled it here the browser saw
//   {"error":{"code":"unknown_route","message":"no cursor plugin route
//   for GET /v0/managementv0/resource/plugins/cursor/cli-proxy-api/cursor/accounts"}}
// (yes, two v0's mashed together) on #/plugin-pages/cursor/0.
func TestStripCPAPathPrefixes(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "management namespace with routePrefix",
			in:   "/v0/management/cli-proxy-api/cursor/accounts",
			want: "/accounts",
		},
		{
			name: "resource namespace (menu-decorated route)",
			in:   "/v0/resource/plugins/cursor/cli-proxy-api/cursor/accounts",
			want: "/accounts",
		},
		{
			name: "resource namespace without routePrefix",
			// Some CPA versions may register resources without the
			// plugin's routePrefix. Handle both.
			in:   "/v0/resource/plugins/cursor/accounts",
			want: "/accounts",
		},
		{
			name: "legacy /plugins/<name> prefix",
			in:   "/plugins/cursor/cli-proxy-api/cursor/account/events",
			want: "/account/events",
		},
		{
			name: "bare /<name> prefix (older CPA hosts)",
			in:   "/cursor/pool-summary",
			want: "/pool-summary",
		},
		{
			name: "trailing slash tolerated",
			in:   "/v0/management/cli-proxy-api/cursor/accounts/",
			want: "/accounts",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripCPAPathPrefixes(tc.in)
			if got != tc.want {
				t.Fatalf("stripCPAPathPrefixes(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRouteManagement_ResourcePath_ListAccounts is the end-to-end
// regression for the browser panel bug. Reproduces the exact Path
// CPA sends when the panel opens #/plugin-pages/cursor/0 and asserts
// the router now returns the account list instead of 404.
func TestRouteManagement_ResourcePath_ListAccounts(t *testing.T) {
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		snap := fakeSnapshot()
		snap.Email = a.Email
		return snap, nil
	}
	withFixture(t, fetch, func() {
		resp := routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   "/v0/resource/plugins/cursor/cli-proxy-api/cursor/accounts",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("StatusCode = %d, want 200 (body=%s)",
				resp.StatusCode, string(resp.Body))
		}
		var body struct {
			Accounts []*AccountStatus `json:"accounts"`
			Count    int              `json:"count"`
		}
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body.Count == 0 {
			t.Fatal("expected non-zero account count from resource-path fetch")
		}
	})
}

// TestRouteManagement_UnknownRoutePreservesRawPath makes sure the 404
// error message keeps the raw Path CPA sent, not our post-strip
// suffix — otherwise diagnosing "why did I get 404" requires reading
// server logs to reconstruct the original URL.
func TestRouteManagement_UnknownRoutePreservesRawPath(t *testing.T) {
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		return fakeSnapshot(), nil
	}
	withFixture(t, fetch, func() {
		raw := "/v0/resource/plugins/cursor/nonsense/endpoint"
		resp := routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   raw,
		})
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("StatusCode = %d, want 404", resp.StatusCode)
		}
		if got := string(resp.Body); !containsSubstring(got, raw) {
			t.Fatalf("error body %q does not contain raw path %q", got, raw)
		}
	})
}

// containsSubstring avoids pulling in strings just for one call site.
func containsSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
