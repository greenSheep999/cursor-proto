// Package ide exposes the local Cursor.app Sand patcher used by cursorctl.
//
// The bundle rules are intentionally kept in the same repository as this
// wrapper.  They are version-sensitive JavaScript transformations, so the
// CLI must not silently fall back to an unrelated globally installed tool.
package ide

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed sand_patch.py
var patchScript []byte

// Result is the patcher's captured process result.
type Result struct {
	Output   string
	ExitCode int
}

// Run executes one read-only or mutating operation against a Cursor bundle.
// setup-all provisions the remote Box relay before installing the local patch;
// provision-box changes only the remote relay and its local descriptor.
func Run(ctx context.Context, operation, appPath string, restart bool) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	operation = strings.TrimSpace(operation)
	switch operation {
	case "status", "plan", "install", "uninstall", "provision-box", "setup-all":
	default:
		return Result{}, fmt.Errorf("sand ide: unsupported operation %q", operation)
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		return Result{}, fmt.Errorf("sand ide: Cursor bundle patching is supported on macOS and Windows only")
	}
	python := strings.TrimSpace(os.Getenv("CURSORCTL_PYTHON"))
	if python == "" {
		python = "python3"
		if runtime.GOOS == "windows" {
			python = "python"
		}
	}
	if _, err := exec.LookPath(python); err != nil {
		return Result{}, fmt.Errorf("sand ide: Python runtime %q not found: %w", python, err)
	}
	tmp, err := os.MkdirTemp("", "cursorctl-sand-ide-")
	if err != nil {
		// Some locked-down runners expose an unwritable system TMPDIR.  Keep
		// the fallback narrow and task-specific; the directory is removed below.
		fallbackRoot := filepath.Join(".cursorctl-tmp")
		if mkdirErr := os.MkdirAll(fallbackRoot, 0o700); mkdirErr == nil {
			tmp, err = os.MkdirTemp(fallbackRoot, "sand-ide-")
		}
	}
	if err != nil {
		return Result{}, fmt.Errorf("sand ide: create temporary workspace: %w", err)
	}
	defer os.RemoveAll(tmp)
	script := filepath.Join(tmp, "sand_patch.py")
	if err := os.WriteFile(script, patchScript, 0o700); err != nil {
		return Result{}, fmt.Errorf("sand ide: materialize patch rules: %w", err)
	}
	args := []string{script}
	if strings.TrimSpace(appPath) != "" {
		args = append(args, "--path", appPath)
	}
	args = append(args, operation)
	cmd := exec.CommandContext(ctx, python, args...)
	cmd.Env = append(os.Environ(), "CURSORCTL_SILENT=1")
	if !restart {
		cmd.Env = append(cmd.Env, "SAND_PATCH_SKIP_RESTART=1")
	} else {
		cmd.Env = append(cmd.Env, "SAND_PATCH_SKIP_RESTART=")
	}
	out, runErr := cmd.CombinedOutput()
	result := Result{Output: string(out), ExitCode: 0}
	if runErr == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	return result, fmt.Errorf("sand ide: run patcher: %w", runErr)
}
