package sdk

// Public methods on Supervisor: Ping / CreateAgent / ListAgents /
// StatusAgent / CloseAgent / Send / CancelRun. All go through the
// same call() plumbing in supervisor.go; this file just types the
// method names and payload shapes so callers don't touch JSON.

import (
	"context"
)

// Ping verifies the child is responsive. Cheap to call; useful as a
// liveness probe from the outside (e.g. the HTTP /v1/proxy-info
// handler reports this).
func (s *Supervisor) Ping(ctx context.Context) (*PingResult, error) {
	var r PingResult
	if err := s.call(ctx, "ping", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateAgent asks the runner to spin up a new agent. Blocks until
// the SDK's Agent.create() returns (which involves an HTTP call to
// Cursor's backend for auth verification and, for cloud runtimes,
// VM provisioning).
func (s *Supervisor) CreateAgent(ctx context.Context, p AgentCreateParams) (*AgentCreateResult, error) {
	var r AgentCreateResult
	if err := s.call(ctx, "agent.create", p, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListAgents returns every live agent currently supervised. This
// includes both local and cloud agents; the caller can filter on
// AgentSummary.Runtime.
func (s *Supervisor) ListAgents(ctx context.Context) (*AgentListResult, error) {
	var r AgentListResult
	if err := s.call(ctx, "agent.list", nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// StatusAgent returns metadata about one agent. Returns an RPCError
// with code ErrAgentNotFound if the agentId is unknown.
func (s *Supervisor) StatusAgent(ctx context.Context, agentID string) (*AgentSummary, error) {
	var r AgentSummary
	if err := s.call(ctx, "agent.status", map[string]string{"agentId": agentID}, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// CloseAgent releases an agent and cancels its in-flight runs.
// Idempotent from the caller's perspective — returns ErrAgentNotFound
// for double-close, which callers can safely swallow.
func (s *Supervisor) CloseAgent(ctx context.Context, agentID string) error {
	return s.call(ctx, "agent.close", map[string]string{"agentId": agentID}, nil)
}

// Send starts a new run on an agent and returns (runId, event channel).
//
// The channel receives one RunStreamMsg per SDK event, then closes
// when the run reaches a terminal state (done / error). Callers must
// drain promptly — dropped events are logged but not queued.
//
// Cancellation: closing the caller's ctx does NOT cancel the run
// itself. Use CancelRun(runId) for that. Ctx here only bounds the
// initial agent.send RPC.
func (s *Supervisor) Send(ctx context.Context, agentID, prompt string) (string, <-chan RunStreamMsg, error) {
	var r AgentSendResult
	if err := s.call(ctx, "agent.send", AgentSendParams{AgentID: agentID, Prompt: prompt}, &r); err != nil {
		return "", nil, err
	}
	// Race: agent.send returns synchronously with the runId, and the
	// child may emit the first run.event before we register a
	// subscriber. Protocol.md handles this: runner.ts calls
	// pumpStream() asynchronously and yields to the microtask queue
	// before iterating, so by the time our next JSON write arrives
	// the subscription is already live. Register BEFORE the call
	// would be racy too (no runId yet). We accept the vanishingly
	// small window here; sub-millisecond in practice, and the
	// reader loop's sendRunMsg logs any early drop so it's
	// observable.
	ch := s.registerRun(r.RunID)
	return r.RunID, ch, nil
}

// CancelRun asks the runner to abort a run. Idempotent — always
// returns nil unless the runner is unreachable. The run's event
// channel will close shortly after the cancel takes effect.
func (s *Supervisor) CancelRun(ctx context.Context, runID string) error {
	return s.call(ctx, "run.cancel", map[string]string{"runId": runID}, nil)
}
