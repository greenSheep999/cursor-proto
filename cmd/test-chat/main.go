// test-chat verifies the RunSSE + BidiAppend flow end-to-end.
package main

import (
	"context"
	"database/sql"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/transport"
)

func main() {
	model := flag.String("model", "claude-4.5-sonnet", "model to use")
	msg := flag.String("msg", "Say hi in one word.", "user message")
	timeout := flag.Duration("timeout", 60*time.Second, "overall timeout")
	upstreamProxy := flag.String("upstream-proxy", "", "upstream proxy URL for Cursor backend calls")
	httpVersion := flag.String("http-version", "auto", "upstream HTTP protocol version: auto | http1.1 | http1.0")
	flag.Parse()

	acc := loadAccountFromIDE()
	if *upstreamProxy == "" {
		for _, envKey := range []string{"CURSOR_PROXY_UPSTREAM", "HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy"} {
			if value := os.Getenv(envKey); value != "" {
				*upstreamProxy = value
				break
			}
		}
	}
	httpVer, err := transport.Parse(*httpVersion)
	if err != nil {
		log.Fatalf("bad -http-version: %v", err)
	}
	options := []executor.Option{executor.WithHTTPVersion(httpVer)}
	if *upstreamProxy != "" {
		options = append(options, executor.WithProxyURL(*upstreamProxy))
	}
	c := executor.NewClient(acc, options...)
	// RunSSE / BidiAppend both live on api2 for regular users, not api3.
	// api3 was seen in mitmproxy TLS handshake failures — that's a *pinned*
	// path used by some other subsystem (likely the retrieval index). Chat
	// itself uses api2.
	c.API3 = c.API2
	fmt.Printf("✓ Client ready\n")
	fmt.Printf("  model=%s\n", *model)
	fmt.Printf("  message=%q\n\n", *msg)
	fmt.Printf("  http_version=%s\n", httpVer)
	fmt.Printf("  upstream_proxy=%s\n\n", proxySummary(*upstreamProxy))

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	events, err := c.RunChat(ctx, &executor.ChatRequest{
		Model:       *model,
		UserMessage: *msg,
	})
	if err != nil {
		log.Fatalf("RunChat: %v", err)
	}

	fmt.Printf("← streaming events\n")
	count := 0
	for ev := range events {
		count++
		if ev.Trailer {
			fmt.Printf("  [%d] TRAILER  (%d bytes) %s\n", count, len(ev.Raw), summarizeTrailer(ev.Raw))
			continue
		}
		if ev.Server == nil {
			fmt.Printf("  [%d] RAW  (%d bytes) %s\n", count, len(ev.Raw), hexPreview(ev.Raw, 40))
			continue
		}
		describeServerMessage(count, ev)
		if count > 30 {
			fmt.Println("  ... (truncating output)")
			break
		}
	}
	fmt.Printf("\nStream closed after %d events\n", count)
}

func describeServerMessage(idx int, ev executor.ChatEvent) {
	m := ev.Server
	if m.GetInteractionUpdate() != nil {
		iu := m.GetInteractionUpdate()
		fmt.Printf("  [%d] InteractionUpdate  text=%q  finished=%v\n",
			idx, truncate(iu.String(), 200), iu.GetTurnEnded())
		return
	}
	if m.GetExecServerMessage() != nil {
		fmt.Printf("  [%d] ExecServerMessage (%d bytes proto)\n", idx, len(ev.Raw))
		return
	}
	fmt.Printf("  [%d] Server (%d bytes) %s\n", idx, len(ev.Raw), truncate(m.String(), 200))
}

func hexPreview(b []byte, n int) string {
	if len(b) < n {
		return hex.EncodeToString(b)
	}
	return hex.EncodeToString(b[:n]) + "..."
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func summarizeTrailer(b []byte) string {
	// Trailer body is textual grpc-status headers. Fully print it — decoding
	// the base64 details often reveals actionable messages.
	return "\n" + string(b)
}

func proxySummary(raw string) string {
	if raw == "" {
		return "(none)"
	}
	return "(configured)"
}

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

	machineID, _ := auth.GetMachineID()
	macID, _ := auth.GetMacMachineID()
	return &auth.Account{
		Email:        email,
		AccessToken:  access,
		MachineID:    machineID,
		MacMachineID: macID,
	}
}
