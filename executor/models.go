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
func (c *Client) ListModels() (*cursorpb.AiserverV1_AvailableModelsResponse, error) {
	req := &cursorpb.AiserverV1_AvailableModelsRequest{}
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
