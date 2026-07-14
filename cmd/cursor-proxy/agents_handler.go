package main

// /v1/agents/* — the "agent mode" HTTP surface introduced in the
// SDK integration (docs/sdk-integration.md § HTTP surface). Wraps
// executor/sdk.Supervisor in the same shape @cursor/sdk's own
// TypeScript API exposes (Agent + Run split), so downstream
// callers who read the SDK docs can transfer that mental model
// here.
//
// Auth: these endpoints go BEHIND -api-keys (same as /v1/messages).
// That means /v1/agents is a stronger requirement to reach than
// /v1/proxy-info / /v1/capabilities / /v1/introspect — you need
// both a proxy API key AND (per request) a live sdk.Supervisor.
//
// Availability: agent mode is optional. If the operator didn't
// configure a Node runner + CURSOR_API_KEY at spawn, agentSupervisor
// stays nil and every /v1/agents/* handler returns 503 with a
// helpful body. cursor2api probes /v1/proxy-info's agent_mode
// field to decide whether to render agent UI at all.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/executor/sdk"
)

// -------- shared bookkeeping --------

// agentSupervisor is the process-wide sdk.Supervisor (or nil when
// agent mode is off). Set by main() before mux registration; read
// by every /v1/agents/* handler through the accessor to avoid
// data races on late startup.
var agentSupervisor *sdk.Supervisor

// setAgentSupervisor is called from main() after (and only after) a
// successful sdk.New(...).Start(). Passing nil disables agent mode.
func setAgentSupervisor(s *sdk.Supervisor) {
	agentSupervisor = s
}

// requireSupervisor writes a 503 error body when agent mode is off
// and returns nil. Handlers should early-return on nil.
func requireSupervisor(w http.ResponseWriter) *sdk.Supervisor {
	if agentSupervisor == nil {
		writeAgentError(w, http.StatusServiceUnavailable, "agent_mode_disabled",
			"agent mode is not configured on this proxy. "+
				"Configure -cursor-api-key (or CURSOR_API_KEY env) and a Node runner path, "+
				"then restart. See docs/sdk-integration.md.")
		return nil
	}
	return agentSupervisor
}

// -------- error helpers --------

// writeAgentError emits an Anthropic-shaped error body so downstream
// clients that already speak `{type:"error", error:{type, message}}`
// keep working across the wire/agent boundary.
func writeAgentError(w http.ResponseWriter, status int, errType, message string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    errType,
			"message": message,
		},
	})
}

// mapRPCErrorToStatus translates supervisor RPC error codes into HTTP
// statuses. Keeps handlers uniform; if a new code appears on the
// Node side we return 502 (bad gateway) by default rather than
// masquerading as a 200.
func mapRPCErrorToStatus(err error) (int, string, string) {
	var rerr *sdk.RPCError
	if !errors.As(err, &rerr) {
		return http.StatusBadGateway, "sdk_transport_error", err.Error()
	}
	switch rerr.Code {
	case sdk.ErrInvalidParams:
		return http.StatusBadRequest, "invalid_request", rerr.Message
	case sdk.ErrNoAPIKey:
		return http.StatusServiceUnavailable, "agent_mode_no_api_key", rerr.Message
	case sdk.ErrAgentNotFound, sdk.ErrRunNotFound:
		return http.StatusNotFound, "not_found", rerr.Message
	case sdk.ErrSDKFailure:
		return http.StatusBadGateway, "sdk_upstream_error", rerr.Message
	case sdk.ErrMethodNotFound:
		// Version drift between Go supervisor and Node runner. Report
		// as 500 so ops notices; the user can't fix this from outside.
		return http.StatusInternalServerError, "sdk_protocol_mismatch", rerr.Message
	default:
		return http.StatusBadGateway, "sdk_error", rerr.Message
	}
}

// -------- helpers --------

// requestTimeout caps how long we wait for a supervisor call to
// return. SSE streaming has a separate, unbounded budget on the
// caller's ctx.
const agentUnaryTimeout = 30 * time.Second

// pathTail returns the segment after the given prefix, stripped of
// leading/trailing slashes. Empty string when the path is exactly
// the prefix.
func pathTail(path, prefix string) string {
	rest := strings.TrimPrefix(path, prefix)
	return strings.Trim(rest, "/")
}

// -------- /v1/agents dispatch --------

// agentsRootHandler dispatches on method: GET → list, POST → create.
// Go 1.22+ mux would let us split these with `GET /v1/agents` and
// `POST /v1/agents` but a single-registration dispatch keeps the
// endpoint list in main.go shorter.
func agentsRootHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		agentsListHandler(w, r)
	case http.MethodPost:
		agentsCreateHandler(w, r)
	default:
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"GET (list) or POST (create) required on /v1/agents")
	}
}

