package executor

import (
	"net/http"

	"github.com/router-for-me/cursor-proto/auth"
)

// Header set replicating the IDE 3.11.19 capture from 2026-07-13.
// See docs/phase-2-report.md for the 3.10.20 baseline; the checksum
// algorithm and header field list are unchanged in 3.11 (only the
// version/commit/releaseHash values differ, plus a handful of new
// protobuf fields the wire format keeps backward-compatible).
const (
	CursorClientVersion = "3.11.19"
	CursorClientCommit  = "bf249e6efb5b097f23d7e21d7283429f0760b740"
	CursorReleaseHash   = auth.KnownReleaseHash_3_11_19
	UserAgent           = "connect-es/1.6.1"
)

// ApplyCommonHeaders sets every x-cursor-* / connect-* header the IDE sends
// on every request. It expects an already-configured Account (with
// ChecksumSession, SessionID, ClientKey, etc. filled — call FillSessionDefaults
// if you loaded it from JSON).
func ApplyCommonHeaders(req *http.Request, acc *auth.Account, requestID string) {
	req.Header.Set("authorization", "Bearer "+acc.AccessToken)
	req.Header.Set("connect-protocol-version", "1")
	req.Header.Set("user-agent", UserAgent)
	req.Header.Set("accept-encoding", "gzip")

	req.Header.Set("x-cursor-checksum", acc.ChecksumSession)
	req.Header.Set("x-cursor-client-version", CursorClientVersion)
	req.Header.Set("x-cursor-client-commit", CursorClientCommit)
	req.Header.Set("x-cursor-client-type", "ide")
	req.Header.Set("x-cursor-client-os", "darwin")
	req.Header.Set("x-cursor-client-os-version", osVersion())
	req.Header.Set("x-cursor-client-arch", clientArch())
	req.Header.Set("x-cursor-client-device-type", "desktop")
	req.Header.Set("x-cursor-client-layout", "unifiedAgent")
	req.Header.Set("x-cursor-timezone", timezone())
	req.Header.Set("x-cursor-streaming", "true")
	req.Header.Set("x-cursor-config-version", acc.ConfigVersion)
	req.Header.Set("x-session-id", acc.SessionID)
	req.Header.Set("x-request-id", requestID)
	req.Header.Set("x-client-key", acc.ClientKey)
	req.Header.Set("x-ghost-mode", "false")
	req.Header.Set("x-new-onboarding-completed", "false")
}
