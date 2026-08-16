package executor

import (
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
)

type clientPlatform struct {
	os            string
	osVersion     string
	arch          string
	shell         string
	workspacePath string
}

func resolveClientPlatform(acc *auth.Account) clientPlatform {
	profile := defaultClientPlatform()
	if acc == nil {
		return profile
	}
	if value := normalizeClientOS(acc.ClientOS); value != "" {
		profile = defaultClientPlatformForOS(value)
	}
	if value := strings.TrimSpace(acc.ClientOSVersion); value != "" {
		profile.osVersion = value
	}
	if value := normalizeClientArch(acc.ClientArch); value != "" {
		profile.arch = value
	}
	if value := strings.TrimSpace(acc.ClientShell); value != "" {
		profile.shell = value
	}
	if value := strings.TrimSpace(acc.WorkspacePath); value != "" {
		profile.workspacePath = value
	}
	return profile
}

func defaultClientPlatform() clientPlatform {
	return defaultClientPlatformForOS(normalizeClientOS(runtime.GOOS))
}

func defaultClientPlatformForOS(clientOS string) clientPlatform {
	switch clientOS {
	case "darwin":
		return clientPlatform{
			os:            "darwin",
			osVersion:     "24.6.0",
			arch:          defaultClientArch(),
			shell:         firstNonEmpty(os.Getenv("SHELL"), "/bin/zsh"),
			workspacePath: "/Users/Shared/Cursor",
		}
	case "win32":
		return clientPlatform{
			os:            "win32",
			osVersion:     "10.0.22631",
			arch:          defaultClientArch(),
			shell:         firstNonEmpty(os.Getenv("ComSpec"), `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`),
			workspacePath: `C:\Users\Public\Cursor`,
		}
	default:
		return clientPlatform{
			os:            "linux",
			osVersion:     "6.6.0",
			arch:          defaultClientArch(),
			shell:         firstNonEmpty(os.Getenv("SHELL"), "/bin/bash"),
			workspacePath: "/tmp/cursor",
		}
	}
}

func normalizeClientOS(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "darwin", "mac", "macos":
		return "darwin"
	case "win32", "windows":
		return "win32"
	case "linux":
		return "linux"
	default:
		return ""
	}
}

func normalizeClientArch(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "amd64", "x86_64", "x64":
		return "x64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return ""
	}
}

func defaultClientArch() string {
	if arch := normalizeClientArch(runtime.GOARCH); arch != "" {
		return arch
	}
	return "x64"
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// timezone returns the IANA name for the local zone.
func timezone() string {
	name, _ := time.Now().Zone()
	if strings.Contains(name, "/") {
		return name
	}
	if tz := time.Local.String(); tz != "" && strings.Contains(tz, "/") {
		return tz
	}
	return "Asia/Shanghai"
}