// -------- POST /v1/agents (create) --------

// createAgentRequest is the HTTP shape mirroring
// sdk.AgentCreateParams with JSON tags for the request body.
type createAgentRequest struct {
	Runtime      string            `json:"runtime"`
	Model        sdk.ModelSelection `json:"model"`
	CWD          string            `json:"cwd,omitempty"`
	Repos        []sdk.CloudRepo   `json:"repos,omitempty"`
	AutoCreatePR bool              `json:"auto_create_pr,omitempty"`
	EnvVars      map[string]string `json:"env_vars,omitempty"`
}

func agentsCreateHandler(w http.ResponseWriter, r *http.Request) {
	sup := requireSupervisor(w)
	if sup == nil {
		return
	}
	if r.Method != http.MethodPost {
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"POST is required to create an agent")
		return
	}
	var req createAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeAgentError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("bad JSON body: %v", err))
		return
	}
	ctx, cancel := contextWithTimeout(r.Context(), agentUnaryTimeout)
	defer cancel()
	result, err := sup.CreateAgent(ctx, sdk.AgentCreateParams{
		Runtime:      req.Runtime,
		Model:        req.Model,
		CWD:          req.CWD,
		Repos:        req.Repos,
		AutoCreatePR: req.AutoCreatePR,
		EnvVars:      req.EnvVars,
	})
	if err != nil {
		status, typ, msg := mapRPCErrorToStatus(err)
		writeAgentError(w, status, typ, msg)
		return
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// -------- GET /v1/agents (list) --------

func agentsListHandler(w http.ResponseWriter, r *http.Request) {
	sup := requireSupervisor(w)
	if sup == nil {
		return
	}
	ctx, cancel := contextWithTimeout(r.Context(), agentUnaryTimeout)
	defer cancel()
	result, err := sup.ListAgents(ctx)
	if err != nil {
		status, typ, msg := mapRPCErrorToStatus(err)
		writeAgentError(w, status, typ, msg)
		return
	}
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// -------- GET /v1/agents/{id} (status) + DELETE (close) --------

func agentsItemHandler(w http.ResponseWriter, r *http.Request) {
	sup := requireSupervisor(w)
	if sup == nil {
		return
	}
	agentID := r.PathValue("id")
	if agentID == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "empty agent id")
		return
	}
	ctx, cancel := contextWithTimeout(r.Context(), agentUnaryTimeout)
	defer cancel()

	switch r.Method {
	case http.MethodGet:
		result, err := sup.StatusAgent(ctx, agentID)
		if err != nil {
			status, typ, msg := mapRPCErrorToStatus(err)
			writeAgentError(w, status, typ, msg)
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(result)
	case http.MethodDelete:
		if err := sup.CloseAgent(ctx, agentID); err != nil {
			// If the agent's already gone, treat as success — DELETE
			// is idempotent. Callers who want strict semantics can
			// GET /v1/agents/{id} first.
			var rerr *sdk.RPCError
			if errors.As(err, &rerr) && rerr.Code == sdk.ErrAgentNotFound {
				w.Header().Set("content-type", "application/json")
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "already_gone": true})
				return
			}
			status, typ, msg := mapRPCErrorToStatus(err)
			writeAgentError(w, status, typ, msg)
			return
		}
		w.Header().Set("content-type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	default:
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"only GET (status) and DELETE (close) are supported on this path")
	}
}

// -------- POST /v1/agents/{id}/runs (send) --------

type sendRunRequest struct {
	Prompt string `json:"prompt"`
	Stream bool   `json:"stream"`
}

type sendRunResponse struct {
	RunID string `json:"run_id"`
}

func agentsSendHandler(w http.ResponseWriter, r *http.Request) {
	sup := requireSupervisor(w)
	if sup == nil {
		return
	}
	if r.Method != http.MethodPost {
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"POST is required to start a run")
		return
	}
	agentID := r.PathValue("id")
	if agentID == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "empty agent id")
		return
	}
	var req sendRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAgentError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("bad JSON body: %v", err))
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_request",
			"prompt is required and must be non-empty")
		return
	}

	// Send is a small RPC; the actual work happens on the returned
	// channel. Cap only the RPC with our unary timeout.
	rpcCtx, cancel := contextWithTimeout(r.Context(), agentUnaryTimeout)
	runID, events, err := sup.Send(rpcCtx, agentID, req.Prompt)
	cancel()
	if err != nil {
		status, typ, msg := mapRPCErrorToStatus(err)
		writeAgentError(w, status, typ, msg)
		return
	}

	if !req.Stream {
		// Non-streaming: block until the run ends, aggregate events,
		// return a single JSON summary. Bounded by the caller's ctx
		// (no server-side timeout — a long run can legitimately take
		// minutes).
		streamNonStreaming(r.Context(), w, runID, events)
		return
	}

	// Streaming: hand runId back synchronously so the client knows
	// where to reconnect, then flush SSE frames as events arrive.
	w.Header().Set("content-type", "application/json")
	_ = json.NewEncoder(w).Encode(sendRunResponse{RunID: runID})

	// We DON'T proxy the stream over the same connection here — the
	// SDK docs say clients should reconnect via /stream to consume
	// events, matching @cursor/sdk's own agent.send() → run.stream()
	// pattern. The event channel keeps running in the background;
	// closing it happens via run.done / run.error notifications on
	// the supervisor. Note: this means events emitted between now
	// and the /stream connect are dropped unless the caller pipes
	// them via a persistent subscription (future work: buffer).
	go drainToVoid(events)
}

