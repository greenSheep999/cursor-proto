package sdk

// Supervisor owns the Node child process, the JSON-RPC framing, and
// the per-run event demux. See doc.go for the top-level design; this
// file is the implementation.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"
)

// Options configures a new Supervisor.
type Options struct {
	// NodeBinary is the path to the `node` executable. Defaults to
	// looking up "node" on PATH.
	NodeBinary string

	// EntryPath is the path to the compiled Node runner's index.js
	// (i.e. node-runner/dist/index.js). Required.
	EntryPath string

	// APIKey is Cursor's dashboard-issued key (crsr_...). Passed to
	// the child as env CURSOR_API_KEY. Empty means the child's
	// agent.create calls will return ErrNoAPIKey — legal for
	// probing / diagnostic use.
	APIKey string

	// Logger receives one line per stderr write from the child and
	// one line per supervisor-level event (spawn / exit / retry).
	// Prefix each line yourself if you want a discriminator; the
	// Supervisor doesn't add one. Nil means log.Printf-equivalent
	// to stderr.
	Logger func(format string, args ...any)

	// RequestTimeout bounds how long a single agent.* / run.* RPC
	// waits for a response before the caller's context should get
	// cancelled. Zero means no timeout (the caller's ctx.Done()
	// is the only bound). 30s is a good default for prod use.
	RequestTimeout time.Duration
}

// Supervisor owns the Node child. Zero value is not usable — call
// New() and then Start().
type Supervisor struct {
	opts Options

	// cmd + pipes are set by Start. They stay nil after Close.
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	// idCounter is the monotonic JSON-RPC request id. Starts at 1;
	// id=0 is reserved for "unsolicited" responses on the Node side.
	idCounter atomic.Uint64

	// pending correlates request id → response channel. Reader
	// goroutine looks up an id and delivers exactly one message,
	// then deletes the entry.
	pendingMu sync.Mutex
	pending   map[uint64]chan *rpcResponse

	// runSubs correlates runId → per-run event channel. agent.send
	// creates the entry before returning to the caller; run.done /
	// run.error close the channel (and remove the entry) on the
	// reader goroutine.
	runsMu  sync.Mutex
	runSubs map[string]chan RunStreamMsg

	// writeMu serializes stdin writes across goroutines. bufio.Writer
	// is not itself concurrency-safe.
	writeMu     sync.Mutex
	writeBuffer *bufio.Writer

	// closed / exit tracking
	closeOnce sync.Once
	closeErr  atomic.Value // stores error
	stopped   chan struct{}
}

// RunStreamMsg is one event on a per-run subscription channel. Exactly
// one of Event, Done, Error is populated. The channel is closed when
// the run reaches a terminal state (Done or Error); readers select
// on the receive to detect completion.
type RunStreamMsg struct {
	Event *RunEvent
	Done  *RunDone
	Error *RunError
}

// New builds a Supervisor without starting it. Call Start() to spawn
// the child. Options are validated in Start, not here.
func New(opts Options) *Supervisor {
	if opts.Logger == nil {
		opts.Logger = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "[sdk-supervisor] "+format+"\n", args...)
		}
	}
	if opts.NodeBinary == "" {
		opts.NodeBinary = "node"
	}
	return &Supervisor{
		opts:    opts,
		pending: map[uint64]chan *rpcResponse{},
		runSubs: map[string]chan RunStreamMsg{},
		stopped: make(chan struct{}),
	}
}

