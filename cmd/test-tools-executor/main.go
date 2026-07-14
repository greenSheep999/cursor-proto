// test-tools-executor drives the executor directly (bypassing the proxy) to
// confirm whether the SSE stall is specific to the MCP tool wire format.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
)

func main() {
	model := flag.String("model", "composer-2.5", "model")
	pure := flag.Bool("pure", false, "PureMode")
	withTool := flag.Bool("tool", true, "include a tool")
	timeout := flag.Duration("timeout", 60*time.Second, "timeout")
	flag.Parse()

	acc := loadAccount()
	c := executor.NewClient(acc)
	c.API3 = c.API2

	req := &executor.ChatRequest{
		Model:             *model,
		UserMessage:       "What is the weather in Paris? Call get_weather with location=\"Paris\".",
		PureMode:          *pure,
		AutoStopOnTurnEnd: true,
	}
	if *withTool {
		req.Tools = []executor.ToolDefinition{{
			Name:        "get_weather",
			Description: "Fetch weather for a city.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"location": map[string]any{"type": "string"},
				},
				"required": []any{"location"},
			},
		}}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	events, err := c.RunChat(ctx, req)
	if err != nil {
		log.Fatalf("RunChat: %v", err)
	}
	n := 0
	toolSeen := false
	textLen := 0
	turnEnded := false
	for ev := range events {
		n++
		if ev.Trailer {
			fmt.Printf("[%d] TRAILER %s\n", n, string(ev.Raw))
			continue
		}
		if ev.Server == nil {
			fmt.Printf("[%d] RAW %d bytes\n", n, len(ev.Raw))
			continue
		}
		m := ev.Server
		switch {
		case m.GetInteractionUpdate() != nil:
			// fallthrough into detailed handler below
		case m.GetExecServerMessage() != nil:
			exec := m.GetExecServerMessage()
			fmt.Printf("[%d] exec_server_message id=%d exec_id=%s\n", n, exec.GetId(), exec.GetExecId())
			if exec.GetMcpArgs() != nil {
				a := exec.GetMcpArgs()
				fmt.Printf("     McpArgs name=%q tool_name=%q call_id=%s args=%d entries\n",
					a.GetName(), a.GetToolName(), a.GetToolCallId(), len(a.GetArgs()))
				for k, v := range a.GetArgs() {
					fmt.Printf("       ARG %q hex=%x str=%q\n", k, v, string(v))
				}
				toolSeen = true
			}
			continue
		case m.GetKvServerMessage() != nil:
			continue
		default:
			fmt.Printf("[%d] other server message\n", n)
			continue
		}
		{
			iu := m.GetInteractionUpdate()
			if iu.GetTextDelta() != nil {
				textLen += len(iu.GetTextDelta().GetText())
				fmt.Printf("[%d] text_delta %q\n", n, iu.GetTextDelta().GetText())
			}
			if iu.GetToolCallStarted() != nil {
				toolSeen = true
				tc := iu.GetToolCallStarted().GetToolCall()
				fmt.Printf("[%d] tool_call_started call_id=%s mcp=%v\n", n, iu.GetToolCallStarted().GetCallId(), tc.GetMcpToolCall() != nil)
				if mcp := tc.GetMcpToolCall(); mcp != nil {
					if a := mcp.GetArgs(); a != nil {
						fmt.Printf("     name=%q tool_name=%q args_len=%d\n", a.GetName(), a.GetToolName(), len(a.GetArgs()))
						for k, v := range a.GetArgs() {
							fmt.Printf("       ARG %q hex=%x str=%q\n", k, v, string(v))
						}
					}
				}
			}
			if iu.GetToolCallCompleted() != nil {
				fmt.Printf("[%d] tool_call_completed call_id=%s\n", n, iu.GetToolCallCompleted().GetCallId())
			}
			if iu.GetTurnEnded() != nil {
				turnEnded = true
				fmt.Printf("[%d] turn_ended\n", n)
			}
		}
	}
	fmt.Printf("events=%d text=%d tool=%v turn_ended=%v\n", n, textLen, toolSeen, turnEnded)
}

func loadAccount() *auth.Account {
	dbPath := os.Getenv("HOME") + "/Library/Application Support/Cursor/User/globalStorage/state.vscdb"
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro")
	if err != nil {
		log.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	var access string
	if err := db.QueryRow(`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken'`).Scan(&access); err != nil {
		log.Fatalf("no accessToken: %v", err)
	}
	machineID, _ := auth.GetMachineID()
	macID, _ := auth.GetMacMachineID()
	return &auth.Account{AccessToken: access, MachineID: machineID, MacMachineID: macID}
}
