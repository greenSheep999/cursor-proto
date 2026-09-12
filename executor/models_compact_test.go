package executor

import (
	"encoding/json"
	"strings"
	"testing"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
	"google.golang.org/protobuf/proto"
)

// Cursor 3.16 catalogs advertise one row per primary model with an inline
// `parameter_definitions` list. The IDE's picker collapses thinking / effort /
// fast / max_mode combinations into that row and renders dropdowns from the
// definitions. Our management surface has to do the same, or a picker built on
// top of us would show a flat 600+ row list where the IDE shows ~40 rows.
//
// CompactModelListing is the projection that keeps the management panel
// aligned with the IDE. This test pins the round-trip: one primary row with
// three parameter definitions goes in, we get one CompactModel with the
// dropdown metadata carried through unchanged.
func TestCompactModelListingCollapsesVariantsToParameters(t *testing.T) {
	resp := &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{
			{
				Name:                        "claude-opus-5",
				ClientDisplayName:           strPtr("Claude Opus 5"),
				SupportsThinking:            boolPtr(true),
				SupportsMaxMode:             boolPtr(true),
				SupportsImages:              boolPtr(true),
				SupportsAgent:               boolPtr(true),
				ContextTokenLimit:           int32Ptr(200000),
				ContextTokenLimitForMaxMode: int32Ptr(1000000),
				ParameterDefinitions: []*cursorpb.AiserverV1_ModelParameterDefinition{
					booleanParam("thinking", "Thinking", "true", "false"),
					enumParam("effort", "Effort", []string{"low", "medium", "high", "xhigh", "max"}, "high"),
					booleanParam("fast", "Fast", "true", "false"),
				},
				// Variants populated but MUST NOT surface as separate rows —
				// they exist only so RoutableModelIDs can enumerate wire slugs
				// for the CPA scheduler. The picker view collapses them.
				Variants: []*cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig{
					{VariantStringRepresentation: strPtr("claude-opus-5-high")},
					{VariantStringRepresentation: strPtr("claude-opus-5-thinking-max")},
				},
			},
			{
				Name:              "grok-4.6",
				ClientDisplayName: strPtr("Grok 4.6"),
				ContextTokenLimit: int32Ptr(128000),
				ParameterDefinitions: []*cursorpb.AiserverV1_ModelParameterDefinition{
					enumParam("effort", "Effort", []string{"low", "medium", "high", "xhigh"}, "medium"),
					booleanParam("fast", "Fast", "true", "false"),
				},
			},
		},
	}
	compact := CompactModelListing(resp)
	if len(compact) != 2 {
		t.Fatalf("compact len = %d, want 2 (one row per primary model, variants collapsed)", len(compact))
	}
	if compact[0].ID != "claude-opus-5" || compact[1].ID != "grok-4.6" {
		t.Fatalf("expected sorted [claude-opus-5, grok-4.6], got [%s, %s]", compact[0].ID, compact[1].ID)
	}
	claude := compact[0]
	if claude.DisplayName != "Claude Opus 5" {
		t.Errorf("DisplayName = %q, want %q", claude.DisplayName, "Claude Opus 5")
	}
	if !claude.SupportsThinking || !claude.SupportsMaxMode || !claude.SupportsImages || !claude.SupportsAgent {
		t.Errorf("capability flags dropped: %+v", claude)
	}
	if claude.ContextTokenLimit != 200000 || claude.ContextTokenLimitForMaxMode != 1000000 {
		t.Errorf("context limits dropped: reg=%d max=%d", claude.ContextTokenLimit, claude.ContextTokenLimitForMaxMode)
	}
	if len(claude.Parameters) != 3 {
		t.Fatalf("parameters count = %d, want 3", len(claude.Parameters))
	}
	// Ordering is preserved from the catalog so the picker renders knobs in
	// the same sequence the IDE does.
	gotIDs := []string{claude.Parameters[0].ID, claude.Parameters[1].ID, claude.Parameters[2].ID}
	wantIDs := []string{"thinking", "effort", "fast"}
	for i := range gotIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Errorf("parameter[%d].ID = %q, want %q", i, gotIDs[i], wantIDs[i])
		}
	}
	if claude.Parameters[0].Kind != "boolean" {
		t.Errorf("thinking Kind = %q, want boolean", claude.Parameters[0].Kind)
	}
	if claude.Parameters[1].Kind != "enum" {
		t.Errorf("effort Kind = %q, want enum", claude.Parameters[1].Kind)
	}
	// Enum values must survive intact so a UI can render the exact dropdown
	// Cursor's picker shows.
	effortValues := make([]string, 0, len(claude.Parameters[1].Values))
	for _, v := range claude.Parameters[1].Values {
		effortValues = append(effortValues, v.Value)
	}
	wantEffort := []string{"low", "medium", "high", "xhigh", "max"}
	for i := range wantEffort {
		if i >= len(effortValues) || effortValues[i] != wantEffort[i] {
			t.Errorf("effort values = %v, want %v", effortValues, wantEffort)
			break
		}
	}
}

