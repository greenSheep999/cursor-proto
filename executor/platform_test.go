package executor

import (
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/cursor-proto/auth"
)

func TestApplyCommonHeadersUsesAccountPlatform(t *testing.T) {
	acc := &auth.Account{
		AccessToken:     "token",
		ClientOS:        "win32",
		ClientOSVersion: "10.0.22631",
		ClientArch:      "x64",
		ClientType:      "glass",
		ClientLayout:    "glass",
		InternalUser:    true,
	}
	req := httptest.NewRequest("POST", "https://api3.cursor.sh/test", nil)

	ApplyCommonHeaders(req, acc, "request-id")

	for header, want := range map[string]string{
		"x-cursor-client-os":         "win32",
		"x-cursor-client-os-version": "10.0.22631",
		"x-cursor-client-arch":       "x64",
		"x-cursor-client-type":       "glass",
		"x-cursor-client-layout":     "glass",
	} {
		if got := req.Header.Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
}

func TestApplyCommonHeadersDefaultsToCapturedIDESurface(t *testing.T) {
	req := httptest.NewRequest("POST", "https://api2.cursor.sh/test", nil)
	ApplyCommonHeaders(req, &auth.Account{AccessToken: "token", PrivacyMode: 1}, "request-id")
	if got := req.Header.Get("x-cursor-client-type"); got != "ide" {
		t.Fatalf("client type = %q, want ide", got)
	}
	if got := req.Header.Get("x-cursor-client-layout"); got != "unifiedAgent" {
		t.Fatalf("client layout = %q, want unifiedAgent", got)
	}
	if got := req.Header.Get("x-ghost-mode"); got != "true" {
		t.Fatalf("ghost mode = %q, want true", got)
	}
	if got := req.Header.Get("x-cursor-client-commit"); got != "" {
		t.Fatalf("normal-user commit header = %q, want omitted", got)
	}
	if got := req.Header.Get("x-cursor-client-os-version"); got != "" {
		t.Fatalf("normal-user os-version header = %q, want omitted", got)
	}
	if got := req.Header.Get("x-amzn-trace-id"); got != "Root=request-id" {
		t.Fatalf("amzn trace = %q, want Root=request-id", got)
	}
	if got := req.Header.Get("traceparent"); len(got) != 55 || got[:3] != "00-" {
		t.Fatalf("traceparent = %q, want W3C trace id", got)
	}
}

func TestResolveClientPlatformDefaultsByOS(t *testing.T) {
	tests := []struct {
		name      string
		clientOS  string
		wantOS    string
		wantPath  string
		wantShell string
	}{
		{name: "macOS", clientOS: "macos", wantOS: "darwin", wantPath: "/Users/Shared/Cursor", wantShell: "/"},
		{name: "Windows", clientOS: "windows", wantOS: "win32", wantPath: `C:\Users\Public\Cursor`, wantShell: `C:\`},
		{name: "Linux", clientOS: "linux", wantOS: "linux", wantPath: "/tmp/cursor", wantShell: "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveClientPlatform(&auth.Account{ClientOS: tt.clientOS})
			if got.os != tt.wantOS {
				t.Errorf("os = %q, want %q", got.os, tt.wantOS)
			}
			if got.workspacePath != tt.wantPath {
				t.Errorf("workspace = %q, want %q", got.workspacePath, tt.wantPath)
			}
			if len(got.shell) < len(tt.wantShell) || got.shell[:len(tt.wantShell)] != tt.wantShell {
				t.Errorf("shell = %q, want prefix %q", got.shell, tt.wantShell)
			}
		})
	}
}

func TestBuildAgentRunRequestUsesAccountEnvironment(t *testing.T) {
	acc := &auth.Account{
		AccessToken:     "token",
		ClientOS:        "win32",
		ClientOSVersion: "10.0.22631",
		ClientArch:      "x64",
		ClientShell:     `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
		WorkspacePath:   `C:\Users\Public\Cursor`,
	}
	client := NewClient(acc)

	got, err := client.buildAgentRunRequest(&ChatRequest{Model: "claude-opus-5", UserMessage: "search", WebSearch: true}, "message-id")
	if err != nil {
		t.Fatalf("buildAgentRunRequest: %v", err)
	}
	env := got.GetAction().GetUserMessageAction().GetRequestContext().GetEnv()
	if env.GetOsVersion() != acc.ClientOSVersion {
		t.Errorf("os_version = %q, want %q", env.GetOsVersion(), acc.ClientOSVersion)
	}
	if env.GetShell() != acc.ClientShell {
		t.Errorf("shell = %q, want %q", env.GetShell(), acc.ClientShell)
	}
	if env.GetProjectFolder() != acc.WorkspacePath {
		t.Errorf("project_folder = %q, want %q", env.GetProjectFolder(), acc.WorkspacePath)
	}
	if paths := env.GetWorkspacePaths(); len(paths) != 1 || paths[0] != acc.WorkspacePath {
		t.Errorf("workspace_paths = %q, want [%q]", paths, acc.WorkspacePath)
	}
}
