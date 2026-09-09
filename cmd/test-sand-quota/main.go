// test-sand-quota compares a real request's usage before and after state.
// It is intentionally an explicit probe rather than part of the normal proxy
// path: running it consumes model quota on the selected account.
//
// Usage:
//
//	test-sand-quota -account ~/.cursor-pool/cursor-you_at_example.com.json \
//	  -model grok-4.6 -msg "reply with one word"
//
// The default probe is the existing production shape (RunSSE + IDE headers).
// Direct InferenceService/Stream and Sand headers are explicit experiments:
//
//	test-sand-quota -rpc inference_stream -client-type sand
//
// The command prints GetSandUsageStatus before and after RunChat, then lists
// recent usage events so the model/kind recorded by Cursor can be inspected.
package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/transport"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
	"github.com/router-for-me/cursor-proto/usage"
	"google.golang.org/protobuf/proto"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func main() {
	accountPath := flag.String("account", "", "path to account JSON (default: read Cursor IDE token)")
	model := flag.String("model", "grok-4.6", "model to use; run once per model/header combination")
	message := flag.String("msg", "Reply with one word: probe.", "message sent to the selected model")
	timeout := flag.Duration("timeout", 90*time.Second, "overall request timeout")
	proxyURL := flag.String("upstream-proxy", "", "optional HTTP/HTTPS/SOCKS proxy")
	httpVersion := flag.String("http-version", "auto", "upstream HTTP protocol: auto | http1.1 | http1.0")
	pageSize := flag.Int("events", 20, "number of recent usage events to print")
	rpc := flag.String("rpc", string(executor.ChatRPCModeRunSSE), "upstream RPC: run_sse | inference_stream")
	clientType := flag.String("client-type", "ide", "client surface: ide | sand")
	relayConfigPath := flag.String("relay-config", "", "Box relay JSON; sends inference_stream through the same route as the patched IDE")
	sandClientVersion := flag.String("sand-client-version", executor.SandClientVersion, "Grok Bot client version sent through the Box relay")
	checksumSource := flag.String("checksum-source", "service", "relay checksum identity: service | ide")
	ideCompatible := flag.Bool("ide-compatible", false, "fail if the relay response contains Connect-compressed data frames that Cursor IDE cannot decode")
	flag.Parse()

	var rpcMode executor.ChatRPCMode
	switch strings.ToLower(strings.TrimSpace(*rpc)) {
	case "", string(executor.ChatRPCModeRunSSE):
		rpcMode = executor.ChatRPCModeRunSSE
	case string(executor.ChatRPCModeInferenceStream):
		rpcMode = executor.ChatRPCModeInferenceStream
	default:
		log.Fatalf("invalid -rpc %q (want run_sse or inference_stream)", *rpc)
	}
	clientSurface := strings.ToLower(strings.TrimSpace(*clientType))
	if clientSurface != "ide" && clientSurface != "sand" {
		log.Fatalf("invalid -client-type %q (want ide or sand)", *clientType)
	}

	acc, err := loadAccount(*accountPath)
	if err != nil {
		log.Fatal(err)
	}
	version, err := transport.Parse(*httpVersion)
	if err != nil {
		log.Fatal(err)
	}
	options := []executor.Option{executor.WithHTTPVersion(version)}
	if strings.TrimSpace(*proxyURL) != "" {
		options = append(options, executor.WithProxyURL(strings.TrimSpace(*proxyURL)))
	}
	execClient := executor.NewClient(acc, options...)
	// Regular user chat uses api2 for both the RunSSE and BidiAppend halves.
	execClient.API3 = execClient.API2
	usageClient := usage.New(execClient)

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	started := time.Now().UTC()

	before, err := usageClient.Fetch(ctx)
	if err != nil {
		log.Fatal("baseline usage: ", err)
	}
	printSand("BEFORE", before)

	fmt.Printf("\nREQUEST model=%s rpc=%s client_type=%q message=%q\n", *model, rpcMode, clientSurface, *message)
	if strings.TrimSpace(*relayConfigPath) != "" {
		if rpcMode != executor.ChatRPCModeInferenceStream {
			log.Fatal("-relay-config requires -rpc inference_stream")
		}
		result, err := runBoxRelay(ctx, acc, *relayConfigPath, *model, *message, *sandClientVersion, *checksumSource, *ideCompatible)
		if err != nil {
			log.Fatal("Box relay: ", err)
		}
		fmt.Printf("STREAM closed data_frames=%d compressed_frames=%d end_frame=%v connect_content_encoding=%q response=%q\n",
			result.DataFrames, result.CompressedFrames, result.EndFrame, result.ConnectContentEncoding, summarize(result.Text, 180))
		if *ideCompatible {
			if err := validateIDECompatible(result); err != nil {
				log.Fatal("IDE compatibility: ", err)
			}
			fmt.Println("IDE_COMPATIBLE true")
		}
	} else {
		events, err := execClient.RunChat(ctx, &executor.ChatRequest{
			Model:              *model,
			UserMessage:        *message,
			RPCMode:            rpcMode,
			ClientTypeOverride: clientSurface,
			AutoStopOnTurnEnd:  true,
			AutoStopOnToolCall: true,
		})
		if err != nil {
			log.Fatal("RunChat: ", err)
		}
		count := 0
		for event := range events {
			count++
			if event.Trailer {
				fmt.Printf("  event[%d] trailer=%s\n", count, strings.TrimSpace(string(event.Raw)))
				continue
			}
			if event.Server != nil {
				fmt.Printf("  event[%d] server=%s\n", count, summarize(event.Server.String(), 180))
			} else {
				fmt.Printf("  event[%d] raw_bytes=%d\n", count, len(event.Raw))
			}
		}
		fmt.Printf("STREAM closed events=%d\n", count)
	}

	// The dashboard event index can lag the request by a few seconds. Keep the
	// wait short and bounded so the probe remains useful in CI-like terminals.
	select {
	case <-time.After(2 * time.Second):
	case <-ctx.Done():
	}
	after, err := usageClient.Fetch(ctx)
	if err != nil {
		log.Fatal("after usage: ", err)
	}
	printSand("AFTER", after)
	printDelta(before, after)

	page, err := usageClient.ListEvents(ctx, usage.EventListOptions{
		StartMs:  started.Add(-2 * time.Minute).UnixMilli(),
		EndMs:    time.Now().UTC().Add(2 * time.Minute).UnixMilli(),
		Page:     1,
		PageSize: int32(*pageSize),
	})
	if err != nil {
		fmt.Printf("EVENTS error=%v\n", err)
		return
	}
	fmt.Printf("EVENTS total=%d returned=%d\n", page.TotalCount, len(page.Events))
	for i, event := range page.Events {
		if i >= *pageSize {
			break
		}
		kind := event.GetKind().String()
		charged := event.GetChargedCents()
		fmt.Printf("  event[%d] ts=%s model=%s kind=%s client_type=%s charged_cents=%.2f conversation=%s\n",
			i+1, time.UnixMilli(event.GetTimestamp()).UTC().Format(time.RFC3339), event.GetModel(), kind,
			event.GetClientType(), charged, event.GetConversationId())
	}
}

