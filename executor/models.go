package executor

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/cursor-proto/auth"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

const modelCatalogTTL = 5 * time.Minute

// ListModels calls AiService/AvailableModels and returns the model list.
//
// The request payload mirrors what the Cursor 3.16.17 IDE model picker
// sends (verified against workbench.glass.main.js's `refreshDefaultModels`
// at offset ~39.7M):
//
//	new AvailableModelsRequest({
//	    isNightly: false,
//	    excludeMaxNamedModels: true,   // do not emit *-max sibling rows
//	    additionalModelNames: [],       // user-added external models
//	    useModelParameters: true,       // send parameterised catalog
//	    useReactModelPicker: true,      // return the newer picker shape
//	    byokEnabled: false,             // user has BYO OpenAI key
//	})
//
// These flags preserve the current IDE request shape and parameterised
// catalog representation. They do not determine Claude entitlement: live
// differential probes show the same account receives 24 non-Claude models
// through Go and 35 models (11 Claude) through Chromium with identical
// protobuf fields and x-cursor headers. See cmd/test-model-stack.
func (c *Client) ListModels() (*cursorpb.AiserverV1_AvailableModelsResponse, error) {
	useModelParameters := true
	useReactModelPicker := true
	byokEnabled := false
	req := &cursorpb.AiserverV1_AvailableModelsRequest{
		IsNightly:             false,
		ExcludeMaxNamedModels: true,
		AdditionalModelNames:  nil,
		UseModelParameters:    &useModelParameters,
		UseReactModelPicker:   &useReactModelPicker,
		ByokEnabled:           &byokEnabled,
	}
	var resp cursorpb.AiserverV1_AvailableModelsResponse
	if err := c.UnaryCall("aiserver.v1.AiService", "AvailableModels", req, &resp); err != nil {
		return nil, err
	}
	c.rememberModelCatalog(&resp, c.CurrentAccount())
	return &resp, nil
}

func (c *Client) resolveRequestedModel(model string, parameters map[string]string) (*cursorpb.AgentV1_RequestedModel, bool, error) {
	account := c.CurrentAccount()
	identity := modelCatalogIdentity(account)

	c.modelCatalogMu.RLock()
	catalog := c.modelCatalog
	fresh := catalog != nil && c.modelCatalogIdentity == identity && time.Since(c.modelCatalogAt) < modelCatalogTTL
	c.modelCatalogMu.RUnlock()

	if !fresh {
		var err error
		catalog, err = c.ListModels()
		if err != nil {
			return nil, false, fmt.Errorf("load live model catalog: %w", err)
		}
	}
	requested, ok := resolveRequestedModelFromCatalogWithParameters(catalog, model, parameters)
	return requested, ok, nil
}

func (c *Client) rememberModelCatalog(catalog *cursorpb.AiserverV1_AvailableModelsResponse, account *auth.Account) {
	if catalog == nil {
		return
	}
	c.modelCatalogMu.Lock()
	c.modelCatalog = proto.Clone(catalog).(*cursorpb.AiserverV1_AvailableModelsResponse)
	c.modelCatalogIdentity = modelCatalogIdentity(account)
	c.modelCatalogAt = time.Now()
	c.modelCatalogMu.Unlock()
}

func modelCatalogIdentity(account *auth.Account) string {
	if account == nil {
		return ""
	}
	return account.AuthID + "\x00" + account.AccessToken
}

// GetDefaultModel calls AiService/GetDefaultModel and returns the raw response.
func (c *Client) GetDefaultModel() (*cursorpb.AiserverV1_GetDefaultModelResponse, error) {
	req := &cursorpb.AiserverV1_GetDefaultModelRequest{}
	var resp cursorpb.AiserverV1_GetDefaultModelResponse
	if err := c.UnaryCall("aiserver.v1.AiService", "GetDefaultModel", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
