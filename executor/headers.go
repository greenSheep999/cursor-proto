package executor

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/router-for-me/cursor-proto/auth"
)

// Header set replicating the IDE 3.16.17 capture from 2026-08-17.
// See docs/phase-2-report.md for the 3.10.20 baseline; the checksum
// algorithm and header field list are unchanged in 3.11 (only the
// version/commit/releaseHash values differ, plus a handful of new
// protobuf fields the wire format keeps backward-compatible).
const (
	CursorClientVersion = "3.16.17"
	CursorClientCommit  = "6b2afae0257df2bb5e1835f15165dc2f0de056b0"
	CursorReleaseHash   = auth.KnownReleaseHash_3_16_17
	UserAgent           = "connect-es/1.6.1"
)

// CursorLine returns the major.minor line this binary belongs to,
// derived from CursorClientVersion (e.g. "3.11" for "3.11.19"). This
// is what cursor2api's `cursor_version_lock` field stores, and what
// the release-tag prefix `cursor<X.Y>/v<semver>` encodes. See
// docs/versioning.md for the two-axis versioning contract.
func CursorLine() string {
	v := CursorClientVersion
	// Return everything up to the second '.' — "3.11.19" → "3.11".
	// Falls back to the raw string on malformed input so a bad constant
	// surfaces loudly through /v1/proxy-info instead of silently.
	first := -1
	for i, r := range v {
		if r == '.' {
			if first == -1 {
				first = i
			} else {
				return v[:i]
			}
		}
	}
	return v
}

// ApplyCommonHeaders sets every x-cursor-* / connect-* header the IDE sends
// on every request. It expects an already-configured Account (with
// ChecksumSession, SessionID, ClientKey, etc. filled — call FillSessionDefaults
// if you loaded it from JSON).
func ApplyCommonHeaders(req *http.Request, acc *auth.Account, requestID string) {
	platform := resolveClientPlatform(acc)
	clientType, clientLayout := resolveClientSurface(acc)
	req.Header.Set("authorization", "Bearer "+acc.AccessToken)
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("user-agent", UserAgent)
	req.Header.Set("accept-encoding", "gzip")

	req.Header.Set("x-cursor-checksum", acc.ChecksumSession)
	req.Header.Set("x-cursor-client-version", CursorClientVersion)
	if acc.InternalUser {
		req.Header.Set("x-cursor-client-commit", CursorClientCommit)
	}
	req.Header.Set("x-cursor-client-type", clientType)
	req.Header.Set("x-cursor-client-os", platform.os)
	if acc.InternalUser && platform.osVersion != "" {
		req.Header.Set("x-cursor-client-os-version", platform.osVersion)
	}
	req.Header.Set("x-cursor-client-arch", platform.arch)
	req.Header.Set("x-cursor-client-device-type", "desktop")
	req.Header.Set("x-cursor-client-layout", clientLayout)
	req.Header.Set("x-cursor-timezone", timezone())
	if teamID := strings.TrimSpace(acc.TeamID); teamID != "" {
		req.Header.Set("x-cursor-team-id", teamID)
	}
	req.Header.Set("x-cursor-config-version", acc.ConfigVersion)
	req.Header.Set("x-session-id", acc.SessionID)
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("x-amzn-trace-id", "Root="+requestID)
	req.Header.Set("traceparent", newTraceparent())
	req.Header.Set("x-client-key", acc.ClientKey)
	req.Header.Set("x-ghost-mode", boolString(acc.PrivacyMode != 0))
	req.Header.Set("x-new-onboarding-completed", "false")
}

func boolString(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func newTraceparent() string {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "00-00000000000000000000000000000000-0000000000000000-00"
	}
	return "00-" + hex.EncodeToString(buf[:16]) + "-" + hex.EncodeToString(buf[16:]) + "-00"
}

func resolveClientSurface(acc *auth.Account) (string, string) {
	clientType := "ide"
	clientLayout := "unifiedAgent"
	if acc == nil {
		return clientType, clientLayout
	}
	if value := strings.TrimSpace(acc.ClientType); value != "" {
		clientType = value
	}
	if value := strings.TrimSpace(acc.ClientLayout); value != "" {
		clientLayout = value
	}
	return clientType, clientLayout
}