// TestCompactModelListingEmitsVariantsWithTierAndCostFlags pins the
// downstream billing contract: when the catalog carries variants with
// parameter_definitions that flag certain values as increases_model_cost,
// CompactModelListing must project each variant slug with:
//  1. the parameter values it resolves to (for base+params submission),
//  2. a Tier index counting how many cost-flagged values it uses,
//  3. is_max_mode carried over (max mode also counts one tier).
//
// This is the data an operations layer (New API's ModelRatio table, a
// billing dashboard) consumes to price variants without hand-maintaining a
// per-slug table.
func TestCompactModelListingEmitsVariantsWithTierAndCostFlags(t *testing.T) {
	resp := &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{
			{
				Name: "claude-opus-5",
				ParameterDefinitions: []*cursorpb.AiserverV1_ModelParameterDefinition{
					enumParamWithCosts("effort", "Effort", map[string]bool{
						"low": false, "medium": false, "high": true, "xhigh": true, "max": true,
					}),
					booleanParamWithCosts("thinking", "Thinking", map[string]bool{
						"true": true, "false": false,
					}),
					booleanParamWithCosts("fast", "Fast", map[string]bool{
						"true": true, "false": false,
					}),
				},
				Variants: []*cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig{
					variantConfig("claude-opus-5-low", map[string]string{"effort": "low", "thinking": "false", "fast": "false"}, false, true, false),
					variantConfig("claude-opus-5-high", map[string]string{"effort": "high", "thinking": "false", "fast": "false"}, false, false, false),
					variantConfig("claude-opus-5-thinking-max", map[string]string{"effort": "max", "thinking": "true", "fast": "false"}, false, false, false),
					variantConfig("claude-opus-5-thinking-max-fast", map[string]string{"effort": "max", "thinking": "true", "fast": "true"}, true, false, true),
				},
			},
		},
	}
	compact := CompactModelListing(resp)
	if len(compact) != 1 || compact[0].ID != "claude-opus-5" {
		t.Fatalf("expected one CompactModel for claude-opus-5, got %+v", compact)
	}
	vs := compact[0].Variants
	if len(vs) != 4 {
		t.Fatalf("variants len = %d, want 4", len(vs))
	}
	// Sorted by tier ascending, then by slug.
	// Tier 0: claude-opus-5-low   (no cost flags)
	// Tier 1: claude-opus-5-high  (effort=high)
	// Tier 3: claude-opus-5-thinking-max (thinking=true + effort=max + [max_mode was false here])
	// Tier 4: claude-opus-5-thinking-max-fast (adds fast + is_max_mode=true)
	wantOrder := []struct {
		slug string
		tier int
	}{
		{"claude-opus-5-low", 0},
		{"claude-opus-5-high", 1},
		{"claude-opus-5-thinking-max", 2},
		{"claude-opus-5-thinking-max-fast", 4},
	}
	for i, w := range wantOrder {
		if vs[i].Slug != w.slug || vs[i].Tier != w.tier {
			t.Errorf("variant[%d] = %s tier=%d, want %s tier=%d",
				i, vs[i].Slug, vs[i].Tier, w.slug, w.tier)
		}
	}
	// The parameter_values map on tier-2 variant must faithfully reproduce
	// what a base+params submission needs to reach that variant, so an
	// operator can wire it into New API's routing table directly.
	tier2 := vs[2]
	if tier2.ParameterValues["thinking"] != "true" || tier2.ParameterValues["effort"] != "max" {
		t.Errorf("tier-2 parameter_values = %+v; want thinking=true effort=max", tier2.ParameterValues)
	}
	if !tier2.IsDefault && tier2.IsDefault == vs[0].IsDefault && vs[0].IsDefault {
		t.Errorf("only claude-opus-5-low was flagged default; propagation is wrong")
	}
	// Sanity: default flag survives round-trip.
	if !vs[0].IsDefault {
		t.Errorf("claude-opus-5-low should carry IsDefault=true, got %+v", vs[0])
	}
	// is_max_mode variant surfaces the flag AND adds one to tier.
	last := vs[3]
	if !last.IsMaxMode {
		t.Errorf("claude-opus-5-thinking-max-fast is_max_mode not propagated: %+v", last)
	}
}

