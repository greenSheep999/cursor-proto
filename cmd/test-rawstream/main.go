// test-rawstream captures the raw HTTP/2 gRPC-web body from a Cursor RunSSE
// call verbatim to disk. Two files are written per run:
//
//	<out>-raw.bin     — every response byte in order, before any envelope split
//	<out>-frames.txt  — one line per decoded Connect frame:
//	                      idx  flags  length  first-bytes-hex  proto-shape
//
// The proto-shape column shows the leading tag byte(s) so we can compare
// message types across models without decoding the whole payload.
//
// Usage:
//
//	go run ./cmd/test-rawstream -model claude-4.5-sonnet -msg "reply with PONG" -out /tmp/claude
package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"google.golang.org/protobuf/proto"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	cursorpb "github.com/router-for-me/cursor-proto/gen/cursor"
)

func main() {
	model := flag.String("model", "composer-2.5", "model to use")
	msg := flag.String("msg", "reply with the exact word: PONG", "user message")
	out := flag.String("out", "/tmp/rawstream", "output file prefix (writes <prefix>-raw.bin and <prefix>-frames.txt)")
	timeout := flag.Duration("timeout", 90*time.Second, "overall timeout")
	autoStop := flag.Bool("auto-stop", true, "stop when turn_ended arrives")
	flag.Parse()

	acc := loadAccountFromIDE()
	c := executor.NewClient(acc)
	c.API3 = c.API2

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	// Run the RunSSE stream and copy every byte to a buffer as it arrives so
	// that even a deadline-exceeded run leaves us with the bytes we captured.
	body, err := executor.RawChatStream(ctx, c, &executor.ChatRequest{
		Model:             *model,
		UserMessage:       *msg,
		PureMode:          true,
		AutoStopOnTurnEnd: *autoStop,
	})
	if err != nil {
		log.Fatalf("RawChatStream: %v", err)
	}
	defer body.Close()

	var rawBody []byte
	readErr := make(chan error, 1)
	buf := make([]byte, 8192)
	go func() {
		for {
			n, err := body.Read(buf)
			if n > 0 {
				rawBody = append(rawBody, buf[:n]...)
			}
			if err != nil {
				readErr <- err
				return
			}
		}
	}()
	select {
	case <-ctx.Done():
		body.Close()
		<-readErr
	case err := <-readErr:
		if err != nil && err != io.EOF {
			fmt.Printf("read error (partial capture kept): %v\n", err)
		}
	}
	_ = err

	rawPath := *out + "-raw.bin"
	framesPath := *out + "-frames.txt"

	if err := os.WriteFile(rawPath, rawBody, 0o644); err != nil {
		log.Fatalf("write raw: %v", err)
	}
	fmt.Printf("wrote %d bytes to %s\n", len(rawBody), rawPath)

	f, err := os.Create(framesPath)
	if err != nil {
		log.Fatalf("create frames: %v", err)
	}
	defer f.Close()

	scan := rawBody
	idx := 0
	for len(scan) >= 5 {
		flags := scan[0]
		length := binary.BigEndian.Uint32(scan[1:5])
		if uint32(len(scan)-5) < length {
			fmt.Fprintf(f, "[%d] TRUNCATED  flags=%02x length=%d have=%d\n",
				idx, flags, length, len(scan)-5)
			break
		}
		end := 5 + int(length)
		payload := scan[5:end]
		hexHead := hex.EncodeToString(payload[:min(32, len(payload))])
		isTrailer := (flags & 0x80) != 0
		shape := shapePeek(payload, isTrailer)
		fmt.Fprintf(f, "[%d] flags=%02x trailer=%v length=%d head=%s shape=%s\n",
			idx, flags, isTrailer, length, hexHead, shape)
		// Try decoding as AgentServerMessage; log which oneof it fills.
		if !isTrailer {
			asm := &cursorpb.AgentV1_AgentServerMessage{}
			if err := proto.Unmarshal(payload, asm); err != nil {
				fmt.Fprintf(f, "    UNMARSHAL_ERR agentservermessage: %v\n", err)
			} else {
				fmt.Fprintf(f, "    agentservermessage: %s\n", oneofPresent(asm))
			}
		}
		scan = scan[end:]
		idx++
	}
	if len(scan) > 0 {
		fmt.Fprintf(f, "TAIL %d bytes: %s\n", len(scan), hex.EncodeToString(scan[:min(64, len(scan))]))
	}
	fmt.Printf("wrote frames breakdown to %s (%d frames)\n", framesPath, idx)
}

func shapePeek(p []byte, isTrailer bool) string {
	if isTrailer {
		return "trailer(" + string(p) + ")"
	}
	if len(p) == 0 {
		return "<empty>"
	}
	tag := p[0]
	field := int(tag >> 3)
	wire := int(tag & 0x07)
	extra := ""
	if wire == 2 && len(p) > 1 {
		// try to also show the inner tag if any
		inner := ""
		payloadLen, n := binary.Uvarint(p[1:])
		if n > 0 && int(payloadLen)+1+n <= len(p) {
			inner = hex.EncodeToString(p[1+n : min(1+n+8, len(p))])
		}
		extra = fmt.Sprintf(" innerHex=%s", inner)
	}
	// If payload starts with ASCII, show that too
	if isMostlyPrintable(p) {
		extra += " ascii=" + string(bytes.TrimRight(p, "\x00"))
	}
	return fmt.Sprintf("field=%d wire=%d%s", field, wire, extra)
}

func isMostlyPrintable(b []byte) bool {
	if len(b) == 0 || len(b) > 200 {
		return false
	}
	printable := 0
	for _, c := range b {
		if c >= 0x20 && c < 0x7f {
			printable++
		}
	}
	return printable*100/len(b) >= 80
}

func oneofPresent(m *cursorpb.AgentV1_AgentServerMessage) string {
	if m.GetInteractionUpdate() != nil {
		return "interaction_update"
	}
	if m.GetExecServerMessage() != nil {
		return "exec_server_message"
	}
	if m.GetConversationCheckpointUpdate() != nil {
		return "conversation_checkpoint_update"
	}
	if m.GetKvServerMessage() != nil {
		return "kv_server_message"
	}
	if m.GetExecServerControlMessage() != nil {
		return "exec_server_control_message"
	}
	if m.GetInteractionQuery() != nil {
		return "interaction_query"
	}
	return "empty"
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
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
