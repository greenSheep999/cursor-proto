package kernel

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

const pluginVersion = "0.6.1"

// registerResult is the JSON returned for plugin.register / plugin.reconfigure.
func registerResult() string {
	body := map[string]any{
		"schema_version": 1,
		"metadata": map[string]any{
			"Name":             "cursor",
			"Version":          pluginVersion,
			"Author":           "router-for-me",
			"GitHubRepository": "https://github.com/router-for-me/cursor-proto",
			"Logo":             CursorLogoDataURI(),
			"ConfigFields":     []any{},
		},
		"capabilities": map[string]any{
			"auth_provider":           true,
			"executor":                true,
			"executor_model_scope":    "oauth",
			"executor_input_formats":  []string{"openai", "claude"},
			"executor_output_formats": []string{"openai", "claude"},
			"model_provider":          true,
			"management_api":          true,
		},
	}
	buf, _ := json.Marshal(body)
	return string(buf)
}

// identifierResult is the JSON returned by auth.identifier and
// executor.identifier. Both point at the "cursor" provider key.
func identifierResult() string {
	buf, _ := json.Marshal(map[string]string{"identifier": pluginName})
	return string(buf)
}

// knownCursorModels is the static model list advertised through
// model.static / model.for_auth. Cursor exposes a richer list via a
// live gRPC call (executor.Client.ListModels), but static advertising
// only needs the popular defaults — the executor plugin dispatches the
// real request against the account regardless.
var knownCursorModels = []string{
	"composer-2.5",
	"composer-2",
	"claude-4.5-sonnet",
	"claude-4.5-haiku",
	"claude-opus-4.1",
	"gpt-5",
	"gpt-5-mini",
	"gpt-5-codex",
	"gemini-2.5-pro",
	"gemini-2.5-flash",
	"grok-code",
	"cursor-small",
}

// staticModelsResult returns the model list Cursor ships. Kept as a
// hand-maintained list so operators know what to expect before the
// account has been used for a live ListModels call.
func staticModelsResult() string {
	return modelsResult(knownCursorModels)
}

func modelsResult(names []string) string {
	models := make([]map[string]any, 0, len(names))
	for _, m := range names {
		models = append(models, map[string]any{
			"ID":                        m,
			"Object":                    "model",
			"Created":                   time.Now().Unix(),
			"OwnedBy":                   pluginName,
			"Type":                      "chat",
			"DisplayName":               m,
			"Name":                      m,
			"SupportedInputModalities":  []string{"text"},
			"SupportedOutputModalities": []string{"text"},
		})
	}
	body := map[string]any{
		"Provider": pluginName,
		"Models":   models,
	}
	buf, _ := json.Marshal(body)
	return string(buf)
}

type authModelRequest struct {
	AuthID       string `json:"AuthID"`
	AuthProvider string `json:"AuthProvider"`
	StorageJSON  []byte `json:"StorageJSON"`
}

var listModelsForAuth = func(acc *auth.Account) ([]string, error) {
	resp, err := executor.NewClient(acc).ListModels()
	if err != nil {
		return nil, err
	}
	return executor.AvailableModelIDs(resp), nil
}

func handleModelsForAuth(payload []byte) ([]byte, int) {
	var req authModelRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return okEnvelopeJSON(staticModelsResult()), 0
	}
	file, err := cpaformat.Unmarshal(req.StorageJSON)
	if err != nil {
		return okEnvelopeJSON(staticModelsResult()), 0
	}
	acc, err := file.ToAccount()
	if err != nil {
		return okEnvelopeJSON(staticModelsResult()), 0
	}
	names, err := listModelsForAuth(acc)
	if err != nil {
		return okEnvelopeJSON(staticModelsResult()), 0
	}
	if len(names) == 0 {
		return okEnvelopeJSON(staticModelsResult()), 0
	}
	return okEnvelopeJSON(modelsResult(names)), 0
}

// authParseRequest mirrors pluginapi.AuthParseRequest for the ABI JSON
// wire format. We only unmarshal the fields we actually use.
type authParseRequest struct {
	Provider string `json:"Provider"`
	Path     string `json:"Path"`
	FileName string `json:"FileName"`
	RawJSON  []byte `json:"RawJSON"`
}

// authData mirrors pluginapi.AuthData. StorageJSON is base64-encoded by
// the standard encoding/json marshaller when it sees a []byte, which
// matches what the host expects.
type authData struct {
	Provider         string            `json:"Provider"`
	ID               string            `json:"ID"`
	FileName         string            `json:"FileName"`
	Label            string            `json:"Label"`
	Prefix           string            `json:"Prefix,omitempty"`
	ProxyURL         string            `json:"ProxyURL,omitempty"`
	Disabled         bool              `json:"Disabled,omitempty"`
	StorageJSON      []byte            `json:"StorageJSON,omitempty"`
	Metadata         map[string]any    `json:"Metadata,omitempty"`
	Attributes       map[string]string `json:"Attributes,omitempty"`
	NextRefreshAfter time.Time         `json:"NextRefreshAfter,omitempty"`
}

type authParseResponse struct {
	Handled bool     `json:"Handled"`
	Auth    authData `json:"Auth"`
}

