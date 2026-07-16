// login_panel.go — HTML "Add Cursor account" panel served under the
// CPA plugin resource path. The kiro plugin (kernel/login_page.html +
// kernel/management.go) is the reference implementation; we mirror it.
//
// Wire shape:
//
//	GET  /login              — HTML page (Menu-tagged, unauth-exposed)
//	POST /login/start        — thin wrapper → handleAuthLoginStart
//	POST /login/poll         — thin wrapper → handleAuthLoginPoll
//
// The HTML file is embedded at build time so the .so is self-contained.
// The panel JS calls the /login/start and /login/poll endpoints under
// /v0/management/... using the caller's existing management session
// (Caddy basic auth + CPA secret-key), so no extra auth plumbing is
// needed on our side.
package kernel

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
)

//go:embed login_page.html
var loginPageHTML []byte

// serveLoginPage returns the embedded HTML panel. Cache-Control:
// no-store because the panel embeds JS that talks to versioned APIs;
// letting a CDN or the browser stash a stale page across a plugin
// rebuild would leave callers hitting new backends with old JS.
func serveLoginPage() managementResponse {
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers: map[string][]string{
			"Content-Type":  {"text/html; charset=utf-8"},
			"Cache-Control": {"no-store"},
		},
		Body: loginPageHTML,
	}
}

// loginStartWireRequest is the shape the panel POSTs to /login/start.
// It matches authLoginStartRequest closely so we can just re-marshal
// it and hand it to handleAuthLoginStart, letting the existing
// dispatch/mode-selection logic run unchanged.
type loginStartWireRequest struct {
	Provider string         `json:"Provider"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

// handleLoginStartHTTP is invoked by routeManagement for
// POST /login/start. Delegates to handleAuthLoginStart so all three
// modes (oauth / ide / otp) reuse the same code path CPA hits over
// the plugin ABI. That is the important invariant — the browser
// panel and CPA's own login harness must not diverge.
func handleLoginStartHTTP(body []byte) managementResponse {
	if len(body) == 0 {
		return jsonErrorResponse(http.StatusBadRequest, "empty_body",
			"POST body is required (JSON with Provider + Metadata)")
	}
	// Validate the caller sent something we can decode — this
	// mirrors handleAuthLoginStart's own decode but gives a clearer
	// HTTP-level error to the panel JS.
	var probe loginStartWireRequest
	if err := json.Unmarshal(body, &probe); err != nil {
		return jsonErrorResponse(http.StatusBadRequest, "bad_json",
			"decode start request: "+err.Error())
	}
	if strings.TrimSpace(probe.Provider) == "" {
		probe.Provider = pluginName
	}
	// Re-marshal so Provider defaulting propagates into the ABI call.
	buf, err := json.Marshal(probe)
	if err != nil {
		return jsonErrorResponse(http.StatusInternalServerError, "marshal", err.Error())
	}
	envRaw, rc := handleAuthLoginStart(buf)
	return abiEnvelopeToHTTP(envRaw, rc)
}

// loginPollWireRequest is what the panel POSTs to /login/poll.
type loginPollWireRequest struct {
	Provider string         `json:"Provider"`
	State    string         `json:"State"`
	Metadata map[string]any `json:"Metadata,omitempty"`
}

// handleLoginPollHTTP invokes handleAuthLoginPoll. The panel JS
// polls this every ~1.5-2s while a login flow is in flight; the
// plugin's OTP flow may return Status=pending for many seconds.
func handleLoginPollHTTP(body []byte) managementResponse {
	if len(body) == 0 {
		return jsonErrorResponse(http.StatusBadRequest, "empty_body",
			"POST body is required (JSON with State)")
	}
	var probe loginPollWireRequest
	if err := json.Unmarshal(body, &probe); err != nil {
		return jsonErrorResponse(http.StatusBadRequest, "bad_json",
			"decode poll request: "+err.Error())
	}
	if strings.TrimSpace(probe.Provider) == "" {
		probe.Provider = pluginName
	}
	buf, err := json.Marshal(probe)
	if err != nil {
		return jsonErrorResponse(http.StatusInternalServerError, "marshal", err.Error())
	}
	envRaw, rc := handleAuthLoginPoll(buf)
	return abiEnvelopeToHTTP(envRaw, rc)
}

// abiEnvelopeToHTTP unwraps the plugin ABI envelope produced by
// handleAuthLoginStart / handleAuthLoginPoll (they return
// {"ok":..., "result":..., "error":...}) and forwards the inner
// payload as the HTTP response the panel JS expects. On rc != 0 we
// surface the error body verbatim with 4xx/5xx so the JS can
// display error.message.
func abiEnvelopeToHTTP(env []byte, rc int) managementResponse {
	// The ABI envelope shape used by errorEnvelope / okEnvelope in
	// this package wraps the payload — decode it here so the panel
	// never sees the outer wrapper.
	var wrapper struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result,omitempty"`
		Error  json.RawMessage `json:"error,omitempty"`
	}
	if err := json.Unmarshal(env, &wrapper); err != nil {
		// Not the shape we expected — return raw so the operator can
		// still see what happened.
		status := http.StatusOK
		if rc != 0 {
			status = http.StatusBadGateway
		}
		return managementResponse{
			StatusCode: status,
			Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
			Body:       env,
		}
	}
	if rc != 0 || !wrapper.OK {
		status := http.StatusBadRequest
		if rc != 0 {
			status = http.StatusBadGateway
		}
		return managementResponse{
			StatusCode: status,
			Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
			Body:       wrapErrorBody(wrapper.Error),
		}
	}
	body := wrapper.Result
	if len(body) == 0 {
		body = []byte("{}")
	}
	return managementResponse{
		StatusCode: http.StatusOK,
		Headers:    map[string][]string{"Content-Type": {"application/json; charset=utf-8"}},
		Body:       body,
	}
}

// wrapErrorBody normalises the ABI error object into a
// {"error":{"code","message"}} shape the panel JS reads.
func wrapErrorBody(errRaw json.RawMessage) []byte {
	if len(errRaw) == 0 {
		return []byte(`{"error":{"code":"unknown","message":"plugin returned no error payload"}}`)
	}
	out, err := json.Marshal(map[string]json.RawMessage{"error": errRaw})
	if err != nil {
		return []byte(`{"error":{"code":"marshal_error","message":"could not encode error body"}}`)
	}
	return out
}
