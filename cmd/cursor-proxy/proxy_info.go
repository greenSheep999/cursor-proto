package main

// /v1/proxy-info reports what Cursor version this binary impersonates,
// so downstream consumers (cursor2api's sidecar supervisor, ops
// dashboards, licence gates) can verify at runtime that they got the
// binary they pinned. Also drives `cursor-proxy -version`.
//
// Response is stable JSON; new fields will be additive. See
// docs/versioning.md for the two-axis versioning model.

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/transport"
)

// ProxyInfo is the JSON shape returned by /v1/proxy-info and printed
// by `cursor-proxy -version`.
type ProxyInfo struct {
	// CursorLine is the major.minor Cursor line (e.g. "3.11"). This
	// is what cursor2api's cursor_version_lock field stores.
	CursorLine string `json:"cursor_line"`

	// ImpersonatedVersion is the exact Cursor build this binary
	// impersonates on the wire (e.g. "3.11.19").
	ImpersonatedVersion string `json:"impersonated_version"`

	// ImpersonatedCommit is the Cursor build's commit hash, sent as
	// x-cursor-client-commit on every upstream request.
	ImpersonatedCommit string `json:"impersonated_commit"`

	// ReleaseHash is the fallback machineID segment of x-cursor-checksum
	// (Cursor releaseHash). Per-version; see auth/machineid.go.
	ReleaseHash string `json:"release_hash"`

	// ProtoVersion is this cursor-proto binary's release tag (e.g.
	// "cursor3.11/v0.2.1"). Injected at build time via -ldflags; falls
	// back to "dev" for local go run / go build.
	ProtoVersion string `json:"proto_version"`

	// HTTPVersion is the upstream wire protocol the running process is
	// pinned to — "auto", "http1.1", or "http1.0". Downstream (cursor2api)
	// uses this to confirm its user-facing dropdown matches what the
	// sidecar actually enforces.
	HTTPVersion string `json:"http_version"`

	// WireMode reports whether the classic /v1/messages surface is
	// operational. Almost always true; falsifying it would take an
	// invalid IDE token or an operator explicitly disabling wire
	// mode (not currently supported — the field is here for
	// symmetry with AgentMode).
	WireMode WireModeInfo `json:"wire_mode"`

	// AgentMode reports whether /v1/agents/* is operational. False
	// when the Node runner didn't start (no -node-runner path, no
	// CURSOR_API_KEY, or a startup failure). cursor2api reads this
	// to decide whether to render agent controls in its UI.
	AgentMode AgentModeInfo `json:"agent_mode"`
}

// WireModeInfo describes the /v1/messages surface. AccountEmail
// mirrors the email cursor-proxy loaded from the IDE's state.vscdb
// (or the -token-file JSON); it's already logged at startup, so
// exposing it via /v1/proxy-info is a convenience for cursor2api's
// "signed in as X" card and not new information.
type WireModeInfo struct {
	Available    bool   `json:"available"`
	AccountEmail string `json:"account_email,omitempty"`
}

// AgentModeInfo describes the /v1/agents/* surface. Populated on
// runtime from the supervisor's most recent Ping; falls back to
// stub values when agent mode is off.
type AgentModeInfo struct {
	Available     bool     `json:"available"`
	SDKVersion    string   `json:"sdk_version,omitempty"`
	NodeVersion   string   `json:"node_version,omitempty"`
	Runtimes      []string `json:"runtimes,omitempty"`
	ActiveAgents  int      `json:"active_agents,omitempty"`
	ActiveRuns    int      `json:"active_runs,omitempty"`
}

// currentHTTPVersion holds the runtime -http-version choice. main() sets
// it via SetCurrentHTTPVersion after flag parsing. Storing this in a
// package-level atomic keeps the ProxyInfo handler free of a Client
// pointer, which matches how the other fields (all build-time constants
// or ProtoVersion ldflag) are surfaced.
var currentHTTPVersion atomic.Value // string

// SetCurrentHTTPVersion is called once at startup by main() with the
// operator's resolved -http-version value. Subsequent reads by
// CurrentProxyInfo see it atomically.
func SetCurrentHTTPVersion(v transport.Version) {
	currentHTTPVersion.Store(v.String())
}

func currentHTTPVersionString() string {
	if s, ok := currentHTTPVersion.Load().(string); ok && s != "" {
		return s
	}
	// Pre-init default. Matches transport.Auto's String() and keeps
	// /v1/proxy-info honest even if someone hits it before main()
	// finishes wiring things up.
	return "auto"
}

// wireAccountEmail is set by main() after loading the account so
// /v1/proxy-info can surface it under wire_mode.account_email
// without threading a pointer through every handler.
var wireAccountEmail atomic.Value // string

// SetWireAccountEmail is called from main() once the account is
// loaded. Empty string is legal (headless deployments with only
// -token-file may not have a cachedEmail on file).
func SetWireAccountEmail(email string) {
	wireAccountEmail.Store(email)
}

func wireAccountEmailString() string {
	if s, ok := wireAccountEmail.Load().(string); ok {
		return s
	}
	return ""
}

// currentAgentModeInfo builds the AgentModeInfo portion of ProxyInfo.
// Returns "off" info when the supervisor isn't running. When it IS
// running, does a bounded Ping — this endpoint is not on the hot
// path (cursor2api polls it every 15-30s), so paying ~1ms for a
// fresh count is fine.
func currentAgentModeInfo() AgentModeInfo {
	sup := agentSupervisor
	if sup == nil {
		return AgentModeInfo{Available: false}
	}
	// Bounded so a stalled runner doesn't hang /v1/proxy-info.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	pong, err := sup.Ping(ctx)
	if err != nil {
		// Runner is registered but not answering — treat as
		// unavailable so cursor2api renders the "reconnecting"
		// state rather than "ready".
		return AgentModeInfo{Available: false}
	}
	return AgentModeInfo{
		Available:    true,
		SDKVersion:   pong.SDKVersion,
		NodeVersion:  pong.NodeVersion,
		Runtimes:     []string{"local", "cloud"},
		ActiveAgents: pong.ActiveAgents,
		ActiveRuns:   pong.ActiveRuns,
	}
}

// CurrentProxyInfo returns the info that describes this binary. Reads
// build-time constants, the runtime HTTP version atomic, and the
// supervisor state; safe to call from any goroutine.
func CurrentProxyInfo() ProxyInfo {
	email := wireAccountEmailString()
	return ProxyInfo{
		CursorLine:          executor.CursorLine(),
		ImpersonatedVersion: executor.CursorClientVersion,
		ImpersonatedCommit:  executor.CursorClientCommit,
		ReleaseHash:         auth.KnownReleaseHashFor(executor.CursorClientVersion),
		ProtoVersion:        ProtoVersion,
		HTTPVersion:         currentHTTPVersionString(),
		WireMode: WireModeInfo{
			// Wire mode is always available in current builds; false
			// would need an operator flag we don't (yet) expose.
			Available:    true,
			AccountEmail: email,
		},
		AgentMode: currentAgentModeInfo(),
	}
}

func proxyInfoHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(CurrentProxyInfo())
}
