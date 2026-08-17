// test-model-stack is a differential diagnostic for Cursor's AvailableModels
// response. It sends the same protobuf request and authenticated x-cursor
// headers through Go and through isolated Chromium configurations, then prints
// only non-secret response facts.
package main

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
	"google.golang.org/protobuf/proto"
)

type chromiumInput struct {
	URL                 string            `json:"url"`
	Headers             map[string]string `json:"headers"`
	BodyBase64          string            `json:"bodyBase64"`
	ChromiumArgs        []string          `json:"chromiumArgs,omitempty"`
	UserAgent           string            `json:"userAgent,omitempty"`
	StripBrowserHeaders bool              `json:"stripBrowserHeaders,omitempty"`
	IgnoreHTTPSErrors   bool              `json:"ignoreHTTPSErrors,omitempty"`
}

type chromiumOutput struct {
	Status             int               `json:"status"`
	BodyBase64         string            `json:"bodyBase64"`
	ByteLength         int               `json:"byteLength"`
	Protocol           string            `json:"protocol"`
	RemoteIPAddress    string            `json:"remoteIPAddress"`
	RequestHeaderNames []string          `json:"requestHeaderNames"`
	BrowserSignals     map[string]string `json:"browserSignals"`
}

type variant struct {
	name                string
	args                []string
	userAgent           string
	stripBrowserHeaders bool
	ignoreHTTPSErrors   bool
}

func main() {
	includeNoH2 := flag.Bool("include-no-h2", true, "also launch Chromium with QUIC and HTTP/2 disabled")
	mitmProxy := flag.String("mitm-proxy", "", "optionally test Chromium through an HTTP MITM proxy, for example http://127.0.0.1:18899")
	omitTeamID := flag.Bool("omit-team-id", false, "omit x-cursor-team-id to test whether team scope independently changes the catalog")
	flag.Parse()

	acc, err := loadIDEAccount()
	check(err)
	if *omitTeamID {
		acc.TeamID = ""
	}
	acc.FillSessionDefaults(time.Now())

	requestBody, headers, err := availableModelsRequest(acc)
	check(err)
	url := executor.DefaultAPI2 + "/aiserver.v1.AiService/AvailableModels"

	goClient := executor.NewClient(acc)
	goResponse, err := goClient.ListModels()
	if err != nil {
		fmt.Printf("go             ERROR %v\n", err)
	} else {
		printCatalog("go", "auto", 0, goResponse)
	}

	variants := []variant{
		{name: "chromium", args: nil},
		{name: "chromium-go-ua", userAgent: executor.UserAgent},
		{name: "chromium-stripped", userAgent: executor.UserAgent, stripBrowserHeaders: true},
		{name: "chromium-no-quic", args: []string{"--disable-quic"}},
	}
	if *includeNoH2 {
		variants = append(variants, variant{
			name: "chromium-no-h2",
			args: []string{"--disable-quic", "--disable-http2"},
		})
	}
	if strings.TrimSpace(*mitmProxy) != "" {
		variants = append(variants, variant{
			name:              "chromium-mitm",
			args:              []string{"--proxy-server=" + strings.TrimSpace(*mitmProxy)},
			ignoreHTTPSErrors: true,
		})
	}

	for _, candidate := range variants {
		out, err := runChromium(chromiumInput{
			URL:                 url,
			Headers:             headers,
			BodyBase64:          base64.StdEncoding.EncodeToString(requestBody),
			ChromiumArgs:        candidate.args,
			UserAgent:           candidate.userAgent,
			StripBrowserHeaders: candidate.stripBrowserHeaders,
			IgnoreHTTPSErrors:   candidate.ignoreHTTPSErrors,
		})
		if err != nil {
			fmt.Printf("%-14s ERROR %v\n", candidate.name, err)
			continue
		}
		body, err := base64.StdEncoding.DecodeString(out.BodyBase64)
		if err != nil {
			fmt.Printf("%-14s ERROR decode response: %v\n", candidate.name, err)
			continue
		}
		var catalog cursorpb.AiserverV1_AvailableModelsResponse
		if err := proto.Unmarshal(body, &catalog); err != nil {
			fmt.Printf("%-14s status=%d protocol=%s bytes=%d ERROR protobuf: %v\n",
				candidate.name, out.Status, out.Protocol, out.ByteLength, err)
			continue
		}
		printCatalog(candidate.name, out.Protocol, out.ByteLength, &catalog)
		if candidate.name == "chromium" {
			printBrowserSignals(out.BrowserSignals)
			goBrowserResponse, byteLength, err := goRequest(url, requestBody, headers, out.BrowserSignals)
			if err != nil {
				fmt.Printf("go+browser-headers ERROR %v\n", err)
			} else {
				printCatalog("go+browser-headers", "auto", byteLength, goBrowserResponse)
			}
		}
	}
}

