package executor

import (
	"strings"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// AvailableModelIDs expands Cursor's live model catalog into the concrete
// model identifiers downstream should advertise. We prefer variant legacy
// slugs when present because those are the stable request IDs Cursor accepts
// for a given configuration.
func AvailableModelIDs(resp *cursorpb.AiserverV1_AvailableModelsResponse) []string {
	if resp == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(resp.GetModels()))
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, model := range resp.GetModels() {
		variantCount := 0
		for _, variant := range model.GetVariants() {
			if slug := strings.TrimSpace(variant.GetLegacySlug()); slug != "" {
				add(slug)
				variantCount++
				continue
			}
			if rep := strings.TrimSpace(variant.GetVariantStringRepresentation()); rep != "" {
				add(rep)
				variantCount++
			}
		}
		if variantCount == 0 {
			add(model.GetServerModelName())
			add(model.GetName())
		}
	}
	return out
}
