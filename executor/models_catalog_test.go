package executor

import (
	"testing"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

func TestAvailableModelIDsExposePrimaryCatalogNames(t *testing.T) {
	resp := &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{
			{
				Name: "claude-opus-4-8",
				Variants: []*cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig{
					{LegacySlug: strPtr("claude-opus-4-8-medium")},
					{LegacySlug: strPtr("claude-opus-4-8-thinking-high")},
				},
			},
			{
				Name: "composer-2.5",
				Variants: []*cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig{
					{LegacySlug: strPtr("composer-2.5-fast")},
				},
			},
		},
	}

	got := AvailableModelIDs(resp)
	want := []string{"claude-opus-4-8", "composer-2.5"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func TestResolveRequestedModelUsesDefaultVariantForPrimaryName(t *testing.T) {
	resp := modelCatalogFixture()

	got, ok := resolveRequestedModelFromCatalog(resp, "claude-opus-4-8")
	if !ok {
		t.Fatal("expected primary model to resolve")
	}
	if got.GetModelId() != "claude-opus-4-8" {
		t.Fatalf("model id = %q", got.GetModelId())
	}
	if !got.GetBuiltInModel() {
		t.Fatal("built_in_model = false, want true for catalog models")
	}
	if got.GetMaxMode() {
		t.Fatal("default non-max variant unexpectedly enabled max mode")
	}
	assertRequestedParameters(t, got, map[string]string{
		"thinking": "true",
		"context":  "300k",
		"effort":   "high",
		"fast":     "false",
	})
}

func TestResolveRequestedModelKeepsLegacyVariantCompatibility(t *testing.T) {
	resp := modelCatalogFixture()

	got, ok := resolveRequestedModelFromCatalog(resp, "claude-opus-4-8-medium")
	if !ok {
		t.Fatal("expected legacy variant to resolve")
	}
	if got.GetModelId() != "claude-opus-4-8" {
		t.Fatalf("model id = %q", got.GetModelId())
	}
	assertRequestedParameters(t, got, map[string]string{
		"thinking": "false",
		"context":  "300k",
		"effort":   "medium",
		"fast":     "false",
	})
}

func TestResolveRequestedModelMatchesLiveVariantParameters(t *testing.T) {
	got, ok := resolveRequestedModelFromCatalogWithParameters(modelCatalogFixture(), "claude-opus-4-8", map[string]string{
		"thinking": "false",
		"effort":   "medium",
	})
	if !ok {
		t.Fatal("expected primary model to resolve")
	}
	assertRequestedParameters(t, got, map[string]string{
		"thinking": "false",
		"context":  "300k",
		"effort":   "medium",
		"fast":     "false",
	})
}

func TestResolveRequestedModelExplicitVariantWinsOverParameters(t *testing.T) {
	got, ok := resolveRequestedModelFromCatalogWithParameters(modelCatalogFixture(), "claude-opus-4-8-medium", map[string]string{
		"thinking": "true",
		"effort":   "high",
	})
	if !ok {
		t.Fatal("expected explicit variant to resolve")
	}
	assertRequestedParameters(t, got, map[string]string{
		"thinking": "false",
		"context":  "300k",
		"effort":   "medium",
		"fast":     "false",
	})
}

func modelCatalogFixture() *cursorpb.AiserverV1_AvailableModelsResponse {
	defaultVariant := true
	return &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{{
			Name:            "claude-opus-4-8",
			ServerModelName: strPtr("claude-opus-4-8"),
			IdAliases:       []string{"opus-4.8"},
			Variants: []*cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig{
				{
					LegacySlug: strPtr("claude-opus-4-8-medium"),
					ParameterValues: requestedParameters(map[string]string{
						"thinking": "false",
						"context":  "300k",
						"effort":   "medium",
						"fast":     "false",
					}),
				},
				{
					LegacySlug:            strPtr("claude-opus-4-8-thinking-high"),
					IsDefaultNonMaxConfig: &defaultVariant,
					ParameterValues: requestedParameters(map[string]string{
						"thinking": "true",
						"context":  "300k",
						"effort":   "high",
						"fast":     "false",
					}),
				},
			},
		}},
	}
}

func requestedParameters(values map[string]string) []*cursorpb.AgentV1_RequestedModel_ModelParameterValue {
	out := make([]*cursorpb.AgentV1_RequestedModel_ModelParameterValue, 0, len(values))
	for id, value := range values {
		out = append(out, &cursorpb.AgentV1_RequestedModel_ModelParameterValue{Id: id, Value: value})
	}
	return out
}

func assertRequestedParameters(t *testing.T, got *cursorpb.AgentV1_RequestedModel, want map[string]string) {
	t.Helper()
	actual := make(map[string]string, len(got.GetParameters()))
	for _, parameter := range got.GetParameters() {
		actual[parameter.GetId()] = parameter.GetValue()
	}
	if len(actual) != len(want) {
		t.Fatalf("parameters = %v, want %v", actual, want)
	}
	for id, value := range want {
		if actual[id] != value {
			t.Fatalf("parameter %q = %q, want %q (all=%v)", id, actual[id], value, actual)
		}
	}
}

func strPtr(v string) *string { return &v }