// The exploded-variant catalog shape (Cursor still occasionally serves it when
// use_model_parameters is ignored) folds every variant row back to its primary
// id via baseModelID — the compact view must not duplicate. This test guards
// against a regression where multiple exploded rows for the same base slip
// through as duplicate CompactModel entries.
func TestCompactModelListingDeduplicatesExplodedVariants(t *testing.T) {
	resp := &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{
			{Name: "claude-sonnet-4-5", ServerModelName: strPtr("claude-sonnet-4-5")},
			{Name: "claude-sonnet-4-5-thinking-high", ServerModelName: strPtr("claude-sonnet-4-5")},
			{Name: "claude-sonnet-4-5-thinking-medium", ServerModelName: strPtr("claude-sonnet-4-5")},
			{Name: "claude-sonnet-4-5-thinking-low", ServerModelName: strPtr("claude-sonnet-4-5")},
		},
	}
	compact := CompactModelListing(resp)
	if len(compact) != 1 {
		ids := make([]string, 0, len(compact))
		for _, c := range compact {
			ids = append(ids, c.ID)
		}
		t.Fatalf("exploded variants should collapse to 1 row, got %d: %v", len(compact), ids)
	}
	if compact[0].ID != "claude-sonnet-4-5" {
		t.Errorf("compact[0].ID = %q, want %q (base id)", compact[0].ID, "claude-sonnet-4-5")
	}
}

// Live-shape smoke test: a catalog with no parameter_definitions field (an
// older revision, or a model without knobs) still yields a valid CompactModel
// with an empty Parameters slice — the JSON marshalling omits it via omitempty.
func TestCompactModelListingHandlesModelWithoutParameters(t *testing.T) {
	resp := &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{
			{Name: "composer-2.5", ClientDisplayName: strPtr("Composer 2.5")},
		},
	}
	compact := CompactModelListing(resp)
	if len(compact) != 1 || compact[0].ID != "composer-2.5" {
		t.Fatalf("unexpected compact result: %+v", compact)
	}
	if compact[0].Parameters != nil && len(compact[0].Parameters) != 0 {
		t.Errorf("expected no parameters, got %v", compact[0].Parameters)
	}
	// JSON must omit the empty Parameters field so consumers can distinguish
	// "no knobs" from "unknown / parse error".
	buf, _ := json.Marshal(compact[0])
	if strings.Contains(string(buf), `"parameters"`) {
		t.Errorf("expected omitempty to hide empty parameters field, got %s", buf)
	}
}

func TestCompactModelListingNilAndEmpty(t *testing.T) {
	if got := CompactModelListing(nil); got != nil {
		t.Errorf("nil catalog should return nil, got %+v", got)
	}
	if got := CompactModelListing(&cursorpb.AiserverV1_AvailableModelsResponse{}); len(got) != 0 {
		t.Errorf("empty catalog should return empty, got %+v", got)
	}
}

// helper builders for the test tables above. strPtr already lives in
// models_catalog_test.go — reuse it and only add pointer helpers this file
// specifically needs.

func boolPtr(b bool) *bool { return proto.Bool(b) }

func int32Ptr(v int32) *int32 { return proto.Int32(v) }

