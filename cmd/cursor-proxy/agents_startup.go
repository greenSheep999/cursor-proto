package main

// Bootstrap wiring for agent mode. Isolated from main.go so the flag
// / env fallback logic and the "should we start it?" decision live
// in one place, and main.go stays a straight-line startup.

import (
	"context"
	"log"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/executor/sdk"
)

// maybeStartAgentSupervisor returns a running *sdk.Supervisor when
// both a Node runner path and an API key are configured, or nil
// otherwise. Nil means "agent mode disabled" — /v1/agents/* returns
// 503; wire mode is untouched.
//
// Resolution order for each input:
//
//	runnerPath: -node-runner flag > $CURSOR_PROXY_NODE_RUNNER
//	apiKey:     -cursor-api-key flag > $CURSOR_API_KEY
//	nodeBinary: -node-binary flag > `node` on PATH (via os/exec)
//
// A start failure is logged but does NOT kill the process — wire
// mode should still come up. Operators watching the log see the
// exact reason and can decide whether to fix it.
func maybeStartAgentSupervisor(runnerPath, nodeBinary, apiKey string) *sdk.Supervisor {
	runnerPath = strings.TrimSpace(runnerPath)
	if runnerPath == "" {
		runnerPath = strings.TrimSpace(os.Getenv("CURSOR_PROXY_NODE_RUNNER"))
	}
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("CURSOR_API_KEY"))
	}

	if runnerPath == "" && apiKey == "" {
		// The typical "wire-only" deployment. Silent — this isn't
		// an error, it's a valid configuration.
		return nil
	}
	if runnerPath == "" {
		log.Printf("[proxy] agent mode: CURSOR_API_KEY is set but -node-runner is empty; agent mode disabled")
		return nil
	}
	if apiKey == "" {
		log.Printf("[proxy] agent mode: -node-runner set but no CURSOR_API_KEY; agent mode disabled")
		return nil
	}

	sup := sdk.New(sdk.Options{
		NodeBinary:     strings.TrimSpace(nodeBinary), // empty ⇒ "node" on PATH
		EntryPath:      runnerPath,
		APIKey:         apiKey,
		RequestTimeout: 30 * time.Second,
		Logger: func(format string, args ...any) {
			log.Printf("[proxy] "+format, args...)
		},
	})

	// Bounded startup: don't hang forever on a broken Node install.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		log.Printf("[proxy] agent mode: node runner failed to start (%v); agent mode disabled", err)
		return nil
	}

	log.Printf("[proxy] agent mode: ready (node runner at %s)", runnerPath)
	return sup
}
