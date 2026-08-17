package executor

import (
	"strings"

	"google.golang.org/protobuf/proto"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// stableModelFallbackIDs is the last known-good set of primary model IDs for
// the current Cursor protocol line. Live catalog discovery remains the source
// of truth; this fallback exists for hosts that must register model ownership
// before they have called model.for_auth for an account.
var stableModelFallbackIDs = []string{
	"auto-smart",
	"claude-fable-5",
	"claude-haiku-4-5",
	"claude-opus-4-5",
	"claude-opus-4-6",
	"claude-opus-4-7",
	"claude-opus-4-8",
	"claude-opus-5",
	"claude-sonnet-4",
	"claude-sonnet-4-5",
	"claude-sonnet-4-6",
	"claude-sonnet-5",
	"composer-2.5",
	"gemini-2.5-flash",
	"gemini-3-flash",
	"gemini-3.1-pro",
	"gemini-3.5-flash",
	"gemini-3.6-flash",
	"gemini-3.7-flash",
	"glm-5.2",
	"gpt-5-mini",
	"gpt-5.1",
	"gpt-5.2",
	"gpt-5.3-codex",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.4-nano",
	"gpt-5.5",
	"gpt-5.6-luna",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"grok-4.5",
	"grok-4.6",
	"kimi-k2.7-code",
	"kimi-k3",
}

// StableModelFallbackIDs returns a copy of the current protocol line's
// registration fallback. Callers must still prefer AvailableModelIDs from a
// live account whenever the host supports per-auth model discovery.
func StableModelFallbackIDs() []string {
	return append([]string(nil), stableModelFallbackIDs...)
}

// StableRoutableModelFallbackIDs returns the static ownership set used when
// live catalog discovery is temporarily unavailable. Cursor has shipped both
// compact catalogs (one base model plus parameters) and flattened catalogs
// (one model id per parameter combination). CPA resolves model ownership
// before invoking the plugin, so the fallback must claim both shapes or a
// valid request such as claude-opus-5-medium is rejected as an unknown
// provider before the executor can normalize it against the live catalog.
func StableRoutableModelFallbackIDs() []string {
	variantSuffixes := []string{
		"-low",
		"-medium",
		"-high",
		"-xhigh",
		"-max",
		"-thinking-low",
		"-thinking-medium",
		"-thinking-high",
		"-thinking-xhigh",
		"-thinking-max",
		"-longcontext",
		"-long-context",
	}

	seen := make(map[string]struct{}, len(stableModelFallbackIDs)*(len(variantSuffixes)*2+2))
	out := make([]string, 0, len(seen))
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

	for _, base := range stableModelFallbackIDs {
		add(base)
		add(base + "-fast")
		for _, suffix := range variantSuffixes {
			add(base + suffix)
			add(base + suffix + "-fast")
		}
	}
	return out
}

// AvailableModelIDs returns the stable primary IDs shown by Cursor's current
// model picker. Variant slugs remain accepted by resolveRequestedModelFromCatalog
// but are intentionally not advertised as separate models: Cursor 3.16 sends
// one catalog model ID plus parameter values instead of treating every effort,
// thinking, context, and fast combination as a distinct upstream model.
//
// Older IDE-side settings (or a server that ignores our
// use_model_parameters hint) still receive the exploded catalog where each
// entry's Name is a variant slug such as `claude-sonnet-4-5-thinking-high`.
// When we detect that shape we fold variants back to their primary id using
// server_model_name / id_aliases / legacy_slugs / heuristic-stripped variant
// suffixes so CPA's model registry sees the base names its chat requests
// actually target.
func AvailableModelIDs(resp *cursorpb.AiserverV1_AvailableModelsResponse) []string {
	if resp == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(resp.GetModels()))
	add := func(id string) {
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
		add(baseModelID(model))
	}
	return out
}

// RoutableModelIDs returns every model name the executor can accept from a
// downstream router. It includes the compact primary catalog returned by
// AvailableModelIDs plus the live variant slugs Cursor exposes for effort,
// thinking, context, and fast-mode choices.
//
// User-facing model lists can stay compact by using AvailableModelIDs. CPA's
// model.for_auth registration must use this wider set, however, because CPA
// resolves the provider before invoking the plugin. If a valid variant such
// as claude-opus-5-high is omitted here, CPA rejects it as "unknown provider"
// even though resolveRequestedModelFromCatalog can map it correctly.
func RoutableModelIDs(resp *cursorpb.AiserverV1_AvailableModelsResponse) []string {
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
		base := baseModelID(model)
		add(base)

		// Exploded catalogs put the routable variant in Name while baseModelID
		// folds it back to the primary model.
		if name := strings.TrimSpace(model.GetName()); name != base {
			add(name)
		}
		for _, variant := range model.GetVariants() {
			add(variant.GetLegacySlug())
			add(variant.GetVariantStringRepresentation())
		}
		// Some catalog revisions populate legacy_slugs without expanding the
		// Variants field. Restrict these to children of the primary id so short
		// marketing aliases such as "default" are not claimed accidentally.
		for _, slug := range model.GetLegacySlugs() {
			slug = strings.TrimSpace(slug)
			if base != "" && strings.HasPrefix(slug, base+"-") {
				add(slug)
			}
		}
	}
	return out
}