func booleanParam(id, name string, values ...string) *cursorpb.AiserverV1_ModelParameterDefinition {
	vs := make([]*cursorpb.AiserverV1_ModelParameterDefinition_BooleanParameterDefinition_BooleanParameterValue, 0, len(values))
	for _, v := range values {
		vs = append(vs, &cursorpb.AiserverV1_ModelParameterDefinition_BooleanParameterDefinition_BooleanParameterValue{Value: v})
	}
	return &cursorpb.AiserverV1_ModelParameterDefinition{
		Id:   id,
		Name: name,
		ParameterType: &cursorpb.AiserverV1_ModelParameterDefinition_ModelParameterType{
			BooleanParameter: &cursorpb.AiserverV1_ModelParameterDefinition_BooleanParameterDefinition{
				Values: vs,
			},
		},
	}
}

func enumParam(id, name string, values []string, _ string) *cursorpb.AiserverV1_ModelParameterDefinition {
	vs := make([]*cursorpb.AiserverV1_ModelParameterDefinition_EnumParameterDefinition_EnumParameterValue, 0, len(values))
	for _, v := range values {
		vs = append(vs, &cursorpb.AiserverV1_ModelParameterDefinition_EnumParameterDefinition_EnumParameterValue{Value: v})
	}
	return &cursorpb.AiserverV1_ModelParameterDefinition{
		Id:   id,
		Name: name,
		ParameterType: &cursorpb.AiserverV1_ModelParameterDefinition_ModelParameterType{
			EnumParameter: &cursorpb.AiserverV1_ModelParameterDefinition_EnumParameterDefinition{
				Values: vs,
			},
		},
	}
}

func enumParamWithCosts(id, name string, valueCosts map[string]bool) *cursorpb.AiserverV1_ModelParameterDefinition {
	vs := make([]*cursorpb.AiserverV1_ModelParameterDefinition_EnumParameterDefinition_EnumParameterValue, 0, len(valueCosts))
	for v, cost := range valueCosts {
		e := &cursorpb.AiserverV1_ModelParameterDefinition_EnumParameterDefinition_EnumParameterValue{Value: v}
		if cost {
			e.IncreasesModelCost = boolPtr(true)
		}
		vs = append(vs, e)
	}
	return &cursorpb.AiserverV1_ModelParameterDefinition{
		Id:   id,
		Name: name,
		ParameterType: &cursorpb.AiserverV1_ModelParameterDefinition_ModelParameterType{
			EnumParameter: &cursorpb.AiserverV1_ModelParameterDefinition_EnumParameterDefinition{
				Values: vs,
			},
		},
	}
}

func booleanParamWithCosts(id, name string, valueCosts map[string]bool) *cursorpb.AiserverV1_ModelParameterDefinition {
	vs := make([]*cursorpb.AiserverV1_ModelParameterDefinition_BooleanParameterDefinition_BooleanParameterValue, 0, len(valueCosts))
	for v, cost := range valueCosts {
		e := &cursorpb.AiserverV1_ModelParameterDefinition_BooleanParameterDefinition_BooleanParameterValue{Value: v}
		if cost {
			e.IncreasesModelCost = boolPtr(true)
		}
		vs = append(vs, e)
	}
	return &cursorpb.AiserverV1_ModelParameterDefinition{
		Id:   id,
		Name: name,
		ParameterType: &cursorpb.AiserverV1_ModelParameterDefinition_ModelParameterType{
			BooleanParameter: &cursorpb.AiserverV1_ModelParameterDefinition_BooleanParameterDefinition{
				Values: vs,
			},
		},
	}
}

func variantConfig(slug string, params map[string]string, isMaxMode, isDefault, _ bool) *cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig {
	pvs := make([]*cursorpb.AgentV1_RequestedModel_ModelParameterValue, 0, len(params))
	for k, v := range params {
		pvs = append(pvs, &cursorpb.AgentV1_RequestedModel_ModelParameterValue{Id: k, Value: v})
	}
	v := &cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig{
		LegacySlug:      strPtr(slug),
		ParameterValues: pvs,
		IsMaxMode:       isMaxMode,
	}
	if isDefault {
		v.IsDefaultNonMaxConfig = boolPtr(true)
	}
	return v
}
