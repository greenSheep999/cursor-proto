package kernel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/usage"
)

// TestQuotaSlotManifest_ShapeMatchesDesignDoc pins the wire contract
// documented in docs/plugin-quota-slot-design.md. Any change here must
// be intentional — the CPA-frontend renderer reads these keys.
func TestQuotaSlotManifest_ShapeMatchesDesignDoc(t *testing.T) {
	m := quotaSlotManifest()
	requiredTopKeys := []string{
		"id", "title_fallback", "description_fallback",
		"data_path", "columns", "row_actions",
	}
	for _, k := range requiredTopKeys {
		if _, ok := m[k]; !ok {
			t.Errorf("manifest missing required top-level key %q", k)
		}
	}
	if m["id"] != pluginName {
		t.Errorf("manifest id = %q, want %q", m["id"], pluginName)
	}
	dp, _ := m["data_path"].(string)
	if !strings.HasSuffix(dp, "/quota-rows") {
		t.Errorf("data_path = %q, want to end with /quota-rows", dp)
	}
	cols, _ := m["columns"].([]map[string]any)
	if len(cols) < 6 {
		t.Fatalf("columns count = %d, want at least 6 (identifier + core metrics)", len(cols))
	}
	// First column must be an identifier — the panel highlights it as
	// the row's "handle". If we ever reorder columns and lose this,
	// the layout regresses silently.
	if cols[0]["key"] != "email" || cols[0]["type"] != "identifier" {
		t.Errorf("columns[0] = %v, want email/identifier", cols[0])
	}
	// Every column must have key + label + type. A missing field
	// breaks the frontend renderer's dispatch by column type.
	for i, c := range cols {
		for _, k := range []string{"key", "label", "type"} {
			if v, ok := c[k]; !ok || v == "" {
				t.Errorf("columns[%d] missing/empty %q: %v", i, k, c)
			}
		}
	}
	// Column types must be from the v1 registry (design doc § Column
	// type registry). Frontend renders unknown types as `text` with
	// a console warning; keeping the plugin honest saves debugging
	// later.
	allowed := map[string]bool{
		"identifier": true, "tag": true, "int": true, "cents": true,
		"percent": true, "percent_bar": true, "iso8601": true,
		"boolean": true, "enum": true, "text": true,
	}
	for i, c := range cols {
		typ, _ := c["type"].(string)
		if !allowed[typ] {
			t.Errorf("columns[%d] type %q not in v1 registry", i, typ)
		}
	}
}

// TestHandleQuotaSlotManifest_ServesManifestAtEndpoint verifies the
// GET /quota-slot-manifest endpoint returns the same manifest the
// frontend expects. This is the endpoint the CPA-frontend
// PluginQuotaSection probes at page load — a 200 declares the slot
// exists, 404 declares the plugin opts out. Wrong shape here means
// the /quota page silently skips the plugin's card.
func TestHandleQuotaSlotManifest_ServesManifestAtEndpoint(t *testing.T) {
	resp := routeManagement(context.Background(), managementRequest{
		Method: http.MethodGet,
		Path:   managementBasePath + routePrefix + "/quota-slot-manifest",
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200 (body=%s)", resp.StatusCode, string(resp.Body))
	}
	var manifest map[string]any
	if err := json.Unmarshal(resp.Body, &manifest); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if manifest["id"] != pluginName {
		t.Errorf("manifest.id = %v, want %s", manifest["id"], pluginName)
	}
	if _, ok := manifest["columns"]; !ok {
		t.Error("manifest missing columns")
	}
}

// TestManifestEmbedsInRegisterResult confirms the quota_slot blob
// actually reaches CPA — a broken embed would leave CPA rendering
// nothing on /quota and silently blame the plugin.
func TestManifestEmbedsInRegisterResult(t *testing.T) {
	raw := managementRegisterResult()
	var body map[string]any
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode register result: %v", err)
	}
	slot, ok := body["quota_slot"].(map[string]any)
	if !ok {
		t.Fatalf("register result missing quota_slot: keys=%v", keysOf(body))
	}
	if slot["id"] != pluginName {
		t.Errorf("quota_slot.id = %v, want %s", slot["id"], pluginName)
	}
}