type boxRelayConfig struct {
	Version   int               `json:"version"`
	BaseURL   string            `json:"baseUrl"`
	Token     string            `json:"token"`
	Headers   map[string]string `json:"headers"`
	RelayPath string            `json:"relayPath"`
	Refresh   *boxRelayRefresh  `json:"refresh,omitempty"`
}

type boxRelayRefresh struct {
	MachineID string `json:"machineId"`
}

type boxRelayResult struct {
	DataFrames             int
	CompressedFrames       int
	EndFrame               bool
	ConnectContentEncoding string
	Text                   string
}

func runBoxRelay(ctx context.Context, acc *auth.Account, configPath, model, message, sandClientVersion, checksumSource string, emulateIDECompression bool) (*boxRelayResult, error) {
	// Server config versions rotate while Cursor is running. The baseline usage
	// call above can take long enough to stale the account loaded at startup, so
	// refresh immediately before constructing the model request.
	if fresh, err := auth.LoadAccountFromIDE(); err == nil && fresh.AccessToken == acc.AccessToken {
		acc = fresh
	}
	if configPath == "auto" {
		configPath = filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "SandClientModeStream", "sand-client-cli", "grok-box-relay.json")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read relay config: %w", err)
	}
	var config boxRelayConfig
	if err := json.Unmarshal(raw, &config); err != nil {
		return nil, fmt.Errorf("decode relay config: %w", err)
	}
	base, err := url.Parse(config.BaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil {
		return nil, fmt.Errorf("relay baseUrl must be an HTTPS origin")
	}
	if config.Token == "" || config.RelayPath != "/sand-stream-relay/aiserver.v1.InferenceService/Stream" {
		return nil, fmt.Errorf("relay config is incomplete or has an unexpected path")
	}
	conversationID := auth.GenerateSessionID()
	conversationGroupID := auth.GenerateSessionID()
	request := &cursorpb.AiserverV1_InferenceStreamRequest{
		ConversationId:      &conversationID,
		ConversationGroupId: &conversationGroupID,
		RequestedModel: &cursorpb.AiserverV1_InferenceRequestedModel{
			ModelId:      model,
			BuiltInModel: true,
		},
		Messages: []*cursorpb.AiserverV1_InferenceCoreMessage{{
			Role:    cursorpb.AiserverV1_INFERENCE_MESSAGE_ROLE_USER,
			Content: &cursorpb.AiserverV1_InferenceCoreMessage_Text{Text: message},
		}},
	}
	payload, err := proto.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	frame, requestContentEncoding, err := buildConnectRequestFrame(payload, emulateIDECompression)
	if err != nil {
		return nil, fmt.Errorf("encode Connect request frame: %w", err)
	}
	endpoint := strings.TrimRight(config.BaseURL, "/") + config.RelayPath
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(frame))
	if err != nil {
		return nil, err
	}
	if err := applyBoxRelayHeaders(httpReq, acc, &config, sandClientVersion, checksumSource); err != nil {
		return nil, err
	}
	httpReq.Header.Set("authorization", "Bearer "+config.Token)
	httpReq.Header.Set("content-type", "application/connect+proto")
	httpReq.Header.Set("accept", "application/connect+proto")
	for name, value := range config.Headers {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "authorization", "host", "content-length", "transfer-encoding":
			continue
		}
		if strings.TrimSpace(value) != "" {
			httpReq.Header.Set(name, value)
		}
	}
	if emulateIDECompression {
		// Cursor 3.19.x negotiates gzip in both directions. The request frame and
		// its encoding declaration must travel together through the relay.
		httpReq.Header.Set("connect-content-encoding", requestContentEncoding)
		httpReq.Header.Set("connect-accept-encoding", "gzip")
	}
	fmt.Printf("RELAY_HEADERS client_version=%s checksum_source=%s config_version=%s commit_present=%v content_type=%s connect_content_encoding=%q connect_accept_encoding=%q\n",
		httpReq.Header.Get("x-cursor-client-version"),
		strings.ToLower(strings.TrimSpace(checksumSource)),
		httpReq.Header.Get("x-cursor-config-version"),
		httpReq.Header.Get("x-cursor-client-commit") != "",
		httpReq.Header.Get("content-type"),
		httpReq.Header.Get("connect-content-encoding"),
		httpReq.Header.Get("connect-accept-encoding"),
	)
	resp, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	reader := io.Reader(resp.Body)
	if strings.EqualFold(resp.Header.Get("content-encoding"), "gzip") {
		gz, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("open gzip response: %w", err)
		}
		defer gz.Close()
		reader = gz
	}
	body, err := io.ReadAll(io.LimitReader(reader, 32<<20+1))
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if len(body) > 32<<20 {
		return nil, fmt.Errorf("relay response exceeds 32 MiB")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d: %s", resp.StatusCode, summarize(string(body), 300))
	}
	result, err := decodeBoxRelayFrames(body)
	if err != nil {
		return nil, err
	}
	result.ConnectContentEncoding = strings.TrimSpace(resp.Header.Get("connect-content-encoding"))
	if !result.EndFrame {
		return nil, fmt.Errorf("stream closed without a Connect end frame")
	}
	if strings.TrimSpace(result.Text) == "" {
		return nil, fmt.Errorf("stream completed without assistant text")
	}
	return result, nil
}

