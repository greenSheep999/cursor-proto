package sdk

// Integration tests against the real Node runner in ../../node-runner.
// These need `npm run build` to have produced dist/index.js;
// findNodeRunner skips the test with a clear message when it hasn't.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// findNodeRunner returns the path to node-runner/dist/index.js relative
// to this test file's package location, or (empty, skip=true) when
// the compiled runner is missing.
func findNodeRunner(t *testing.T) string {
	t.Helper()
	// Walk up from cwd to find the repo root (has go.mod at root).
	// Tests run from executor/sdk/, so ../.. is the repo root.
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	entry := filepath.Join(root, "node-runner", "dist", "index.js")
	if _, err := os.Stat(entry); err != nil {
		t.Skipf("node runner not built (%s): run `cd node-runner && npm install && npm run build`", entry)
	}
	return entry
}

func newTestSupervisor(t *testing.T, apiKey string) *Supervisor {
	t.Helper()
	sup := New(Options{
		EntryPath:      findNodeRunner(t),
		APIKey:         apiKey,
		RequestTimeout: 10 * time.Second,
		Logger: func(format string, args ...any) {
			t.Logf("[sup] "+format, args...)
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := sup.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = sup.Close() })
	return sup
}

func TestSupervisor_PingReturnsSDKAndNodeVersion(t *testing.T) {
	sup := newTestSupervisor(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := sup.Ping(ctx)
	if err != nil {
		t.Fatalf("ping: %v", err)
	}
	if !r.Pong {
		t.Error("Pong should be true")
	}
	if r.SDKVersion == "" || r.SDKVersion == "unknown" {
		t.Errorf("SDKVersion missing or 'unknown', got %q", r.SDKVersion)
	}
	if r.NodeVersion == "" {
		t.Error("NodeVersion missing")
	}
	if r.ActiveAgents != 0 || r.ActiveRuns != 0 {
		t.Errorf("fresh runner should be empty; got %+v", r)
	}
}

func TestSupervisor_CreateAgentWithoutAPIKeyReturnsErrNoAPIKey(t *testing.T) {
	sup := newTestSupervisor(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := sup.CreateAgent(ctx, AgentCreateParams{
		Runtime: "local",
		CWD:     "/tmp",
		Model:   ModelSelection{ID: "composer-2.5"},
	})
	if err == nil {
		t.Fatal("expected error when apiKey unset")
	}
	var rerr *RPCError
	if !errors.As(err, &rerr) {
		t.Fatalf("expected RPCError, got %T: %v", err, err)
	}
	if rerr.Code != ErrNoAPIKey {
		t.Errorf("expected code=%d (ErrNoAPIKey), got %d (%s)", ErrNoAPIKey, rerr.Code, rerr.Message)
	}
}

func TestSupervisor_CreateAgentInvalidParamsReturnsErrInvalidParams(t *testing.T) {
	sup := newTestSupervisor(t, "test-key-not-real")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Missing cwd for local runtime.
	_, err := sup.CreateAgent(ctx, AgentCreateParams{
		Runtime: "local",
		Model:   ModelSelection{ID: "composer-2.5"},
	})
	if err == nil {
		t.Fatal("expected error for local without cwd")
	}
	var rerr *RPCError
	if !errors.As(err, &rerr) {
		t.Fatalf("expected RPCError, got %T: %v", err, err)
	}
	if rerr.Code != ErrInvalidParams {
		t.Errorf("expected ErrInvalidParams, got %d: %s", rerr.Code, rerr.Message)
	}

	// Bogus runtime.
	_, err = sup.CreateAgent(ctx, AgentCreateParams{
		Runtime: "wat",
		Model:   ModelSelection{ID: "composer-2.5"},
	})
	if err == nil {
		t.Fatal("expected error for bogus runtime")
	}
	if !errors.As(err, &rerr) || rerr.Code != ErrInvalidParams {
		t.Errorf("expected ErrInvalidParams, got %v", err)
	}
}

func TestSupervisor_ListAgentsOnFreshRunnerIsEmpty(t *testing.T) {
	sup := newTestSupervisor(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, err := sup.ListAgents(ctx)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(r.Agents) != 0 {
		t.Errorf("fresh runner should have 0 agents, got %d", len(r.Agents))
	}
}

func TestSupervisor_CancelRunUnknownIsIdempotent(t *testing.T) {
	sup := newTestSupervisor(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := sup.CancelRun(ctx, "no-such-run"); err != nil {
		t.Errorf("cancel of unknown run should succeed (idempotent): %v", err)
	}
}

func TestSupervisor_StoppedChannelClosesOnClose(t *testing.T) {
	sup := newTestSupervisor(t, "")
	// Immediately close and verify Stopped() fires.
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = sup.Close()
	}()
	select {
	case <-sup.Stopped():
		// success
	case <-time.After(5 * time.Second):
		t.Fatal("Stopped() never closed after Close()")
	}
}

func TestSupervisor_UnknownMethodReturnsErrMethodNotFound(t *testing.T) {
	sup := newTestSupervisor(t, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Bypass typed API — send a raw request for a method the runner
	// doesn't implement.
	err := sup.call(ctx, "totally.made.up", nil, nil)
	if err == nil {
		t.Fatal("expected error for unknown method")
	}
	var rerr *RPCError
	if !errors.As(err, &rerr) {
		t.Fatalf("expected RPCError, got %T: %v", err, err)
	}
	if rerr.Code != ErrMethodNotFound {
		t.Errorf("expected ErrMethodNotFound, got %d: %s", rerr.Code, rerr.Message)
	}
}
