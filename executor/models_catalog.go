package executor

import (
	"strings"

	"google.golang.org/protobuf/proto"

	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

// AvailableModelIDs returns the stable primary IDs shown by Cursor's current
// model picker. Variant slugs remain accepted by resolveRequestedModelFromCatalog
// but are intentionally not advertised as separate models: Cursor 3.16 sends
// one catalog model ID plus parameter values instead of treating every effort,
// thinking, context, and fast combination as a distinct upstream model.
func AvailableModelIDs(resp *cursorpb.AiserverV1_AvailableModelsResponse) []string {
	if resp == nil {
		return nil
	}
	seen := make(map[string]struct{})
	out := make([]string, 0, len(resp.GetModels()))
	for _, model := range resp.GetModels() {
		id := primaryModelID(model)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
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
