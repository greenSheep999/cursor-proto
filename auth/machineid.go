package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net"
	"os/exec"
	"regexp"
	"runtime"
	"strings"

	"github.com/google/uuid"
)

// Cursor release hashes. Used as the `machineID` segment of the
// x-cursor-checksum header when the client-side telemetry hasn't been
// initialised yet — the IDE substitutes this value into the checksum
// (see nVg / DZg in workbench.desktop.main.js, depending on version).
//
// Each entry corresponds to the first segment of the update URL:
//
//	https://downloads.cursor.com/production/<releaseHash>/<platform>/...
//
// so it can be re-derived with:
//
//	curl -s "https://api2.cursor.sh/updates/api/update/darwin-arm64/cursor/<prev>/stable" | jq .url
const (
	KnownReleaseHash_3_10_20 = "4071c661bcb367c518becc7b3d4d57cbd69d2291d8b302c558d79080f8fd4f75"
	KnownReleaseHash_3_11_19 = "bf249e6efb5b097f23d7e21d7283429f0760b74a"
	KnownReleaseHash_3_15_19 = "de07bee81cefe43461ebf4f40c3d2d78d15052aa"
	KnownReleaseHash_3_16_17 = "6b2afae0257df2bb5e1835f15165dc2f0de056b2"
	// KnownChecksumMachineID_3_16_17 is the machineId value observed in the
	// 3.16.17 IDE's request-header builder. It intentionally differs from both
	// the 3.16 release hash and the host's stable device MachineID.
	KnownChecksumMachineID_3_16_17 = KnownReleaseHash_3_10_20
)

// KnownReleaseHashFor returns the releaseHash for a specific Cursor
// version, or "" if we don't have one on file. Used by
// /v1/proxy-info and `cursor-proxy -version` to report the exact
// value the running binary uses as the checksum fallback.
func KnownReleaseHashFor(version string) string {
	switch version {
	case "3.10.20":
		return KnownReleaseHash_3_10_20
	case "3.11.19":
		return KnownReleaseHash_3_11_19
	case "3.15.19":
		return KnownReleaseHash_3_15_19
	case "3.16.17":
		return KnownReleaseHash_3_16_17
	default:
		return ""
	}
}

// GetMachineID returns SHA-256(IOPlatformUUID) on macOS, following Cursor's
// _getTrueMachineId() in main.js: `l5(!0)`. On other platforms it uses the
// equivalent platform-specific stable identifier.
//
// This is the value the IDE sends as the `machineID` segment of x-cursor-checksum
// AFTER telemetry has bootstrapped (before that, the pre-baked releaseHash is used).
func GetMachineID() (string, error) {
	raw, err := getPlatformDeviceID()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:]), nil
}

// GetMacMachineID returns SHA-256(<first valid MAC address>).
//
// Cursor's `c5` function in main.js:
//
//	c5(e) = sha256(OJ())
//	OJ()  = first valid MAC address string from os.networkInterfaces()
//
// This is what the IDE sends as the trailing `/<macMachineID>` segment of
// x-cursor-checksum after telemetry initialises.
func GetMacMachineID() (string, error) {
	mac, err := firstValidMAC()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(mac))
	return hex.EncodeToString(sum[:]), nil
}

// firstValidMAC iterates network interfaces and returns the first non-empty,
// non-zero MAC address as an unformatted lowercase string (matching Node.js
// os.networkInterfaces() output which yields e.g. "aa:bb:cc:dd:ee:ff").
func firstValidMAC() (string, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	for _, ifi := range ifs {
		hw := ifi.HardwareAddr
		if len(hw) == 0 {
			continue
		}
		s := hw.String() // "aa:bb:cc:dd:ee:ff"
		if isValidMACString(s) {
			return s, nil
		}
	}
	return "", fmt.Errorf("no valid MAC address found")
}

// isValidMACString replicates Cursor's MJ() sanity check:
// strip dashes, require at least one non-zero digit.
func isValidMACString(s string) bool {
	stripped := strings.NewReplacer("-", "", ":", "").Replace(s)
	if stripped == "" {
		return false
	}
	for _, r := range stripped {
		if r != '0' {
			return true
		}
	}
	return false
}

var iOPlatformUUIDRe = regexp.MustCompile(`"IOPlatformUUID"\s*=\s*"([^"]+)"`)

func getPlatformDeviceID() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return deviceIDDarwin()
	case "linux":
		return deviceIDLinux()
	case "windows":
		return deviceIDWindows()
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}
}

func deviceIDDarwin() (string, error) {
	cmd := exec.Command("ioreg", "-rd1", "-c", "IOPlatformExpertDevice")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("ioreg: %w", err)
	}
	m := iOPlatformUUIDRe.FindSubmatch(out)
	if len(m) < 2 {
		return "", fmt.Errorf("IOPlatformUUID not found in ioreg output")
	}
	return string(m[1]), nil
}

func deviceIDLinux() (string, error) {
	for _, path := range []string{"/etc/machine-id", "/var/lib/dbus/machine-id"} {
		out, err := exec.Command("cat", path).Output()
		if err == nil {
			s := strings.TrimSpace(string(out))
			if s != "" {
				return s, nil
			}
		}
	}
	return "", fmt.Errorf("no machine-id file readable")
}

func deviceIDWindows() (string, error) {
	cmd := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reg query: %w", err)
	}
	re := regexp.MustCompile(`REG_SZ\s+([0-9a-fA-F-]{36})`)
	m := re.FindSubmatch(out)
	if len(m) < 2 {
		return "", fmt.Errorf("MachineGuid not found in registry output")
	}
	return string(m[1]), nil
}

// GenerateSessionID returns a fresh session-level identifier suitable for the
// x-session-id / x-cursor-config-version headers.
func GenerateSessionID() string {
	return uuid.NewString()
}

// GenerateRequestID returns a fresh per-request UUID suitable for x-request-id
// and the middle segment of traceparent / x-amzn-trace-id.
func GenerateRequestID() string {
	return uuid.NewString()
}

// GenerateClientKey returns a fresh 64-char hex value for x-client-key.
// The IDE regenerates this once per session; we do the same.
func GenerateClientKey() string {
	u1 := uuid.NewString()
	u2 := uuid.NewString()
	sum := sha256.Sum256([]byte(u1 + u2))
	return hex.EncodeToString(sum[:])
}
