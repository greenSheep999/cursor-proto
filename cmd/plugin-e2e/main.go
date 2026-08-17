// plugin-e2e drives the cursor CPA plugin through the same ABI
// surface CPA would use, but co-located inside a single Go process.
//
// Why not dlopen the cursor.dylib built with -buildmode=c-shared?
// A Go binary that dlopens another Go binary crashes with runtime
// heap corruption (`bad sweepgen in refill`) because both share the
// same address space but each carries its own Go scheduler / GC.
// CPA works around this in production by living in one Go binary
// and calling the plugin from cgo threads; recreating that here
// meant either building a C harness (a lot of code for one test)
// or refactoring the plugin so its logic is a plain Go package we
// could import.
//
// We chose the second path: plugin/cursor/kernel exposes a
// Dispatch(method, payload, emitter) function that runs the same
// handlers cliproxyPluginCall would invoke, minus the C shim. The
// harness passes a StreamEmitter that records every host.stream.emit
// and host.stream.close call — so the observable ABI behaviour is
// identical to what a native cgo host would see.
package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/plugin/cursor/kernel"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// hostState tracks the emitted chunks and close signal for one
// executor.execute_stream call.
type hostState struct {
	mu       sync.Mutex
	chunks   [][]byte
	closed   bool
	closeErr string
	done     chan struct{}
}