func buildConnectRequestFrame(payload []byte, compress bool) ([]byte, string, error) {
	flags := byte(0)
	contentEncoding := ""
	if compress {
		var compressed bytes.Buffer
		writer := gzip.NewWriter(&compressed)
		if _, err := writer.Write(payload); err != nil {
			return nil, "", err
		}
		if err := writer.Close(); err != nil {
			return nil, "", err
		}
		payload = compressed.Bytes()
		flags = 0x01
		contentEncoding = "gzip"
	}
	frame := make([]byte, 5+len(payload))
	frame[0] = flags
	binary.BigEndian.PutUint32(frame[1:5], uint32(len(payload)))
	copy(frame[5:], payload)
	return frame, contentEncoding, nil
}

func decodeBoxRelayFrames(body []byte) (*boxRelayResult, error) {
	result := &boxRelayResult{}
	var text strings.Builder
	for len(body) > 0 {
		if len(body) < 5 {
			return nil, fmt.Errorf("truncated Connect frame header")
		}
		flags := body[0]
		length := int(binary.BigEndian.Uint32(body[1:5]))
		body = body[5:]
		if length > len(body) {
			return nil, fmt.Errorf("truncated Connect frame payload")
		}
		framePayload := body[:length]
		body = body[length:]
		if flags&0x02 != 0 {
			result.EndFrame = true
			if bytes.Contains(framePayload, []byte(`"error"`)) {
				return nil, fmt.Errorf("Connect end frame reported an error: %s", summarize(string(framePayload), 2000))
			}
			continue
		}
		if flags&0x01 != 0 {
			result.CompressedFrames++
			gz, err := gzip.NewReader(bytes.NewReader(framePayload))
			if err != nil {
				return nil, fmt.Errorf("open compressed frame: %w", err)
			}
			framePayload, err = io.ReadAll(io.LimitReader(gz, 8<<20+1))
			_ = gz.Close()
			if err != nil || len(framePayload) > 8<<20 {
				return nil, fmt.Errorf("decode compressed frame")
			}
		}
		var stream cursorpb.AiserverV1_InferenceStreamResponse
		if err := proto.Unmarshal(framePayload, &stream); err != nil {
			return nil, fmt.Errorf("decode response frame: %w", err)
		}
		result.DataFrames++
		if part := stream.GetTextPart(); part != nil {
			text.WriteString(part.GetText())
		}
		if streamErr := stream.GetError(); streamErr != nil {
			return nil, fmt.Errorf("stream error %s: %s", streamErr.GetCode(), streamErr.GetMessage_())
		}
	}
	result.Text = text.String()
	return result, nil
}

