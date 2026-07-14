package main

// /v1/proxy-info reports what Cursor version this binary impersonates,
// so downstream consumers (cursor2api's sidecar supervisor, ops
// dashboards, licence gates) can verify at runtime that they got the
// binary they pinned. Also drives `cursor-proxy -version`.
//
// Response is stable JSON; new fields will be additive. See
// docs/versioning.md for the two-axis versioning model.

import (
	"encoding/json"
	"net/http"
	"sync/atomic"

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

// CurrentProxyInfo returns the info that describes this binary. Reads
// build-time constants plus the runtime HTTP version atomic; safe to
// call from any goroutine.
func CurrentProxyInfo() ProxyInfo {
	return ProxyInfo{
		CursorLine:          executor.CursorLine(),
		ImpersonatedVersion: executor.CursorClientVersion,
		ImpersonatedCommit:  executor.CursorClientCommit,
		ReleaseHash:         auth.KnownReleaseHashFor(executor.CursorClientVersion),
		ProtoVersion:        ProtoVersion,
		HTTPVersion:         currentHTTPVersionString(),
	}
}

func proxyInfoHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("content-type", "application/json")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(CurrentProxyInfo())
}
