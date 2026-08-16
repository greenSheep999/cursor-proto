package executor

import (
	"strings"
	"time"
)

// clientArch matches the captured macOS Cursor IDE fingerprint used by
// ApplyCommonHeaders. Keeping the declared OS, kernel version, architecture,
// shell, and workspace family consistent matters for server-side tools, which
// are gated more strictly than plain text generation.
func clientArch() string {
	return "x64"
}

// osVersion returns a human-ish kernel version string.
// Node.js os.release() returns raw kernel version, e.g. "24.6.0" on macOS.
// We do not actually shell out — the header value only needs to be non-empty
// and plausible. IDE captures show "24.6.0" (darwin), "10.0.22631" (windows).
func osVersion() string {
	return "24.6.0"
}

// timezone returns the IANA name for the local zone.
func timezone() string {
	name, _ := time.Now().Zone()
	// time.Zone() returns short names ("CST"). We want IANA. Fallback to Asia/Shanghai
	// which is the capture value; callers can override.
	if strings.Contains(name, "/") {
		return name
	}
	if tz := time.Local.String(); tz != "" && strings.Contains(tz, "/") {
		return tz
	}
	return "Asia/Shanghai"
}