func validateIDECompatible(result *boxRelayResult) error {
	if result == nil {
		return fmt.Errorf("missing relay result")
	}
	if result.CompressedFrames > 0 && !strings.EqualFold(strings.TrimSpace(result.ConnectContentEncoding), "gzip") {
		return fmt.Errorf(
			"received %d compressed Connect data frame(s) without connect-content-encoding: gzip; Cursor 3.19.x fails with %q",
			result.CompressedFrames,
			"received compressed envelope, but do not know how to decompress",
		)
	}
	return nil
}

func applyBoxRelayHeaders(httpReq *http.Request, acc *auth.Account, config *boxRelayConfig, sandClientVersion, checksumSource string) error {
	version := strings.TrimSpace(sandClientVersion)
	if version == "" {
		return fmt.Errorf("sand client version is empty")
	}
	executor.ApplyCommonHeadersWithClientType(httpReq, acc, auth.GenerateRequestID(), "sand")
	httpReq.Header.Del("x-cursor-client-source")
	httpReq.Header.Set("x-cursor-client-version", version)
	httpReq.Header.Del("x-cursor-config-version")
	httpReq.Header.Del("x-cursor-client-commit")

	switch strings.ToLower(strings.TrimSpace(checksumSource)) {
	case "service":
		if config == nil || config.Refresh == nil || strings.TrimSpace(config.Refresh.MachineID) == "" {
			return fmt.Errorf("relay config has no refresh.machineId for service checksum")
		}
		httpReq.Header.Set("x-cursor-checksum", auth.GenerateChecksum(time.Now(), strings.TrimSpace(config.Refresh.MachineID), ""))
	case "ide":
		if strings.TrimSpace(httpReq.Header.Get("x-cursor-checksum")) == "" {
			return fmt.Errorf("IDE account has no checksum")
		}
	default:
		return fmt.Errorf("invalid checksum source %q (want service or ide)", checksumSource)
	}
	return nil
}

