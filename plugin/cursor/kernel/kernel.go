// Package kernel implements the CPA plugin's dispatch logic for the
// Cursor provider. It lives in its own package so tests and the E2E
// harness can drive the plugin's handlers directly, without going
// through cgo. The plugin binary at plugin/cursor is a thin cgo shim
// that delegates every ABI call to Dispatch.
package kernel

import (
	"encoding/json"
	"sync"
)

// abiVersion matches CPA's pluginabi.ABIVersion. Kept in sync
// manually so this plugin does not need to import CPA's module.
const abiVersion uint32 = 1

// pluginName is the stable identifier reported to CPA via
// plugin.register, auth.identifier, and executor.identifier.
const pluginName = "cursor"

// envelope is the plugin-side view of pluginabi.Envelope. Duplicated
// here (rather than imported) so this binary depends only on the
// standard library + cursor-proto's own packages.
type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

// StreamEmitter is invoked by the streaming handlers whenever the
// plugin would call the host back. `method` is one of
// "host.stream.emit" or "host.stream.close" and `payload` is the raw
// JSON that would be sent. Returning an error propagates as a stream
// failure.
type StreamEmitter func(method string, payload []byte) error

// hostInvoker is the cgo bridge that main.go registers. When nil (as
// during unit tests without a real host), callHost falls back to any
// per-dispatch emitter installed via Dispatch.
var (
	hostMu      sync.RWMutex
	hostInvoker func(method string, payload []byte) ([]byte, error)
)

// SetHostInvoker registers the real host-callback bridge. Called
// once by the plugin main package at init time. Tests do not call
// this — they use Dispatch's emitter argument instead.
func SetHostInvoker(fn func(method string, payload []byte) ([]byte, error)) {
	hostMu.Lock()
	hostInvoker = fn
	hostMu.Unlock()
}

// dispatchMu serialises test-scoped emitter swaps to keep the
// package-level hostCallInvoker safe under concurrent Dispatch
// calls with distinct emitters.
var dispatchMu sync.Mutex

// hostCallInvoker is the runtime call target for callHost. main.go's
// init sets it to cgoHostInvoke via SetHostInvoker; tests override
// it via Dispatch's emitter parameter.
var hostCallInvoker func(method string, payload []byte) ([]byte, error)

// callHost forwards a plugin→host callback. It prefers the current
// dispatch-scoped invoker (set by Dispatch's emitter) and falls
// back to the process-wide SetHostInvoker registration. Returning
// an error stops the streaming loop and marks the terminal chunk
// with an error.
func callHost(method string, payload []byte) ([]byte, error) {
	if hostCallInvoker != nil {
		return hostCallInvoker(method, payload)
	}
	hostMu.RLock()
	fn := hostInvoker
	hostMu.RUnlock()
	if fn == nil {
		return []byte(`{"ok":true}`), nil
	}
	return fn(method, payload)
}

// Dispatch runs one plugin ABI call. Returns the envelope bytes to
// send back to the host and the "rc" the cgo wrapper should hand
// back to CPA (0 for structured envelopes, non-zero only for
// unrecoverable failures like an unknown method).
//
// When emitter is non-nil, streaming handlers redirect their host
// callbacks to it. Streaming methods run a goroutine that outlives
// the synchronous Dispatch return, so we do NOT restore the
// previous invoker until that goroutine finishes. In practice test
// harnesses install one emitter per stream and wait for
// host.stream.close before making a new Dispatch call, which keeps
// the swap safe.
func Dispatch(method string, payload []byte, emitter StreamEmitter) ([]byte, int) {
	if emitter != nil {
		dispatchMu.Lock()
		hostCallInvoker = func(m string, p []byte) ([]byte, error) {
			if err := emitter(m, p); err != nil {
				return nil, err
			}
			return []byte(`{"ok":true}`), nil
		}
		// Non-streaming methods do not spawn a goroutine, so it is
		// safe to restore synchronously. Streaming methods keep
		// hostCallInvoker installed until the caller invokes
		// ResetHostInvoker (typically after seeing host.stream.close).
		defer dispatchMu.Unlock()
	}
	return dispatch(method, payload)
}

// ResetHostInvoker clears any test-scoped emitter installed via
// Dispatch. Call this after observing host.stream.close if you
// need to run another streaming dispatch with a fresh emitter.
func ResetHostInvoker() {
	dispatchMu.Lock()
	defer dispatchMu.Unlock()
	hostCallInvoker = nil
}

// dispatch routes an ABI method to its handler. Split from Dispatch
// so unit tests can drive the plain switch without touching the
// emitter machinery.
func dispatch(method string, payload []byte) ([]byte, int) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return okEnvelopeJSON(registerResult()), 0
	case "plugin.shutdown":
		return okEnvelopeJSON(`{}`), 0

	case "auth.identifier":
		return okEnvelopeJSON(identifierResult()), 0
	case "auth.parse":
		raw, rc := handleAuthParse(payload)
		// Teach the pool-status registry about accounts as they arrive.
		if rc == 0 {
			var req authParseRequest
			if err := json.Unmarshal(payload, &req); err == nil {
				registerAccountFromParse(req.RawJSON)
			}
		}
		return raw, rc
	case "auth.refresh":
		return handleAuthRefresh(payload)
	case "auth.login.start":
		return handleAuthLoginStart(payload)
	case "auth.login.poll":
		return handleAuthLoginPoll(payload)

	case "executor.identifier":
		return okEnvelopeJSON(identifierResult()), 0
	case "executor.execute":
		return handleExecutorExecute(payload)
	case "executor.execute_stream":
		return handleExecutorExecuteStream(payload)
	case "executor.count_tokens":
		return handleExecutorCountTokens(payload)

	case "model.register", "model.static":
		return okEnvelopeJSON(staticModelsResult()), 0
	case "model.for_auth":
		return handleModelsForAuth(payload)

	case "management.register":
		return okEnvelopeJSON(managementRegisterResult()), 0
	case "management.handle":
		return handleManagement(payload)

	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method, false), 1
	}
}

// okEnvelopeJSON wraps a pre-marshaled result JSON in an envelope.
func okEnvelopeJSON(result string) []byte {
	buf, _ := json.Marshal(envelope{OK: true, Result: json.RawMessage(result)})
	return buf
}

// ErrorEnvelope constructs an OK=false envelope with a code + message.
// Exported so main.go can build error envelopes for cgo edge cases
// (e.g. a nil method pointer) without importing json.
func ErrorEnvelope(code, message string, retryable bool) []byte {
	buf, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message, Retryable: retryable}})
	return buf
}

// errorEnvelope is the internal spelling used by handlers/executor
// code. Kept as an alias so the diff between "was package main" and
// "is package kernel" stays minimal.
func errorEnvelope(code, message string, retryable bool) []byte {
	return ErrorEnvelope(code, message, retryable)
}