// handleAuthParse parses a CPA auth file into an authData for the host.
// It answers Handled=false for anything that does not carry our
// provider type so the host can fall back to another parser.
func handleAuthParse(payload []byte) ([]byte, int) {
	var req authParseRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse auth request: %v", err), false), 1
	}
	if strings.EqualFold(strings.TrimSpace(req.Provider), pluginName) == false && len(req.RawJSON) == 0 {
		// Nothing to inspect and provider does not match.
		buf, _ := json.Marshal(authParseResponse{Handled: false})
		return okEnvelopeJSON(string(buf)), 0
	}
	auth, err := cpaformat.Unmarshal(req.RawJSON)
	if err != nil {
		return errorEnvelope("bad_auth_file", err.Error(), false), 1
	}
	if auth.Type != cpaformat.ProviderType {
		buf, _ := json.Marshal(authParseResponse{Handled: false})
		return okEnvelopeJSON(string(buf)), 0
	}
	if err := auth.Validate(); err != nil {
		return errorEnvelope("bad_auth_file", err.Error(), false), 1
	}

	fileName := strings.TrimSpace(req.FileName)
	if fileName == "" && req.Path != "" {
		fileName = filepath.Base(req.Path)
	}
	if fileName == "" {
		fileName = auth.FileName()
	}
	label := strings.TrimSpace(auth.Email)
	if label == "" {
		label = pluginName
	}

	metadata := map[string]any{
		"type":       cpaformat.ProviderType,
		"email":      auth.Email,
		"user_id":    auth.UserID,
		"auth_id":    auth.AuthID,
		"expired":    auth.Expired,
		"machine_id": auth.MachineID,
	}
	if auth.DisableCooling {
		metadata["disable_cooling"] = true
	}
	if auth.RequestRetry > 0 {
		metadata["request_retry"] = auth.RequestRetry
	}
	if len(auth.ExcludedModels) > 0 {
		metadata["excluded_models"] = auth.ExcludedModels
	}
	if auth.Note != "" {
		metadata["note"] = auth.Note
	}

	attributes := map[string]string{
		"provider": pluginName,
	}
	if auth.MachineID != "" {
		attributes["machine_id"] = auth.MachineID
	}
	if auth.MacMachineID != "" {
		attributes["mac_machine_id"] = auth.MacMachineID
	}

	// The host normalises StorageJSON to a raw byte slice; ensuring
	// what we hand back parses cleanly is a cheap safety check.
	storage, err := json.Marshal(auth)
	if err != nil {
		return errorEnvelope("marshal_storage", err.Error(), false), 1
	}

	resp := authParseResponse{
		Handled: true,
		Auth: authData{
			Provider:    pluginName,
			ID:          req.Path,
			FileName:    fileName,
			Label:       label,
			Prefix:      auth.Prefix,
			ProxyURL:    auth.ProxyURL,
			Disabled:    auth.Disabled,
			StorageJSON: storage,
			Metadata:    metadata,
			Attributes:  attributes,
		},
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}

// authRefreshRequest mirrors pluginapi.AuthRefreshRequest.
type authRefreshRequest struct {
	AuthID       string            `json:"AuthID"`
	AuthProvider string            `json:"AuthProvider"`
	StorageJSON  []byte            `json:"StorageJSON"`
	Metadata     map[string]any    `json:"Metadata"`
	Attributes   map[string]string `json:"Attributes"`
}

type authRefreshResponse struct {
	Auth             authData  `json:"Auth"`
	NextRefreshAfter time.Time `json:"NextRefreshAfter"`
}

// handleAuthRefresh currently returns the caller's storage unmodified
// with a NextRefreshAfter set for one hour. Wiring up the real Cursor
// refresh call is a follow-up; see docs/phase-7f-plugin-plan.md.
func handleAuthRefresh(payload []byte) ([]byte, int) {
	var req authRefreshRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		return errorEnvelope("bad_request", fmt.Sprintf("parse refresh request: %v", err), false), 1
	}
	auth, err := cpaformat.Unmarshal(req.StorageJSON)
	if err != nil {
		return errorEnvelope("bad_storage", err.Error(), false), 1
	}
	// Passthrough: refresh is not implemented yet. Bump LastRefresh to
	// "now" so operator dashboards show activity but keep the same
	// tokens and identifiers.
	auth.LastRefresh = cpaformat.FormatTime(time.Now())
	storage, err := json.Marshal(auth)
	if err != nil {
		return errorEnvelope("marshal_storage", err.Error(), false), 1
	}
	resp := authRefreshResponse{
		Auth: authData{
			Provider:    pluginName,
			ID:          req.AuthID,
			FileName:    auth.FileName(),
			Label:       auth.Email,
			Prefix:      auth.Prefix,
			ProxyURL:    auth.ProxyURL,
			Disabled:    auth.Disabled,
			StorageJSON: storage,
			Metadata:    req.Metadata,
			Attributes:  req.Attributes,
		},
		NextRefreshAfter: time.Now().Add(1 * time.Hour),
	}
	buf, err := json.Marshal(resp)
	if err != nil {
		return errorEnvelope("marshal_response", err.Error(), false), 1
	}
	return okEnvelopeJSON(string(buf)), 0
}