func printSand(label string, snap *usage.Snapshot) {
	if snap == nil {
		fmt.Printf("%s sand=<nil>\n", label)
		return
	}
	fetched := snap.Fetched.SandUsage
	fmt.Printf("%s sand_fetched=%v unlocked=%v available=%v percent=%.6f plan=%q next_reset=%s\n",
		label, fetched, snap.BotUnlocked, snap.BotHasAvailable, snap.BotPercentUsed, snap.BotPlanLabel, formatTime(snap.BotNextResetAt))
	fmt.Printf("%s auto=%.6f other=%.6f errors=%d\n", label, snap.AutoPercentUsed, snap.APIPercentUsed, len(snap.Errors))
}

func printDelta(before, after *usage.Snapshot) {
	if before == nil || after == nil {
		return
	}
	fmt.Printf("DELTA bot_percent=%+.6f auto_percent=%+.6f other_percent=%+.6f bot_available=%v->%v\n",
		after.BotPercentUsed-before.BotPercentUsed,
		after.AutoPercentUsed-before.AutoPercentUsed,
		after.APIPercentUsed-before.APIPercentUsed,
		before.BotHasAvailable, after.BotHasAvailable)
}

func formatTime(value *time.Time) string {
	if value == nil {
		return "-"
	}
	return value.Format(time.RFC3339)
}

func effectiveClientType(acc *auth.Account) string {
	if acc != nil && strings.TrimSpace(acc.ClientType) != "" {
		return strings.TrimSpace(acc.ClientType)
	}
	return "ide"
}

func summarize(value string, max int) string {
	value = strings.ReplaceAll(strings.ReplaceAll(value, "\n", " "), "\r", " ")
	if len(value) <= max {
		return value
	}
	return value[:max-3] + "..."
}

func loadAccount(path string) (*auth.Account, error) {
	if strings.TrimSpace(path) != "" {
		buf, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read account: %w", err)
		}
		var acc auth.Account
		if err := json.Unmarshal(buf, &acc); err != nil {
			return nil, fmt.Errorf("decode account: %w", err)
		}
		if acc.AccessToken == "" {
			return nil, fmt.Errorf("account %s has empty access_token", path)
		}
		acc.FillSessionDefaults(time.Now())
		return &acc, nil
	}
	return loadAccountFromIDE()
}

func loadAccountFromIDE() (*auth.Account, error) {
	return auth.LoadAccountFromIDE()
}