// baseModelID returns the primary model id for a catalog entry.
//
// For the modern Cursor 3.16 parameterised catalog Name is already the base
// (e.g. "claude-sonnet-4-5") and the variants live under Variants — we return
// Name verbatim. server_model_name is the same string on this shape, and we
// deliberately do NOT touch id_aliases/legacy_slugs (they carry marketing
// abbreviations like "gpt" / "gemini" and every variant slug respectively —
// collapsing to those would over-fold distinct base models into one).
//
// The fallback path fires only when Name matches an exploded-variant shape
// (a Name identical to a legacy_slug entry, or trailing a known variant
// suffix). This covers the 207-row team catalog codex tracked down: each
// row lands as `<base>-<effort>-<thinking>` with the base repeated in
// id_aliases/legacy_slugs. In that case we recover the base via the
// server_model_name (if different from Name) or by stripping the trailing
// variant suffixes.
func baseModelID(model *cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel) string {
	if model == nil {
		return ""
	}
	name := strings.TrimSpace(model.GetName())
	if name == "" {
		return strings.TrimSpace(model.GetServerModelName())
	}
	server := strings.TrimSpace(model.GetServerModelName())
	if !looksLikeExplodedVariant(model, name) {
		return name
	}
	// Exploded shape detected: prefer server_model_name when it disagrees
	// with name (the exploded row's server field carries the base), then
	// try to strip the variant suffix off name, and finally give up and
	// return name verbatim so the caller at least advertises something.
	if server != "" && server != name {
		return server
	}
	if stripped := stripVariantSuffixes(name); stripped != "" && stripped != name {
		return stripped
	}
	return name
}

// looksLikeExplodedVariant returns true when a catalog row appears to be a
// single variant of a base model rather than a parameterised base.
// Heuristic:
//
//  1. The row's legacy_slugs list contains name itself — the modern shape
//     never repeats name in that list because Variants carry the slugs.
//  2. name ends with a known variant suffix AND one of the id_aliases or
//     legacy_slugs holds a shorter form of it. This second guard prevents
//     us from stripping legitimate base names that happen to end in the
//     word "max" or "fast" without an actual sibling row.
func looksLikeExplodedVariant(model *cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel, name string) bool {
	for _, slug := range model.GetLegacySlugs() {
		if strings.TrimSpace(slug) == name {
			return true
		}
	}
	if !hasKnownVariantSuffix(name) {
		return false
	}
	stripped := stripVariantSuffixes(name)
	if stripped == "" || stripped == name {
		return false
	}
	for _, alias := range model.GetIdAliases() {
		if strings.TrimSpace(alias) == stripped {
			return true
		}
	}
	for _, slug := range model.GetLegacySlugs() {
		if strings.TrimSpace(slug) == stripped {
			return true
		}
	}
	// server_model_name equal to the stripped form is a strong signal the
	// row is a variant that happens to omit the id from the alias arrays.
	if strings.TrimSpace(model.GetServerModelName()) == stripped {
		return true
	}
	return false
}

// variantSuffixes lists the trailing tokens Cursor appends to a base model
// id when it emits an exploded-variant catalog row. Ordering matters — the
// longest match wins so "claude-opus-5-thinking-high" collapses cleanly to
// "claude-opus-5" without partial matches on "-high" leaving orphans.
var variantSuffixes = []string{
	"-thinking-max",
	"-thinking-high",
	"-thinking-medium",
	"-thinking-low",
	"-thinking-fast",
	"-thinking",
	"-longcontext",
	"-long-context",
	"-max",
	"-fast",
	"-effort-max",
	"-effort-high",
	"-effort-medium",
	"-effort-low",
}

// hasKnownVariantSuffix reports whether name ends with any variant suffix.
// Used by looksLikeExplodedVariant to keep the fold-back path narrowly
// scoped to catalogs that emit one row per (base, effort, thinking) tuple.
func hasKnownVariantSuffix(name string) bool {
	for _, suffix := range variantSuffixes {
		if strings.HasSuffix(name, suffix) && name != suffix {
			return true
		}
	}
	return false
}