// Start spawns the Node child. Returns once the process is running
// AND a health-check ping has succeeded — so a caller who sees no
// error can immediately issue agent.create requests.
func (s *Supervisor) Start(ctx context.Context) error {
	if s.opts.EntryPath == "" {
		return errors.New("sdk: Options.EntryPath is required")
	}
	if _, err := os.Stat(s.opts.EntryPath); err != nil {
		return fmt.Errorf("sdk: entry not found at %s: %w", s.opts.EntryPath, err)
	}

	// Deliberately NOT exec.CommandContext(ctx, ...): the caller's
	// ctx bounds the *start* operation (spawn + initial ping), not
	// the child process's lifetime. Tying the child to ctx meant a
	// caller who cancelled after Start returned would silently kill
	// the runner. Child lifetime is instead managed by Close() and
	// awaitExit() below.
	cmd := exec.Command(s.opts.NodeBinary, s.opts.EntryPath)
	// Env: passthrough OS env, then set CURSOR_API_KEY (may override
	// an inherited value from the parent's env). NODE_ENV=production
	// silences chatty dev warnings from @cursor/sdk's dependencies.
	env := os.Environ()
	env = append(env, "NODE_ENV=production")
	if s.opts.APIKey != "" {
		env = append(env, "CURSOR_API_KEY="+s.opts.APIKey)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("sdk: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("sdk: stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("sdk: stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("sdk: start node: %w", err)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = stdout
	s.stderr = stderrPipe
	s.writeBuffer = bufio.NewWriter(stdin)

	// Two goroutines: one reads stdout (JSON-RPC), one reads stderr
	// (unstructured logs) and forwards to the caller's Logger.
	go s.readStdout()
	go s.readStderr()
	go s.awaitExit()

	// Ping the child to confirm it's up before returning. Uses a
	// bounded timeout so a broken runner doesn't hang the caller
	// forever.
	pingCtx := ctx
	if s.opts.RequestTimeout > 0 {
		var cancel context.CancelFunc
		pingCtx, cancel = context.WithTimeout(ctx, s.opts.RequestTimeout)
		defer cancel()
	}
	if _, err := s.Ping(pingCtx); err != nil {
		// Health check failed — kill the child so we don't leak it.
		_ = s.Close()
		return fmt.Errorf("sdk: initial ping failed: %w", err)
	}
	s.opts.Logger("node runner ready (pid=%d)", cmd.Process.Pid)
	return nil
}

// Close terminates the child and drains goroutines. Idempotent.
// Blocks until the process has exited or a short grace period
// elapses — SIGKILL after 3s if graceful stdin-close doesn't work.
func (s *Supervisor) Close() error {
	s.closeOnce.Do(func() {
		// Closing stdin signals the Node runner to shut down.
		if s.stdin != nil {
			_ = s.stdin.Close()
		}
		// Give it a moment to exit gracefully, then SIGKILL.
		exited := make(chan struct{})
		go func() {
			if s.cmd != nil && s.cmd.Process != nil {
				_ = s.cmd.Wait()
			}
			close(exited)
		}()
		select {
		case <-exited:
		case <-time.After(3 * time.Second):
			if s.cmd != nil && s.cmd.Process != nil {
				_ = s.cmd.Process.Kill()
			}
			<-exited
		}
		// Fail all pending requests and close all run streams.
		s.pendingMu.Lock()
		for id, ch := range s.pending {
			// Non-blocking send — pending channels are always
			// buffered 1.
			select {
			case ch <- &rpcResponse{ID: id, Error: &RPCError{
				Code:    ErrInternal,
				Message: "sdk: supervisor closed",
			}}:
			default:
			}
			close(ch)
		}
		s.pending = map[uint64]chan *rpcResponse{}
		s.pendingMu.Unlock()

		s.runsMu.Lock()
		for _, ch := range s.runSubs {
			close(ch)
		}
		s.runSubs = map[string]chan RunStreamMsg{}
		s.runsMu.Unlock()

		close(s.stopped)
	})
	if v := s.closeErr.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// Stopped returns a channel that closes when the supervisor has
// finished shutting down (either by Close or by the child dying).
// Handlers use this to detect "the runner is gone" and 503 their
// requests instead of hanging.
func (s *Supervisor) Stopped() <-chan struct{} {
	return s.stopped
}

// -------- request/response plumbing --------

// call sends one request and waits for its response, honoring the
// caller's context and the supervisor's RequestTimeout.
func (s *Supervisor) call(ctx context.Context, method string, params any, out any) error {
	id := s.idCounter.Add(1)

	respCh := make(chan *rpcResponse, 1)
	s.pendingMu.Lock()
	// Guard against calls after Close — pending has been zeroed.
	if s.pending == nil {
		s.pendingMu.Unlock()
		return errors.New("sdk: supervisor closed")
	}
	s.pending[id] = respCh
	s.pendingMu.Unlock()

	cleanup := func() {
		s.pendingMu.Lock()
		delete(s.pending, id)
		s.pendingMu.Unlock()
	}

	var raw json.RawMessage
	if params != nil {
		b, err := json.Marshal(params)
		if err != nil {
			cleanup()
			return fmt.Errorf("sdk: marshal params: %w", err)
		}
		raw = b
	}
	req := rpcRequest{JSONRPC: "2.0", ID: id, Method: method, Params: raw}
	buf, err := json.Marshal(req)
	if err != nil {
		cleanup()
		return fmt.Errorf("sdk: marshal request: %w", err)
	}

	s.writeMu.Lock()
	_, werr := s.writeBuffer.Write(append(buf, '\n'))
	if werr == nil {
		werr = s.writeBuffer.Flush()
	}
	s.writeMu.Unlock()
	if werr != nil {
		cleanup()
		return fmt.Errorf("sdk: write to child: %w", werr)
	}

	// Wait for the reply.
	waitCtx := ctx
	if s.opts.RequestTimeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, s.opts.RequestTimeout)
		defer cancel()
	}
	select {
	case resp := <-respCh:
		cleanup()
		if resp == nil {
			// Channel closed without a real reply → supervisor died.
			return errors.New("sdk: child died before responding")
		}
		if resp.Error != nil {
			return resp.Error
		}
		if out != nil && len(resp.Result) > 0 {
			if err := json.Unmarshal(resp.Result, out); err != nil {
				return fmt.Errorf("sdk: unmarshal result of %s: %w", method, err)
			}
		}
		return nil
	case <-waitCtx.Done():
		cleanup()
		return waitCtx.Err()
	case <-s.stopped:
		cleanup()
		return errors.New("sdk: supervisor stopped")
	}
}

// -------- reader goroutines --------

// readStdout is the single reader of the child's stdout. It parses
// JSON-RPC frames and dispatches:
//
//   - Responses (with matching pending id): delivered onto the id's channel.
//   - Notifications (run.event / run.done / run.error): demuxed onto
//     the runId's channel.
//   - Everything else: logged and dropped.
//
// Reads until stdout hits EOF, then closes s.stopped indirectly via
// awaitExit.
func (s *Supervisor) readStdout() {
	scanner := bufio.NewScanner(s.stdout)
	// Some SDK stream events can be large (tool result blobs, big
	// diff outputs); raise the max buffer from the 64KB default to
	// 4MB. If a single event exceeds this we log and drop it rather
	// than truncating silently.
	buf := make([]byte, 0, 128*1024)
	scanner.Buffer(buf, 4*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}
		var resp rpcResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			s.opts.Logger("bad JSON from child (%d bytes): %v", len(line), err)
			continue
		}
		if resp.Method != "" {
			// Notification.
			s.dispatchNotification(resp.Method, resp.Params)
			continue
		}
		// Response — deliver to pending channel.
		s.pendingMu.Lock()
		ch, ok := s.pending[resp.ID]
		s.pendingMu.Unlock()
		if !ok {
			// Unsolicited or already-timed-out response. Log at low
			// severity and drop.
			s.opts.Logger("unsolicited response id=%d (likely a timed-out request)", resp.ID)
			continue
		}
		// Non-blocking send — channel is buffered 1; if a duplicate
		// arrives we drop it rather than block the reader.
		select {
		case ch <- &resp:
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		s.opts.Logger("stdout scanner error: %v", err)
	}
}

// readStderr logs the child's stderr line by line so panic traces
// and console.error output land in the parent's log stream instead
// of the OS void.
func (s *Supervisor) readStderr() {
	scanner := bufio.NewScanner(s.stderr)
	scanner.Buffer(make([]byte, 0, 4096), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		s.opts.Logger("[node] %s", line)
	}
}

// awaitExit blocks on the child's exit; when it happens we
// asynchronously close ourselves so pending callers see the
// "supervisor stopped" signal.
func (s *Supervisor) awaitExit() {
	if s.cmd == nil {
		return
	}
	err := s.cmd.Wait()
	if err != nil {
		s.opts.Logger("node runner exited: %v", err)
		s.closeErr.Store(fmt.Errorf("sdk: node runner exited: %w", err))
	} else {
		s.opts.Logger("node runner exited cleanly")
	}
	// Close() is idempotent and drains all pending state.
	_ = s.Close()
}

// dispatchNotification demuxes run.event / run.done / run.error onto
// the run's subscription channel. If no channel is registered for
// the runId, we log and drop the event — this is unexpected (agent
// .send should have registered a channel before returning) but not
// fatal.
func (s *Supervisor) dispatchNotification(method string, params json.RawMessage) {
	switch method {
	case "run.event":
		var e RunEvent
		if err := json.Unmarshal(params, &e); err != nil {
			s.opts.Logger("bad run.event params: %v", err)
			return
		}
		s.sendRunMsg(e.RunID, RunStreamMsg{Event: &e})
	case "run.done":
		var d RunDone
		if err := json.Unmarshal(params, &d); err != nil {
			s.opts.Logger("bad run.done params: %v", err)
			return
		}
		s.sendRunMsg(d.RunID, RunStreamMsg{Done: &d})
		s.closeRun(d.RunID)
	case "run.error":
		var re RunError
		if err := json.Unmarshal(params, &re); err != nil {
			s.opts.Logger("bad run.error params: %v", err)
			return
		}
		s.sendRunMsg(re.RunID, RunStreamMsg{Error: &re})
		s.closeRun(re.RunID)
	default:
		s.opts.Logger("unknown notification method: %s", method)
	}
}

func (s *Supervisor) sendRunMsg(runID string, msg RunStreamMsg) {
	s.runsMu.Lock()
	ch, ok := s.runSubs[runID]
	s.runsMu.Unlock()
	if !ok {
		s.opts.Logger("no subscriber for run %s (event dropped)", runID)
		return
	}
	// Try to deliver; if the subscriber's channel is full we still
	// prefer to drop rather than block the reader loop for all runs.
	// Subscribers are expected to drain promptly.
	select {
	case ch <- msg:
	default:
		s.opts.Logger("run %s subscriber slow; dropping event", runID)
	}
}

func (s *Supervisor) closeRun(runID string) {
	s.runsMu.Lock()
	ch, ok := s.runSubs[runID]
	if ok {
		delete(s.runSubs, runID)
	}
	s.runsMu.Unlock()
	if ok {
		close(ch)
	}
}

// registerRun creates a subscription channel for a runId. Called by
// Send() before it returns so the caller can start draining events
// immediately.
func (s *Supervisor) registerRun(runID string) <-chan RunStreamMsg {
	// Buffered so a burst of small events doesn't stall the reader.
	// 256 is enough for typical SDK streams; SSE emit tends to keep up.
	ch := make(chan RunStreamMsg, 256)
	s.runsMu.Lock()
	s.runSubs[runID] = ch
	s.runsMu.Unlock()
	return ch
}
