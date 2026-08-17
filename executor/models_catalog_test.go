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

// TestAvailableModelIDsFoldsExplodedTeamCatalog reproduces the failure mode
// codex hit against a legacy team catalog: instead of returning one row per
// base model with parameterised variants, the server emits one row per
// (base, effort, thinking, context) combination — the "207 exploded
// variants" shape referenced in docs/versioning.md. Without fold-back these
// variant slugs get registered as separate model ids in CPA and subsequent
// chat requests targeting the base id fail with "unknown provider". The
// resolver already knows how to map a variant slug back to its base for
// dispatch; AvailableModelIDs now advertises just the bases so the two
// halves stay in sync.
//
// The fold-back is scoped: catalog rows whose Name is already the base id
// (i.e., the modern parameterised shape) pass through unchanged so the
// marketing aliases like "gpt" / "gemini" that Cursor also ships in
// id_aliases don't collapse distinct base models into one.
func TestAvailableModelIDsFoldsExplodedTeamCatalog(t *testing.T) {
	base := "claude-sonnet-4-5"
	resp := &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{
			// Modern parameterised row — must pass through untouched even
			// when id_aliases contains marketing shortcuts.
			{
				Name:            "gpt-5.6-sol",
				ServerModelName: strPtr("gpt-5.6-sol"),
				IdAliases:       []string{"gpt-latest", "gpt", "gpt-5.6"},
				LegacySlugs:     []string{"gpt-5.6-sol-low", "gpt-5.6-sol-high"},
			},
			// Exploded rows — server repeats the base as server_model_name
			// and lists the variant slug in Name.
			{
				Name:            base + "-thinking-high",
				ServerModelName: strPtr(base),
				IdAliases:       []string{base, base + "-thinking-high"},
				LegacySlugs:     []string{base + "-thinking-high"},
			},
			{
				Name:            base + "-thinking-max",
				ServerModelName: strPtr(base),
				IdAliases:       []string{base},
				LegacySlugs:     []string{base + "-thinking-max"},
			},
			// Second base — verify dedupe across primary ids.
			{
				Name:            "claude-opus-5-thinking-medium",
				ServerModelName: strPtr("claude-opus-5"),
				IdAliases:       []string{"claude-opus-5"},
			},
			// Row with no alias / server_model_name but with a legacy_slug
			// that repeats Name — the shape used by the very oldest
			// exploded catalogs.
			{
				Name:        "claude-fable-5-fast",
				LegacySlugs: []string{"claude-fable-5-fast"},
			},
		},
	}

	got := AvailableModelIDs(resp)
	want := []string{"gpt-5.6-sol", "claude-sonnet-4-5", "claude-opus-5", "claude-fable-5"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

// TestAvailableModelIDsPreservesModernBaseNames pins the "do not over-fold"
// contract. Cursor 3.16 accounts return the parameterised shape where each
// row's Name is already the base id and id_aliases carries short marketing
// abbreviations (`gpt`, `gemini`, `codex`) plus the "-latest" pointer.
// AvailableModelIDs must return each base name as-is; the fold-back path
// is opt-in for rows that look like exploded variants (Name matches an
// entry in its own LegacySlugs, or ends with a known variant suffix and
// has a shorter sibling in the alias/slug lists).
func TestAvailableModelIDsPreservesModernBaseNames(t *testing.T) {
	resp := &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{
			{
				Name:            "auto-smart",
				ServerModelName: strPtr("auto-smart"),
				LegacySlugs:     []string{"default"},
			},
			{
				Name:            "gpt-5.6-sol",
				ServerModelName: strPtr("gpt-5.6-sol"),
				IdAliases:       []string{"gpt-latest", "gpt", "gpt-5.6"},
				LegacySlugs:     []string{"gpt-5.6-sol-low", "gpt-5.6-sol-high"},
			},
			{
				Name:            "gemini-3.1-pro",
				ServerModelName: strPtr("gemini-3.1-pro"),
				IdAliases:       []string{"gemini-latest", "gemini-pro-latest", "gemini", "gemini-pro"},
			},
		},
	}
	got := AvailableModelIDs(resp)
	want := []string{"auto-smart", "gpt-5.6-sol", "gemini-3.1-pro"}
	if len(got) != len(want) {
		t.Fatalf("len(got)=%d want %d (got=%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

// TestStripVariantSuffixesHandlesCompoundVariants pins the fallback code
// path used when a catalog row is missing both server_model_name and any
// id_alias — this happens on old team catalogs that pre-date those
// fields, and is the exact shape the resolver bug report cited.
func TestStripVariantSuffixesHandlesCompoundVariants(t *testing.T) {
	cases := map[string]string{
		"claude-opus-5-thinking-high":     "claude-opus-5",
		"claude-sonnet-4-5-thinking-max":  "claude-sonnet-4-5",
		"claude-sonnet-4-5-fast":          "claude-sonnet-4-5",
		"claude-sonnet-4-5-thinking-fast": "claude-sonnet-4-5",
		"composer-2.5":                    "composer-2.5",
		"":                                "",
	}
	for input, want := range cases {
		if got := stripVariantSuffixes(input); got != want {
			t.Errorf("stripVariantSuffixes(%q) = %q, want %q", input, got, want)
		}
	}
}
