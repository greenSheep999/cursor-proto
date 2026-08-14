package executor

import (
	"testing"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

func TestAvailableModelIDsPrefersVariantLegacySlugs(t *testing.T) {
	resp := &cursorpb.AiserverV1_AvailableModelsResponse{
		Models: []*cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel{
			{
				Name: "claude-opus-4-6",
				Variants: []*cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig{
					{LegacySlug: strPtr("claude-4.6-opus-low")},
					{LegacySlug: strPtr("claude-4.6-opus-high")},
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
	want := []string{"claude-4.6-opus-low", "claude-4.6-opus-high", "composer-2.5-fast"}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q (all=%v)", i, got[i], want[i], got)
		}
	}
}

func strPtr(v string) *string { return &v }