func goRequest(url string, body []byte, baseHeaders, extraHeaders map[string]string) (*cursorpb.AiserverV1_AvailableModelsResponse, int, error) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	for name, value := range baseHeaders {
		req.Header.Set(name, value)
	}
	for name, value := range extraHeaders {
		// Let net/http negotiate and transparently decode gzip. The response
		// entitlement is independent of compression, while copying Chromium's
		// br/zstd accept-encoding would require a separate decoder here.
		if strings.EqualFold(name, "accept-encoding") {
			continue
		}
		req.Header.Set(name, value)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, len(raw), fmt.Errorf("http status %d", resp.StatusCode)
	}
	var catalog cursorpb.AiserverV1_AvailableModelsResponse
	if err := proto.Unmarshal(raw, &catalog); err != nil {
		return nil, len(raw), err
	}
	return &catalog, len(raw), nil
}

func printBrowserSignals(signals map[string]string) {
	names := make([]string, 0, len(signals))
	for name := range signals {
		names = append(names, name)
	}
	sort.Strings(names)
	fmt.Printf("browser signal headers: %s\n", strings.Join(names, ", "))
}

func availableModelsRequest(acc *auth.Account) ([]byte, map[string]string, error) {
	useModelParameters := true
	useReactModelPicker := true
	byokEnabled := false
	reqBody, err := proto.Marshal(&cursorpb.AiserverV1_AvailableModelsRequest{
		IsNightly:             false,
		ExcludeMaxNamedModels: true,
		UseModelParameters:    &useModelParameters,
		UseReactModelPicker:   &useReactModelPicker,
		ByokEnabled:           &byokEnabled,
	})
	if err != nil {
		return nil, nil, err
	}
	req, err := http.NewRequest(http.MethodPost, executor.DefaultAPI2, bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("content-type", "application/proto")
	executor.ApplyCommonHeaders(req, acc, auth.GenerateRequestID())
	headers := make(map[string]string, len(req.Header))
	for name, values := range req.Header {
		lower := strings.ToLower(name)
		// Chromium owns these forbidden request headers. Supplying them to
		// fetch would not make the wire request more faithful and can cause
		// browser-version-dependent errors.
		if lower == "user-agent" || lower == "accept-encoding" || lower == "content-length" {
			continue
		}
		headers[lower] = strings.Join(values, ", ")
	}
	return reqBody, headers, nil
}

func runChromium(input chromiumInput) (*chromiumOutput, error) {
	repo, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	script := filepath.Join(repo, "scripts", "cursor_model_probe.mjs")
	payload, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command("node", script)
	cmd.Stdin = bytes.NewReader(payload)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("node probe: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var out chromiumOutput
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode node output: %w: %s", err, stdout.String())
	}
	if out.Status != http.StatusOK {
		return nil, fmt.Errorf("http status %d", out.Status)
	}
	return &out, nil
}

func printCatalog(name, protocol string, byteLength int, catalog *cursorpb.AiserverV1_AvailableModelsResponse) {
	claude := make([]string, 0)
	for _, model := range catalog.GetModels() {
		if strings.Contains(strings.ToLower(model.GetName()), "claude") {
			claude = append(claude, model.GetName())
		}
	}
	sort.Strings(claude)
	fmt.Printf("%-18s protocol=%-8s bytes=%-7d models=%-3d claude=%-2d\n",
		name, protocol, byteLength, len(catalog.GetModels()), len(claude))
}

func loadIDEAccount() (*auth.Account, error) {
	dbPath := filepath.Join(os.Getenv("HOME"), "Library", "Application Support", "Cursor", "User", "globalStorage", "state.vscdb")
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	query := func(key string) string {
		var value string
		_ = db.QueryRow(`SELECT value FROM ItemTable WHERE key = ?`, key).Scan(&value)
		return value
	}
	access := query("cursorAuth/accessToken")
	if access == "" {
		return nil, fmt.Errorf("Cursor IDE state has no access token")
	}
	machineID, _ := auth.GetMachineID()
	macMachineID, _ := auth.GetMacMachineID()
	return &auth.Account{
		Email:        query("cursorAuth/cachedEmail"),
		AccessToken:  access,
		TeamID:       teamIDFromIDEValues(query("cursorAuth/teamId"), query("cursorAuth/cachedTeam")),
		MachineID:    machineID,
		MacMachineID: macMachineID,
	}, nil
}

func teamIDFromIDEValues(direct, cached string) string {
	if id := strings.TrimSpace(direct); id != "" && id != "0" {
		return id
	}
	raw := strings.TrimSpace(cached)
	if unquoted, err := strconv.Unquote(raw); err == nil {
		raw = strings.TrimSpace(unquoted)
	}
	var payload struct {
		TeamID any `json:"teamId"`
	}
	if json.Unmarshal([]byte(raw), &payload) != nil {
		return ""
	}
	switch value := payload.TeamID.(type) {
	case string:
		return strings.TrimSpace(value)
	case float64:
		if value > 0 {
			return strconv.FormatInt(int64(value), 10)
		}
	}
	return ""
}

func check(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
