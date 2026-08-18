// Command plugin-http-harness exposes the CPA plugin kernel over a local
// Anthropic-compatible HTTP endpoint.
//
// cmd/cursor-proxy and plugin/cursor/kernel are two different surfaces that
// share only executor/ and translator/. Production traffic (New API -> CPA ->
// cursor plugin) runs the kernel path, so conformance probes aimed at the
// proxy can pass while the deployed path fails. This harness closes that gap:
// it drives kernel.Dispatch exactly as CPA's cgo host does, then renders the
// host stream bridge chunks as an SSE response, so one probe can be pointed at
// either surface and the results diffed.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/plugin/cursor/kernel"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable"`
}

type executorResponse struct {
	Payload []byte `json:"Payload,omitempty"`
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8401", "listen address")
	authFile := flag.String("auth-file", defaultAuthFilePath(), "CPA auth JSON from cursor-export")
	upstreamProxy := flag.String("upstream-proxy", "", "proxy URL persisted into the CPA auth storage")
	flag.Parse()

	storage, email, err := loadStorage(*authFile, *upstreamProxy)
	if err != nil {
		log.Fatalf("load account: %v", err)
	}
	log.Printf("[harness] account: %s", email)

	if raw, rc := kernel.Dispatch("plugin.register", nil, nil); rc != 0 {
		log.Fatalf("plugin.register rc=%d: %s", rc, raw)
	}

	// New API does not currently route this path to the plugin, so exposing it
	// here is what lets a probe exercise kernel's executor.count_tokens.
	http.HandleFunc("/v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		request, _ := json.Marshal(map[string]any{
			"AuthID":       "harness-" + email,
			"AuthProvider": "cursor",
			"Format":       "claude",
			"Payload":      body,
			"StorageJSON":  storage,
		})
		raw, rc := kernel.Dispatch("executor.count_tokens", request, nil)
		if rc != 0 {
			writeError(w, http.StatusBadGateway, string(raw))
			return
		}
		var env envelope
		if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
			writeError(w, http.StatusBadGateway, envMessage(env))
			return
		}
		var response executorResponse
		if err := json.Unmarshal(env.Result, &response); err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write(response.Payload)
	})

	http.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody(r)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		var probe struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		_ = json.Unmarshal(body, &probe)

		request := map[string]any{
			"AuthID":       "harness-" + email,
			"AuthProvider": "cursor",
			"Model":        probe.Model,
			"Format":       "claude",
			"Stream":       probe.Stream,
			"Payload":      body,
			"StorageJSON":  storage,
			"stream_id":    fmt.Sprintf("harness-%d", time.Now().UnixNano()),
		}
		encoded, _ := json.Marshal(request)

		if probe.Stream {
			serveStream(w, encoded)
			return
		}
		serveUnary(w, encoded)
	})

	log.Printf("[harness] listening on http://%s (plugin kernel path)", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

// serveStream mirrors CPA's host stream bridge: the plugin returns
// synchronously and then pushes chunks through the emitter callback.
//
// Chunks are forwarded and flushed as they arrive rather than collected
// first. Buffering would hide the plugin's real time-to-first-byte, which is
// the property a CLI client reacts to.
func serveStream(w http.ResponseWriter, request []byte) {
	state := &streamState{done: make(chan struct{}), chunks: make(chan []byte, 64)}
	raw, rc := kernel.Dispatch("executor.execute_stream", request, state.emit)
	if rc != 0 {
		writeError(w, http.StatusBadGateway, string(raw))
		return
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !env.OK {
		writeError(w, http.StatusBadGateway, envMessage(env))
		return
	}

	flusher, _ := w.(http.Flusher)
	committed := false
	deadline := time.After(300 * time.Second)
	for {
		select {
		case chunk := <-state.chunks:
			if !committed {
				w.Header().Set("content-type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				committed = true
			}
			_, _ = w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		case <-state.done:
			// Drain anything emitted just before close.
			for {
				select {
				case chunk := <-state.chunks:
					if !committed {
						w.Header().Set("content-type", "text/event-stream")
						w.WriteHeader(http.StatusOK)
						committed = true
					}
					_, _ = w.Write(chunk)
				default:
					state.mu.Lock()
					closeErr := state.closeErr
					state.mu.Unlock()
					// A pre-commit failure closes the bridge with an error and
					// zero chunks; CPA turns that into a retryable HTTP error,
					// so the harness must not present it as an empty 200.
					if !committed && closeErr != "" {
						writeError(w, http.StatusBadGateway, closeErr)
						return
					}
					if closeErr != "" {
						_, _ = fmt.Fprintf(w, "\n: host-close-error %s\n\n", closeErr)
					}
					if flusher != nil {
						flusher.Flush()
					}
					return
				}
			}
		case <-deadline:
			if !committed {
				writeError(w, http.StatusGatewayTimeout, "plugin produced no stream close within 300s")
			}
			return
		}
	}
}

func serveUnary(w http.ResponseWriter, request []byte) {
	raw, rc := kernel.Dispatch("executor.execute", request, nil)
	if rc != 0 {
		writeError(w, http.StatusBadGateway, string(raw))
		return
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !env.OK {
		writeError(w, http.StatusBadGateway, envMessage(env))
		return
	}
	var response executorResponse
	if err := json.Unmarshal(env.Result, &response); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("content-type", "application/json")
	_, _ = w.Write(response.Payload)
}

type streamState struct {
	mu       sync.Mutex
	chunks   chan []byte
	closeErr string
	done     chan struct{}
}

func (s *streamState) emit(method string, payload []byte) error {
	switch method {
	case "host.stream.emit":
		var req struct {
			Payload []byte `json:"payload"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return err
		}
		s.chunks <- append([]byte(nil), req.Payload...)
	case "host.stream.close":
		var req struct {
			Error string `json:"error"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return err
		}
		s.mu.Lock()
		s.closeErr = req.Error
		s.mu.Unlock()
		select {
		case <-s.done:
		default:
			close(s.done)
		}
	}
	return nil
}

func envMessage(env envelope) string {
	if env.Error == nil {
		return "unknown plugin error"
	}
	return fmt.Sprintf("%s: %s (retryable=%v)", env.Error.Code, env.Error.Message, env.Error.Retryable)
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]any{"type": "api_error", "message": message},
	})
}

func readBody(r *http.Request) ([]byte, error) {
	defer r.Body.Close()
	buf := make([]byte, 0, 4096)
	chunk := make([]byte, 32*1024)
	for {
		n, err := r.Body.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			if err.Error() == "EOF" {
				return buf, nil
			}
			return buf, nil
		}
	}
}

