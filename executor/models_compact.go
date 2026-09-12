package executor

// Cursor 3.16 shifted model discovery to a base+parameters shape: the picker
// UI collapses every effort/thinking/fast combination into a single row and
// renders a dropdown for the parameters instead of a flat 600-entry list.
// AvailableModelIDs already returns the 38 primary ids; this file adds the
// parameter metadata that goes with them so a caller (management panel, CLI
// tools) can present the same UX the IDE does.

import (
	"sort"
	"strings"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// CompactModel is a picker-friendly view of one primary model: the base id
// plus the parameters the user can toggle in the IDE dropdown. It intentionally
// omits per-variant slugs — those live in RoutableModelIDs for the CPA
// scheduler and stay out of the human-facing surface.
type CompactModel struct {
	// ID is the base model id (e.g. "claude-opus-5"). It is what a request
	// should carry in its `model` field when using parameterised routing.
	ID string `json:"id"`

	// DisplayName is Cursor's IDE label (e.g. "Claude Opus 5") when the
	// catalog carries client_display_name; otherwise it falls back to ID.
	DisplayName string `json:"display_name,omitempty"`

	// SupportsThinking, SupportsMaxMode, SupportsImages, SupportsAgent reflect
	// the boolean capability flags Cursor's picker reads to gate its own UI.
	SupportsThinking bool `json:"supports_thinking,omitempty"`
	SupportsMaxMode  bool `json:"supports_max_mode,omitempty"`
	SupportsImages   bool `json:"supports_images,omitempty"`
	SupportsAgent    bool `json:"supports_agent,omitempty"`

	// ContextTokenLimit / ContextTokenLimitForMaxMode carry the numeric
	// context window Cursor advertises for each mode. Zero means unknown.
	ContextTokenLimit           int32 `json:"context_token_limit,omitempty"`
	ContextTokenLimitForMaxMode int32 `json:"context_token_limit_for_max_mode,omitempty"`

	// Parameters describes each user-tunable knob (thinking/effort/fast/...)
	// with its type, allowed values, and human-readable labels. This is the
	// data source the IDE picker's dropdowns are built from.
	Parameters []CompactParameter `json:"parameters,omitempty"`

	// Variants enumerates the routable slugs Cursor advertises for this
	// base model (e.g. "claude-opus-5-thinking-max-fast") together with
	// the parameter values they resolve to, a cost tier index, and a
	// human-readable display name.
	//
	// This is the data an operations layer (New API's ModelRatio /
	// abilities tables, a billing dashboard, a routing gateway) needs to
	// price and route variants consistently WITHOUT having to expose all
	// slugs to end users in the picker. Consumers list only `ID` in the
	// public model picker, keep the full slug set in their routing table,
	// and use `Tier` for a coarse cost multiplier when Cursor'"'"'s catalog
	// does not carry an explicit price.
	Variants []CompactVariant `json:"variants,omitempty"`
}

// CompactVariant is one routable slug for a base model. Slug is what a
// client actually sends when it wants a specific configuration; ParameterValues
// is the equivalent {parameter_id: value} map for base+parameters submission.
// Tier is 0 for the default variant and increases with each "increases model
// cost" flag on the parameter values used — see CompactModelListing for the
// exact scoring rule.
type CompactVariant struct {
	Slug            string            `json:"slug"`
	DisplayName     string            `json:"display_name,omitempty"`
	IsMaxMode       bool              `json:"is_max_mode,omitempty"`
	IsDefault       bool              `json:"is_default,omitempty"`
	ParameterValues map[string]string `json:"parameter_values,omitempty"`
	CostFlags       []string          `json:"cost_flags,omitempty"`
	Tier            int               `json:"tier"`
}

// CompactParameter matches Cursor's ModelParameterDefinition wire shape but
// flattened for JSON consumers. Kind is either "boolean" or "enum".
type CompactParameter struct {
	ID      string                  `json:"id"`
	Name    string                  `json:"name,omitempty"`
	Kind    string                  `json:"kind"`
	Tooltip string                  `json:"tooltip,omitempty"`
	Values  []CompactParameterValue `json:"values,omitempty"`
}

// CompactParameterValue is one dropdown option. IncreasesModelCost is exposed
// so a UI can badge premium tiers the way the IDE does.
type CompactParameterValue struct {
	Value              string `json:"value"`
	DisplayName        string `json:"display_name,omitempty"`
	IncreasesModelCost bool   `json:"increases_model_cost,omitempty"`
	BlockedByAdmin     bool   `json:"blocked_by_admin,omitempty"`
	Tooltip            string `json:"tooltip,omitempty"`
}

// CompactModelListing returns one entry per primary model in the live catalog.
// Variants ARE NOT expanded — the picker/CLI presents them as parameter values
// on the base id instead. The list is sorted alphabetically for stable output.
//
// The empty catalog (nil resp, no models, or a catalog whose entries are all
// exploded variants) returns nil so callers can fall back cleanly.
func CompactModelListing(resp *cursorpb.AiserverV1_AvailableModelsResponse) []CompactModel {
	if resp == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]CompactModel, 0, len(resp.GetModels()))
	for _, m := range resp.GetModels() {
		if m == nil {
			continue
		}
		id := strings.TrimSpace(baseModelID(m))
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			// Multiple catalog rows can fold to the same base id when Cursor
			// still ships an exploded shape. Keep the first row (which is
			// typically the primary), so we don't double-report the model.
			continue
		}
		seen[id] = struct{}{}

		entry := CompactModel{
			ID:                          id,
			DisplayName:                 strings.TrimSpace(m.GetClientDisplayName()),
			SupportsThinking:            m.GetSupportsThinking(),
			SupportsMaxMode:             m.GetSupportsMaxMode(),
			SupportsImages:              m.GetSupportsImages(),
			SupportsAgent:               m.GetSupportsAgent(),
			ContextTokenLimit:           m.GetContextTokenLimit(),
			ContextTokenLimitForMaxMode: m.GetContextTokenLimitForMaxMode(),
		}
		if entry.DisplayName == "" {
			entry.DisplayName = id
		}
		entry.Parameters = compactParameterDefinitions(m.GetParameterDefinitions())
		entry.Variants = compactVariants(m)
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func compactParameterDefinitions(defs []*cursorpb.AiserverV1_ModelParameterDefinition) []CompactParameter {
	if len(defs) == 0 {
		return nil
	}
	out := make([]CompactParameter, 0, len(defs))
	for _, def := range defs {
		if def == nil {
			continue
		}
		id := strings.TrimSpace(def.GetId())
		if id == "" {
			continue
		}
		entry := CompactParameter{
			ID:      id,
			Name:    strings.TrimSpace(def.GetName()),
			Tooltip: strings.TrimSpace(def.GetMarkdownTooltip()),
		}
		pt := def.GetParameterType()
		switch {
		case pt.GetBooleanParameter() != nil:
			entry.Kind = "boolean"
			for _, v := range pt.GetBooleanParameter().GetValues() {
				if v == nil {
					continue
				}
				entry.Values = append(entry.Values, CompactParameterValue{
					Value:              strings.TrimSpace(v.GetValue()),
					DisplayName:        strings.TrimSpace(v.GetDisplayName()),
					IncreasesModelCost: v.GetIncreasesModelCost(),
					BlockedByAdmin:     v.GetBlockedByAdminAllowlist(),
				})
			}
		case pt.GetEnumParameter() != nil:
			entry.Kind = "enum"
			for _, v := range pt.GetEnumParameter().GetValues() {
				if v == nil {
					continue
				}
				entry.Values = append(entry.Values, CompactParameterValue{
					Value:              strings.TrimSpace(v.GetValue()),
					DisplayName:        strings.TrimSpace(v.GetDisplayName()),
					IncreasesModelCost: v.GetIncreasesModelCost(),
					BlockedByAdmin:     v.GetBlockedByAdminAllowlist(),
					Tooltip:            strings.TrimSpace(v.GetMarkdownTooltip()),
				})
			}
		default:
			// Unknown parameter type — surface the id so callers can at least
			// display it, but leave Kind empty so the UI knows it's opaque.
		}
		out = append(out, entry)
	}
	return out
}

