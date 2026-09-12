package executor

// Cursor 3.16 shifted model discovery to a base+parameters shape: the picker
// UI collapses every effort/thinking/fast combination into a single row and
// renders a dropdown for the parameters instead of a flat 600-entry list.
// AvailableModelIDs already returns the 38 primary ids; this file adds the
// parameter metadata that goes with them so a caller (management panel, CLI
// tools) can present the same UX the IDE does.

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
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
// client actually sends when it wants a specific configuration;
// ParameterValues is the equivalent {parameter_id: value} map for
// base+parameters submission.
//
// CostMultiplier is Cursor's own pricing factor relative to the base
// variant (base = 1.0). It combines two sources:
//
//  1. The variant's tooltip_data.markdown_content string, which Cursor
//     server writes into the picker UI. When fast mode is on, the
//     tooltip contains "at Nx the price, using Anthropic's fast mode",
//     so we extract the multiplier verbatim.
//  2. Cursor's advertised max-mode pricing (6x by default), applied
//     when is_max_mode is true. The 6x default is what Cursor's
//     dashboard has published; override via env
//     CURSOR_MAX_MODE_COST_MULTIPLIER to track future changes without
//     a plugin rebuild.
//
// Effort tiers (low/medium/high/xhigh/max) DO NOT increase price on
// their own — Cursor bills them at the same rate as the base — so they
// do not contribute to CostMultiplier. Thinking on/off is the same
// wire model with a reasoning gate; also no price change.
type CompactVariant struct {
	Slug            string            `json:"slug"`
	DisplayName     string            `json:"display_name,omitempty"`
	IsMaxMode       bool              `json:"is_max_mode,omitempty"`
	IsDefault       bool              `json:"is_default,omitempty"`
	ParameterValues map[string]string `json:"parameter_values,omitempty"`
	// CostMultiplier is the price factor Cursor charges for this variant
	// compared to the base (base = 1.0). fast=true adds the multiplier
	// Cursor puts in the tooltip ("2x the price"), is_max_mode adds the
	// Cursor max-mode factor (6x by default, override via
	// CURSOR_MAX_MODE_COST_MULTIPLIER). Both apply multiplicatively.
	CostMultiplier float64 `json:"cost_multiplier"`
	// CostFlags is the human-readable breakdown of what contributed to
	// CostMultiplier. Example: ["fast=2x", "max_mode=6x"] on a
	// fast+max variant would multiply out to 12x.
	CostFlags []string `json:"cost_flags,omitempty"`
	// Tier is a coarse integer bucket derived from CostMultiplier,
	// intended for UIs that only need "base / mid / premium" grouping:
	// 0 for 1x, 1 for <=2x, 2 for <=4x, 3 for <=8x, 4 for >8x.
	Tier int `json:"tier"`
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
// with the parameter values they resolve to and Cursor's own cost multiplier.
//
// Cost model (matches Cursor's live dashboard, verified against
// TooltipData.markdown_content from a real 3.19.7 catalog on 2026-09-12):
//
//   - base = 1.0x
//   - fast=true: the tooltip carries a "Nx the price" string (typically
//     2x for Anthropic fast mode); we parse it out per variant so a
//     future change to that number auto-propagates.
//   - is_max_mode=true: Cursor charges max mode at 6x by default. That
//     number is dashboard-only (not in the wire protocol), so it lives
//     as a plugin constant. Override via CURSOR_MAX_MODE_COST_MULTIPLIER
//     if Cursor changes it.
//   - effort tiers (low/medium/high/xhigh/max) and thinking on/off:
//     Cursor does NOT bill differently — they are routing knobs, not
//     price levers — so they do not affect CostMultiplier.
//
// Fast + max_mode combine multiplicatively (2 * 6 = 12x for a
// fast max-mode variant), which is what Cursor's dashboard shows.
//
// The exploded parameter-string form (VariantStringRepresentation, e.g.
// "claude-opus-5[thinking=true,context=1m,effort=max,fast=true]") is
// deliberately not emitted here — no client submits it verbatim, and the
// short LegacySlug ("claude-opus-5-thinking-max-fast") is the routable
// name every real caller uses.
// fastCostMultiplierRE captures the "at Nx the price" phrase Cursor
// server writes into a variant tooltip when fast mode adds a premium.
// Sampled 2026-09-12 against a live 3.19.7 catalog:
//
//	"...at 2x the price, using Anthropic's fast mode..."
//
// The regex tolerates decimal multipliers (e.g. "1.5x") in case Cursor
// changes the number for a specific model family in the future.
var fastCostMultiplierRE = regexp.MustCompile(`at\s+(\d+(?:\.\d+)?)x\s+the\s+price`)

// defaultMaxModeCostMultiplier is Cursor's published max-mode premium
// (6x). Not in the wire protocol — sourced from Cursor's dashboard.
// Override at plugin startup via CURSOR_MAX_MODE_COST_MULTIPLIER.
const defaultMaxModeCostMultiplier = 6.0

func maxModeCostMultiplier() float64 {
	raw := strings.TrimSpace(os.Getenv("CURSOR_MAX_MODE_COST_MULTIPLIER"))
	if raw == "" {
		return defaultMaxModeCostMultiplier
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		return defaultMaxModeCostMultiplier
	}
	return v
}

// costMultiplierTier buckets a raw multiplier into a coarse integer for
// UIs that want a small badge instead of a decimal. 1x → 0, ≤2x → 1,
// ≤4x → 2, ≤8x → 3, >8x → 4. Matches how Cursor's own picker groups
// premium tiers.
func costMultiplierTier(m float64) int {
	switch {
	case m <= 1.0:
		return 0
	case m <= 2.0:
		return 1
	case m <= 4.0:
		return 2
	case m <= 8.0:
		return 3
	default:
		return 4
	}
}

func compactVariants(model *cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel) []CompactVariant {
	if model == nil {
		return nil
	}
	maxMul := maxModeCostMultiplier()

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
			// Keep the first — CPA's scheduler sees identical routing.
			continue
		}
		seen[slug] = struct{}{}

		params := make(map[string]string, len(v.GetParameterValues()))
		fastVal := ""
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
			if id == "fast" {
				fastVal = val
			}
		}

		multiplier := 1.0
		var flags []string
		if fastVal == "true" {
			fastMul := extractFastCostMultiplier(v)
			if fastMul <= 0 {
				fastMul = 2.0 // fallback matches Cursor's default fast pricing
			}
			multiplier *= fastMul
			flags = append(flags, fmt.Sprintf("fast=%sx", trimTrailingZeros(fastMul)))
		}
		if v.GetIsMaxMode() {
			multiplier *= maxMul
			flags = append(flags, fmt.Sprintf("max_mode=%sx", trimTrailingZeros(maxMul)))
		}

		out = append(out, CompactVariant{
			Slug:            slug,
			DisplayName:     strings.TrimSpace(v.GetDisplayName()),
			IsMaxMode:       v.GetIsMaxMode(),
			IsDefault:       v.GetIsDefaultNonMaxConfig() || v.GetIsDefaultMaxConfig(),
			ParameterValues: params,
			CostMultiplier:  multiplier,
			CostFlags:       flags,
			Tier:            costMultiplierTier(multiplier),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CostMultiplier != out[j].CostMultiplier {
			return out[i].CostMultiplier < out[j].CostMultiplier
		}
		return out[i].Slug < out[j].Slug
	})
	return out
}

// extractFastCostMultiplier reads the price multiplier out of the
// variant's tooltip markdown, which Cursor server populates with a
// literal "at Nx the price" phrase when fast mode adds a premium.
// Returns 0 when the tooltip isn't present (older catalogs) so the
// caller can fall back to the default.
func extractFastCostMultiplier(v *cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig) float64 {
	if v == nil {
		return 0
	}
	td := v.GetTooltipData()
	if td == nil {
		return 0
	}
	candidates := []string{td.GetMarkdownContent(), td.GetSecondaryText(), td.GetPrimaryText()}
	for _, s := range candidates {
		if s == "" {
			continue
		}
		if m := fastCostMultiplierRE.FindStringSubmatch(s); len(m) == 2 {
			if val, err := strconv.ParseFloat(m[1], 64); err == nil && val > 0 {
				return val
			}
		}
	}
	return 0
}

// trimTrailingZeros formats a float without unnecessary trailing zeros
// (2.0 → "2", 1.5 → "1.5") so cost_flags read naturally.
func trimTrailingZeros(v float64) string {
	s := strconv.FormatFloat(v, 'f', -1, 64)
	return s
}