func defaultAuthFilePath() string {
	if p := strings.TrimSpace(os.Getenv("CURSOR_CPA_AUTH_FILE")); p != "" {
		return p
	}
	return "accounts/current.json"
}

func loadStorage(authFilePath, proxyURL string) ([]byte, string, error) {
	if strings.TrimSpace(authFilePath) != "" {
		if raw, err := os.ReadFile(authFilePath); err == nil {
			file, err := cpaformat.Unmarshal(raw)
			if err != nil {
				return nil, "", fmt.Errorf("parse %s: %w", authFilePath, err)
			}
			if proxyURL != "" {
				file.ProxyURL = proxyURL
			}
			encoded, err := json.Marshal(file)
			return encoded, file.Email, err
		}
	}
	return loadStorageFromIDE(proxyURL)
}

func loadStorageFromIDE(proxyURL string) ([]byte, string, error) {
	path := os.Getenv("HOME") + "/Library/Application Support/Cursor/User/globalStorage/state.vscdb"
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, "", err
	}
	defer db.Close()
	var access, email string
	if err := db.QueryRow(
		`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken'`,
	).Scan(&access); err != nil {
		return nil, "", err
	}
	_ = db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/cachedEmail'`).Scan(&email)
	machineID, _ := auth.GetMachineID()
	macMachineID, _ := auth.GetMacMachineID()
	account := &auth.Account{
		Email:        email,
		AccessToken:  access,
		MachineID:    machineID,
		MacMachineID: macMachineID,
		IssuedAt:     time.Now(),
		ProxyURL:     proxyURL,
	}
	file, err := cpaformat.FromAccount(account)
	if err != nil {
		return nil, "", err
	}
	encoded, err := json.Marshal(file)
	return encoded, email, err
}