func (s *hostState) emitter(method string, payload []byte) error {
	switch method {
	case "host.stream.emit":
		var req struct {
			StreamID string `json:"stream_id"`
			Payload  []byte `json:"payload"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return err
		}
		s.mu.Lock()
		s.chunks = append(s.chunks, append([]byte(nil), req.Payload...))
		s.mu.Unlock()
	case "host.stream.close":
		var req struct {
			StreamID string `json:"stream_id"`
			Error    string `json:"error"`
		}
		if err := json.Unmarshal(payload, &req); err != nil {
			return err
		}
		s.mu.Lock()
		s.closed = true
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

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func main() {
	dylibPath := flag.String("dylib", "plugin/cursor/cursor.dylib", "output path for the built shared library (a build sanity check; the harness does not dlopen it)")
	skipBuild := flag.Bool("skip-build", false, "skip building the plugin shared library")
	msg := flag.String("msg", "Say hello in one short sentence.", "user message to send")
	model := flag.String("model", "claude-4.5-sonnet", "model to use")
	format := flag.String("format", "openai", "output format: openai or claude")
	timeout := flag.Duration("timeout", 90*time.Second, "overall stream timeout")
	logPath := flag.String("log", "phase-8b-verify.log", "verification log path (overwritten)")
	upstreamProxy := flag.String("upstream-proxy", "", "upstream proxy URL persisted into the CPA auth storage")
	flag.Parse()

	if !*skipBuild {
		if err := buildPlugin(*dylibPath); err != nil {
			log.Fatalf("build plugin: %v", err)
		}
	}

	logFile, err := os.Create(*logPath)
	if err != nil {
		log.Fatalf("open log: %v", err)
	}
	defer logFile.Close()
	logf := func(format string, args ...any) {
		line := fmt.Sprintf(format, args...)
		fmt.Println(line)
		logFile.WriteString(line + "\n")
	}

	if !*skipBuild {
		logf("built plugin at %s (build-only sanity check; ABI is driven via kernel package)", *dylibPath)
	}

	acc := loadAccountFromIDE()
	if *upstreamProxy != "" {
		acc.ProxyURL = *upstreamProxy
	}
	storage, err := marshalStorage(acc)
	if err != nil {
		log.Fatalf("marshal storage: %v", err)
	}
	logf("loaded cursor account: email=%s (access_token len=%d)", acc.Email, len(acc.AccessToken))

	// plugin.register / executor.identifier — sanity checks.
	regRaw, rc := kernel.Dispatch("plugin.register", nil, nil)
	if rc != 0 {
		log.Fatalf("plugin.register rc=%d: %s", rc, string(regRaw))
	}
	var regEnv envelope
	_ = json.Unmarshal(regRaw, &regEnv)
	if !regEnv.OK {
		log.Fatalf("plugin.register error: %+v", regEnv.Error)
	}
	logf("plugin.register: ok")

	identRaw, _ := kernel.Dispatch("executor.identifier", nil, nil)
	var identEnv envelope
	_ = json.Unmarshal(identRaw, &identEnv)
	logf("executor.identifier: %s", string(identEnv.Result))

	modelReq, _ := json.Marshal(map[string]any{
		"AuthID":       "e2e-" + acc.Email,
		"AuthProvider": "cursor",
		"StorageJSON":  storage,
	})
	modelsRaw, rcModels := kernel.Dispatch("model.for_auth", modelReq, nil)
	if rcModels != 0 {
		log.Fatalf("model.for_auth rc=%d: %s", rcModels, string(modelsRaw))
	}
	if !bytes.Contains(modelsRaw, []byte(`"ID":"`+*model+`"`)) {
		log.Fatalf("model.for_auth did not advertise requested model %q: %s", *model, string(modelsRaw))
	}
	logf("model.for_auth: requested model %s advertised", *model)

	// Build the request body and drive executor.execute_stream.
	payload := buildPayload(*format, *model, *msg)
	req := map[string]any{
		"AuthID":       "e2e-" + acc.Email,
		"AuthProvider": "cursor",
		"Model":        *model,
		"Format":       *format,
		"Stream":       true,
		"Payload":      payload,
		"StorageJSON":  storage,
		"stream_id":    "e2e-1",
	}
	body, _ := json.Marshal(req)

	st := &hostState{done: make(chan struct{})}
	logf("dispatching executor.execute_stream (model=%s format=%s)", *model, *format)
	streamRaw, rcStream := kernel.Dispatch("executor.execute_stream", body, st.emitter)
	if rcStream != 0 {
		log.Fatalf("executor.execute_stream rc=%d: %s", rcStream, string(streamRaw))
	}
	var streamEnv envelope
	if err := json.Unmarshal(streamRaw, &streamEnv); err != nil {
		log.Fatalf("decode stream envelope: %v", err)
	}
	if !streamEnv.OK {
		log.Fatalf("executor.execute_stream error: %+v", streamEnv.Error)
	}
	logf("executor.execute_stream returned synchronously; waiting for chunks…")

	select {
	case <-st.done:
	case <-time.After(*timeout):
		log.Fatalf("timed out after %s waiting for stream close", *timeout)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	logf("stream closed (err=%q), %d chunks emitted", st.closeErr, len(st.chunks))
	if st.closeErr != "" {
		log.Fatalf("stream close reported error: %s", st.closeErr)
	}

	assistantText := ""
	sawFinish := false
	sawUsage := false
	for i, chunk := range st.chunks {
		text := string(chunk)
		trimmed := strings.TrimSpace(text)
		logf("  chunk[%d] (%d bytes): %s", i, len(chunk), truncate(trimmed, 240))
		if *format == "openai" {
			if strings.Contains(trimmed, `"finish_reason":"stop"`) {
				sawFinish = true
			}
			if strings.Contains(trimmed, `"prompt_tokens"`) || strings.Contains(trimmed, `"completion_tokens"`) {
				sawUsage = true
			}
			if content := extractOpenAIContent(trimmed); content != "" {
				assistantText += content
			}
		} else {
			if strings.Contains(trimmed, `"stop_reason":"end_turn"`) || strings.Contains(trimmed, `"stop_reason":"tool_use"`) {
				sawFinish = true
			}
			if strings.Contains(trimmed, `"output_tokens"`) || strings.Contains(trimmed, `"input_tokens"`) {
				sawUsage = true
			}
			if content := extractClaudeContent(trimmed); content != "" {
				assistantText += content
			}
		}
	}
	logf("assistant text: %q", assistantText)
	if !sawFinish {
		log.Fatalf("no finish chunk observed")
	}
	if !sawUsage {
		log.Fatalf("no usage chunk observed")
	}
	if strings.TrimSpace(assistantText) == "" {
		log.Fatalf("assistant text was empty")
	}
	logf("PASS: finish_reason=stop and non-empty usage observed")
}

// buildPayload constructs a minimal OpenAI or Anthropic request body.
func buildPayload(format, model, message string) []byte {
	if strings.ToLower(strings.TrimSpace(format)) == "claude" {
		body := map[string]any{
			"model":  model,
			"stream": true,
			"messages": []map[string]any{
				{"role": "user", "content": message},
			},
		}
		buf, _ := json.Marshal(body)
		return buf
	}
	body := map[string]any{
		"model":  model,
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": message},
		},
		"stream_options": map[string]any{"include_usage": true},
	}
	buf, _ := json.Marshal(body)
	return buf
}

// buildPlugin invokes `go build -buildmode=c-shared` so we still
// prove the plugin compiles as the shared library CPA loads at
// runtime, even though we drive it via kernel.Dispatch.
func buildPlugin(dylibPath string) error {
	dir, _ := filepath.Split(dylibPath)
	if dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	cmd := exec.Command("go", "build", "-buildmode=c-shared", "-o", dylibPath, "./plugin/cursor")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// loadAccountFromIDE reads the Cursor IDE's SQLite state for the
// signed-in user's access token. Same helper as cmd/test-chat.
func loadAccountFromIDE() *auth.Account {
	dbPath := os.Getenv("HOME") + "/Library/Application Support/Cursor/User/globalStorage/state.vscdb"
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var access, email string
	if err := db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken'`).Scan(&access); err != nil {
		log.Fatalf("no accessToken: %v", err)
	}
	_ = db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/cachedEmail'`).Scan(&email)
	mid, _ := auth.GetMachineID()
	mac, _ := auth.GetMacMachineID()
	return &auth.Account{
		Email:        email,
		AccessToken:  access,
		MachineID:    mid,
		MacMachineID: mac,
		IssuedAt:     time.Now(),
	}
}

// marshalStorage converts an *auth.Account into the CPA on-disk
// AuthFile bytes the plugin expects.
func marshalStorage(acc *auth.Account) ([]byte, error) {
	file, err := cpaformat.FromAccount(acc)
	if err != nil {
		return nil, err
	}
	return json.Marshal(file)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// extractOpenAIContent accepts both the raw JSON payload units emitted to
// CPA's stream bridge and legacy HTTP-ready `data:` frames.
func extractOpenAIContent(frame string) string {
	var out strings.Builder
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if line == "" || line == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(line), &chunk) == nil && len(chunk.Choices) > 0 {
			out.WriteString(chunk.Choices[0].Delta.Content)
		}
	}
	return out.String()
}

// extractClaudeContent walks every Anthropic event in one host-emitted chunk.
func extractClaudeContent(frame string) string {
	var out strings.Builder
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var chunk struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk) == nil &&
			chunk.Type == "content_block_delta" && chunk.Delta.Type == "text_delta" {
			out.WriteString(chunk.Delta.Text)
		}
	}
	return out.String()
}