// compactVariants enumerates the routable variant slugs for one base model
// with the parameter values they resolve to and a coarse cost tier index.
//
// Tier scoring: each parameter value that Cursor'"'"'s catalog flagged with
// increases_model_cost=true contributes one point, plus one point when the
// variant sits under is_max_mode=true. Tier 0 is the default variant (no
// upgrades). This mirrors what the IDE picker uses to badge premium tiers
// and lets an operator generate consistent New API ModelRatio entries
// without hand-maintaining a per-slug price table.
//
// The exploded parameter-string form (VariantStringRepresentation, e.g.
// "claude-opus-5[thinking=true,context=1m,effort=max,fast=true]") is
// deliberately not emitted here — no client submits it verbatim, and the
// short LegacySlug ("claude-opus-5-thinking-max-fast") is the routable
// name every real caller uses.
func compactVariants(model *cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel) []CompactVariant {
	if model == nil {
		return nil
	}
	// Pre-compute the (paramID, value) → increases_cost lookup from the
	// parameter_definitions so per-variant scoring is O(1) per pair.
	costFlagged := make(map[string]bool)
	for _, def := range model.GetParameterDefinitions() {
		if def == nil {
			continue
		}
		id := strings.TrimSpace(def.GetId())
		if id == "" {
			continue
		}
		pt := def.GetParameterType()
		if pt.GetBooleanParameter() != nil {
			for _, v := range pt.GetBooleanParameter().GetValues() {
				if v == nil || !v.GetIncreasesModelCost() {
					continue
				}
				costFlagged[id+"="+strings.TrimSpace(v.GetValue())] = true
			}
		}
		if pt.GetEnumParameter() != nil {
			for _, v := range pt.GetEnumParameter().GetValues() {
				if v == nil || !v.GetIncreasesModelCost() {
					continue
				}
				costFlagged[id+"="+strings.TrimSpace(v.GetValue())] = true
			}
		}
	}

	seen := make(map[string]struct{})
	out := make([]CompactVariant, 0, len(model.GetVariants()))
	for _, v := range model.GetVariants() {
		if v == nil {
			continue
		}
		slug := strings.TrimSpace(v.GetLegacySlug())
		if slug == "" {
			continue
		}
		if _, dup := seen[slug]; dup {
			// The catalog can repeat a slug across max/non-max context
			// windows (context=300k vs context=1m for the same effort).
			// Keep the first — CPA'"'"'s scheduler sees identical routing.
			continue
		}
		seen[slug] = struct{}{}

		params := make(map[string]string, len(v.GetParameterValues()))
		var flags []string
		tier := 0
		for _, pv := range v.GetParameterValues() {
			if pv == nil {
				continue
			}
			id := strings.TrimSpace(pv.GetId())
			val := strings.TrimSpace(pv.GetValue())
			if id == "" {
				continue
			}
			params[id] = val
			if costFlagged[id+"="+val] {
				flags = append(flags, id+"="+val)
				tier++
			}
		}
		if v.GetIsMaxMode() {
			tier++
			flags = append(flags, "max_mode")
		}
		out = append(out, CompactVariant{
			Slug:            slug,
			DisplayName:     strings.TrimSpace(v.GetDisplayName()),
			IsMaxMode:       v.GetIsMaxMode(),
			IsDefault:       v.GetIsDefaultNonMaxConfig() || v.GetIsDefaultMaxConfig(),
			ParameterValues: params,
			CostFlags:       flags,
			Tier:            tier,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Tier != out[j].Tier {
			return out[i].Tier < out[j].Tier
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}