// drainToVoid consumes the event channel until it closes so the
// supervisor's dispatcher doesn't stall on a full buffer. Used when
// the caller of POST /v1/agents/{id}/runs chose stream=true but
// never actually opened /stream — a real client shouldn't do this
// but a probe / misconfiguration might.
func drainToVoid(events <-chan sdk.RunStreamMsg) {
	for range events {
	}
}

// -------- GET /v1/agents/{id}/runs/{run_id}/stream (SSE) --------

func agentsStreamHandler(w http.ResponseWriter, r *http.Request) {
	sup := requireSupervisor(w)
	if sup == nil {
		return
	}
	if r.Method != http.MethodGet {
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"GET is required for the SSE stream")
		return
	}
	_ = r.PathValue("id") // agentId is validated on send; here we
	                      // only need the runId.
	runID := r.PathValue("run_id")
	if runID == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "empty run id")
		return
	}
	// Note: the current design does NOT support subscribing to a
	// run by id after the fact — the event channel was handed to
	// whoever called Send(). This handler is a placeholder for the
	// two-step API (POST /runs returns runId, then GET /stream
	// subscribes). A follow-up in Phase 4 will add supervisor-side
	// event buffering so /stream can replay events that arrived
	// between the POST response and the /stream connect. For MVP,
	// clients should use stream=true in the POST body when they
	// want SSE, and consume the same connection that returned the
	// runId inline (see the "combined" endpoint below).
	writeAgentError(w, http.StatusNotImplemented, "not_implemented",
		"Standalone /stream reconnect is not yet supported. "+
			"Use POST /v1/agents/{id}/runs/stream (combined) instead, or "+
			"POST /v1/agents/{id}/runs with stream=false for a non-streaming call.")
}

// -------- POST /v1/agents/{id}/runs/stream (combined send + SSE) --------

// Combined send+stream: one HTTP request, prompt in the body, SSE in
// the response. This is the shape most clients want because it
// dodges the reconnect race in the two-step API. When Phase 4 adds
// event buffering we'll bring back /stream as the canonical form.
func agentsSendStreamHandler(w http.ResponseWriter, r *http.Request) {
	sup := requireSupervisor(w)
	if sup == nil {
		return
	}
	if r.Method != http.MethodPost {
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"POST is required")
		return
	}
	agentID := r.PathValue("id")
	if agentID == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "empty agent id")
		return
	}
	var req sendRunRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeAgentError(w, http.StatusBadRequest, "invalid_request",
			fmt.Sprintf("bad JSON body: %v", err))
		return
	}
	if strings.TrimSpace(req.Prompt) == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_request",
			"prompt is required")
		return
	}

	rpcCtx, cancel := contextWithTimeout(r.Context(), agentUnaryTimeout)
	runID, events, err := sup.Send(rpcCtx, agentID, req.Prompt)
	cancel()
	if err != nil {
		status, typ, msg := mapRPCErrorToStatus(err)
		writeAgentError(w, status, typ, msg)
		return
	}
	streamSSE(r.Context(), w, runID, events)
}

// -------- POST /v1/agents/{id}/runs/{run_id}/cancel --------

func agentsCancelHandler(w http.ResponseWriter, r *http.Request) {
	sup := requireSupervisor(w)
	if sup == nil {
		return
	}
	if r.Method != http.MethodPost {
		writeAgentError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"POST is required")
		return
	}
	runID := r.PathValue("run_id")
	if runID == "" {
		writeAgentError(w, http.StatusBadRequest, "invalid_request", "empty run id")
		return
	}
	ctx, cancel := contextWithTimeout(r.Context(), agentUnaryTimeout)
	defer cancel()
	if err := sup.CancelRun(ctx, runID); err != nil {
		status, typ, msg := mapRPCErrorToStatus(err)
		writeAgentError(w, status, typ, msg)
		return
	}
	// Cancel is documented idempotent on both sides. 202 Accepted
	// signals "acknowledged, will eventually take effect" — the
	// run channel closes when the cancel actually lands.
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