// stripVariantSuffixes trims a single Cursor variant suffix from name. It is
// applied repeatedly so pathological compound suffixes like
// `-thinking-high-fast` collapse to their base id. Returns "" when the input
// is empty so the caller can treat "" as a signal to keep the raw name.
func stripVariantSuffixes(name string) string {
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return ""
	}
	for changed := true; changed; {
		changed = false
		for _, suffix := range variantSuffixes {
			if strings.HasSuffix(trimmed, suffix) && trimmed != suffix {
				trimmed = strings.TrimSuffix(trimmed, suffix)
				changed = true
				break
			}
		}
	}
	return trimmed
}

func resolveRequestedModelFromCatalog(resp *cursorpb.AiserverV1_AvailableModelsResponse, requestedID string) (*cursorpb.AgentV1_RequestedModel, bool) {
	return resolveRequestedModelFromCatalogWithParameters(resp, requestedID, nil)
}

func resolveRequestedModelFromCatalogWithParameters(resp *cursorpb.AiserverV1_AvailableModelsResponse, requestedID string, parameters map[string]string) (*cursorpb.AgentV1_RequestedModel, bool) {
	requestedID = strings.TrimSpace(requestedID)
	if resp == nil || requestedID == "" {
		return nil, false
	}
	for _, model := range resp.GetModels() {
		modelID := primaryModelID(model)
		if modelID == "" {
			continue
		}
		if requestedID == modelID || requestedID == strings.TrimSpace(model.GetServerModelName()) || containsTrimmed(model.GetIdAliases(), requestedID) {
			if variant := matchingModelVariant(model, parameters); variant != nil {
				return requestedModelForVariant(modelID, variant), true
			}
			return requestedModelForVariant(modelID, defaultModelVariant(model)), true
		}
		for _, variant := range model.GetVariants() {
			if requestedID == strings.TrimSpace(variant.GetLegacySlug()) || requestedID == strings.TrimSpace(variant.GetVariantStringRepresentation()) {
				return requestedModelForVariant(modelID, variant), true
			}
		}
		if containsTrimmed(model.GetLegacySlugs(), requestedID) {
			for _, variant := range model.GetVariants() {
				if requestedID == strings.TrimSpace(variant.GetLegacySlug()) {
					return requestedModelForVariant(modelID, variant), true
				}
			}
		}
	}
	return nil, false
}

func matchingModelVariant(model *cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel, parameters map[string]string) *cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig {
	if model == nil || len(parameters) == 0 {
		return nil
	}
	for _, variant := range model.GetVariants() {
		values := make(map[string]string, len(variant.GetParameterValues()))
		for _, parameter := range variant.GetParameterValues() {
			values[strings.TrimSpace(parameter.GetId())] = strings.TrimSpace(parameter.GetValue())
		}
		matches := true
		for id, value := range parameters {
			if values[strings.TrimSpace(id)] != strings.TrimSpace(value) {
				matches = false
				break
			}
		}
		if matches {
			return variant
		}
	}
	return nil
}

func primaryModelID(model *cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel) string {
	if model == nil {
		return ""
	}
	if id := strings.TrimSpace(model.GetName()); id != "" {
		return id
	}
	return strings.TrimSpace(model.GetServerModelName())
}

func defaultModelVariant(model *cursorpb.AiserverV1_AvailableModelsResponse_AvailableModel) *cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig {
	if model == nil {
		return nil
	}
	for _, variant := range model.GetVariants() {
		if variant.GetIsDefaultNonMaxConfig() {
			return variant
		}
	}
	for _, variant := range model.GetVariants() {
		if !variant.GetIsMaxMode() {
			return variant
		}
	}
	if len(model.GetVariants()) > 0 {
		return model.GetVariants()[0]
	}
	return nil
}

func requestedModelForVariant(modelID string, variant *cursorpb.AiserverV1_AvailableModelsResponse_ModelVariantConfig) *cursorpb.AgentV1_RequestedModel {
	requested := &cursorpb.AgentV1_RequestedModel{
		ModelId:      modelID,
		BuiltInModel: true,
	}
	if variant == nil {
		return requested
	}
	requested.MaxMode = variant.GetIsMaxMode()
	requested.Parameters = make([]*cursorpb.AgentV1_RequestedModel_ModelParameterValue, 0, len(variant.GetParameterValues()))
	for _, parameter := range variant.GetParameterValues() {
		if parameter == nil {
			continue
		}
		requested.Parameters = append(requested.Parameters, proto.Clone(parameter).(*cursorpb.AgentV1_RequestedModel_ModelParameterValue))
	}
	return requested
}

func containsTrimmed(values []string, requested string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == requested {
			return true
		}
	}
	return false
}
