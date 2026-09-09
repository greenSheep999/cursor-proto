package kernel

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

const pluginVersion = "0.8.40"

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

// staticModelsResult supplies the shared protocol-line fallback for CPA hosts
// that register model ownership before invoking model.for_auth. Per-account
// live discovery below remains authoritative and automatically accepts future
// catalog changes. The fallback includes flattened variant ids because CPA
// resolves provider ownership before the plugin can normalize a request.
func staticModelsResult() string {
	return modelsResult(executor.StableRoutableModelFallbackIDs())
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
	client, err := newPluginExecutorClient(acc)
	if err != nil {
		return nil, err
	}
	resp, err := client.ListModels()
	if err != nil {
		return nil, err
	}
	return executor.RoutableModelIDs(resp), nil
}

// catalogRetrySchedule spaces retries of a failed live catalog read.
//
// The Chromium sidecar joins CPA's network namespace, so it cannot be ready
// before CPA is: every deploy has a window where AvailableModels is refused.
// CPA registers model shards once at startup, so a single failed read there
// leaves an account advertising nothing and every request answered with
// "unknown provider for model ...". Retrying across that window is what makes
// a restart self-heal instead of needing a second manual restart.
var catalogRetrySchedule = []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}

// lastKnownCatalog remembers the newest successful catalog per auth so a
// transient failure after startup degrades to the previous answer rather than
// silently removing the account from routing.
var lastKnownCatalog sync.Map // authID -> []string

func listModelsForAuthWithRetry(authID string, acc *auth.Account) ([]string, error) {
	var lastErr error
	for attempt := 0; ; attempt++ {
		names, err := listModelsForAuth(acc)
		if err == nil && len(names) > 0 {
			lastKnownCatalog.Store(authID, names)
			return names, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = errEmptyCatalog
		}
		if attempt >= len(catalogRetrySchedule) {
			return nil, lastErr
		}
		time.Sleep(catalogRetrySchedule[attempt])
	}
}

var errEmptyCatalog = errors.New("account catalog is empty")

// cachedCatalog returns the last catalog this auth reported successfully.
func cachedCatalog(authID string) ([]string, bool) {
	value, ok := lastKnownCatalog.Load(authID)
	if !ok {
		return nil, false
	}
	names, ok := value.([]string)
	return names, ok && len(names) > 0
}

func handleModelsForAuth(payload []byte) ([]byte, int) {
	var req authModelRequest
	if err := json.Unmarshal(payload, &req); err != nil {
		// model.static declares provider-wide model ownership. model.for_auth
		// must never reuse that list as an account capability fallback: doing
		// so registers every advanced model against an account whose catalog
		// could not even be read, and CPA will keep routing requests to it.
		return okEnvelopeJSON(modelsResult(nil)), 0
	}
	file, err := cpaformat.Unmarshal(req.StorageJSON)
	if err != nil {
		reportModelForAuthFallback(req.AuthID, "unmarshal_storage", err)
		return okEnvelopeJSON(modelsResult(nil)), 0
	}
	acc, err := file.ToAccount()
	if err != nil {
		reportModelForAuthFallback(req.AuthID, "to_account", err)
		return okEnvelopeJSON(modelsResult(nil)), 0
	}
	names, err := listModelsForAuthWithRetry(req.AuthID, acc)
	if err != nil {
		reason := "list_models"
		if errors.Is(err, errEmptyCatalog) {
			reason = "empty_catalog"
		}
		if cached, ok := cachedCatalog(req.AuthID); ok {
			reportModelForAuthFallback(req.AuthID, reason+"_using_cached", err)
			return okEnvelopeJSON(modelsResult(filterModelsForQuota(req.AuthID, acc, cached))), 0
		}
		reportModelForAuthFallback(req.AuthID, reason, err)
		return okEnvelopeJSON(modelsResult(nil)), 0
	}
	return okEnvelopeJSON(modelsResult(filterModelsForQuota(req.AuthID, acc, names))), 0
}

// reportModelForAuthFallback surfaces the reason CPA's model.for_auth call
// fell back to the static list. Written to stderr (CPA tees plugin stderr
// into its own log) rather than the response envelope so callers cannot
// starve on a partially-populated Models list. AuthID is included so a
// pool with dozens of accounts is trivially diagnosable — the id is the
// same string CPA prints in its own account-error log lines.
func reportModelForAuthFallback(authID, reason string, err error) {
	id := strings.TrimSpace(authID)
	if id == "" {
		id = "<unknown>"
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[cursor-plugin] model.for_auth fallback: auth=%s reason=%s err=%v\n", id, reason, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[cursor-plugin] model.for_auth fallback: auth=%s reason=%s\n", id, reason)
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