// TestHandleQuotaRows_ProjectsSnapshotFields is the end-to-end row
// projection: build a fixture registry, hit the /quota-rows router
// entry, and assert every wire column has a value coming from the
// underlying AccountStatus.
func TestHandleQuotaRows_ProjectsSnapshotFields(t *testing.T) {
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		snap := fakeSnapshot()
		snap.Email = a.Email
		return snap, nil
	}
	withFixture(t, fetch, func() {
		resp := routeManagement(context.Background(), managementRequest{
			Method: http.MethodGet,
			Path:   managementBasePath + routePrefix + "/quota-rows",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("StatusCode = %d, want 200 (body=%s)", resp.StatusCode, string(resp.Body))
		}
		var body struct {
			Rows      []quotaRow `json:"rows"`
			Count     int        `json:"count"`
			FetchedAt time.Time  `json:"fetched_at"`
		}
		if err := json.Unmarshal(resp.Body, &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if body.Count == 0 {
			t.Fatal("expected at least one row from the fixture registry")
		}
		if body.FetchedAt.IsZero() {
			t.Error("fetched_at is zero — frontend uses it as a cache-buster")
		}
		row := body.Rows[0]
		if row.Email == "" {
			t.Errorf("row.email empty; row=%+v", row)
		}
		if row.ID != row.Email {
			t.Errorf("row.id = %q, expected to match email %q", row.ID, row.Email)
		}
		// The fake snapshot populates SpendCents + LimitCents +
		// TotalPercentUsed — assert the projection actually copied
		// them. A regression that maps to the wrong field silently
		// breaks the panel's progress bars.
		if row.SpendCents == 0 || row.LimitCents == 0 {
			t.Errorf("spend/limit not projected: %+v", row)
		}
	})
}

// TestHandleQuotaRows_MenuInResourceNamespace confirms the CPA
// resource-namespace path still routes to the handler. This is the
// same regression class the /accounts route fell into before commit
// 3a8e773 — proving out the "quota-rows works under both prefixes"
// invariant explicitly.
func TestHandleQuotaRows_MenuInResourceNamespace(t *testing.T) {
	fetch := func(ctx context.Context, a *auth.Account) (*usage.Snapshot, error) {
		return fakeSnapshot(), nil
	}
	withFixture(t, fetch, func() {
		// /quota-rows has no Menu tag, so it stays under management.
		// But CPA callers can still address it via the plugin-hosted
		// namespaces; check that stripCPAPathPrefixes handles it.
		for _, p := range []string{
			managementBasePath + routePrefix + "/quota-rows",
			"/plugins/cursor/cli-proxy-api/cursor/quota-rows",
			"/cursor/quota-rows",
		} {
			resp := routeManagement(context.Background(), managementRequest{
				Method: http.MethodGet,
				Path:   p,
			})
			if resp.StatusCode != http.StatusOK {
				t.Errorf("path %q → %d, want 200 (body=%s)",
					p, resp.StatusCode, string(resp.Body))
			}
		}
	})
}

// TestRowFromStatus_MarksBareErrorRows locks in the "one broken
// account doesn't nuke the card" behaviour. A status with no plan
// and no spend but a LastErrorCode surfaces as _status=error so
// the frontend can dim the row; a status with partial data (plan
// present, upstream error since) surfaces as _status=ok because
// stale data is still useful to look at.
func TestRowFromStatus_MarksBareErrorRows(t *testing.T) {
	bare := &AccountStatus{
		Email:         "bare@example.com",
		LastErrorCode: "fetch_snapshot",
	}
	if got := rowFromStatus(bare); got.Status != "error" {
		t.Errorf("bare error row status = %q, want error", got.Status)
	}
	stale := &AccountStatus{
		Email:         "stale@example.com",
		Plan:          "Pro",
		SpendCents:    500,
		LastErrorCode: "transient_5xx",
	}
	if got := rowFromStatus(stale); got.Status == "error" {
		t.Errorf("stale-but-usable row must NOT be error, got status=%q", got.Status)
	}
}

// keysOf lifts map keys for a friendlier failure message.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
