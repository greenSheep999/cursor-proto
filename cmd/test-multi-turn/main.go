// test-multi-turn verifies that the ConversationHistory carries context
// between turns.
//
// Turn 1: "Remember the number 42."   → capture assistant text A1.
// Turn 2: user="What was the number?" History=[user:turn1, assistant:A1]
//
//	→ assistant text A2 must mention "42".
//
// Both turns share a single ConversationId so Cursor treats them as one
// session.
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/translator"
)

func main() {
	model := flag.String("model", "claude-4.5-sonnet", "model to use")
	timeout := flag.Duration("timeout", 120*time.Second, "overall timeout")
	flag.Parse()

	acc := loadAccountFromIDE()
	c := executor.NewClient(acc)
	c.API3 = c.API2

	fmt.Printf("[test-multi-turn] account=%s model=%s\n", acc.Email, *model)

	convID := auth.GenerateSessionID()
	fmt.Printf("[test-multi-turn] conversation_id=%s\n\n", convID)

	// ---- Turn 1 ----
	turn1User := "Remember the number 42. Reply with a very short acknowledgement."
	fmt.Printf("Turn 1 user: %s\n", turn1User)
	ctx1, cancel1 := context.WithTimeout(context.Background(), *timeout)
	turn1Assistant, err := runOneTurn(ctx1, c, &executor.ChatRequest{
		Model:             *model,
		UserMessage:       turn1User,
		ConversationID:    convID,
		Mode:              1, // ask — avoids the "plan mode" system reminder.
		AutoStopOnTurnEnd: true,
	})
	cancel1()
	if err != nil {
		log.Fatalf("turn 1 failed: %v", err)
	}
	fmt.Printf("Turn 1 assistant: %s\n\n", turn1Assistant)

	// ---- Turn 2 ----
	turn2User := "What was the number I asked you to remember?"
	fmt.Printf("Turn 2 user: %s\n", turn2User)
	history := []executor.HistoryTurn{
		{Role: "user", Content: turn1User},
		{Role: "assistant", Content: turn1Assistant},
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), *timeout)
	turn2Assistant, err := runOneTurn(ctx2, c, &executor.ChatRequest{
		Model:             *model,
		UserMessage:       turn2User,
		ConversationID:    convID,
		History:           history,
		Mode:              1,
		AutoStopOnTurnEnd: true,
	})
	cancel2()
	if err != nil {
		log.Fatalf("turn 2 failed: %v", err)
	}
	fmt.Printf("Turn 2 assistant: %s\n\n", turn2Assistant)

	if strings.Contains(turn2Assistant, "42") {
		fmt.Println("PASS: turn 2 reply mentions \"42\" — multi-turn context works.")
		return
	}
	fmt.Println("FAIL: turn 2 reply does not mention \"42\" — context not preserved.")
	os.Exit(1)
}

func runOneTurn(ctx context.Context, c *executor.Client, req *executor.ChatRequest) (string, error) {
	events, err := c.RunChat(ctx, req)
	if err != nil {
		return "", err
	}
	assistant := ""
	evCount := 0
	trailerCount := 0
	for ev := range events {
		evCount++
		if ev.Trailer {
			trailerCount++
			if len(ev.Raw) > 0 {
				fmt.Printf("  [trailer] %s\n", string(ev.Raw))
			}
			continue
		}
		if ev.Server == nil {
			continue
		}
		if blob := translator.FromKvBlob(ev.Server); blob != nil && blob.AssistantText != "" {
			assistant = blob.AssistantText
		}
	}
	fmt.Printf("  [events=%d trailers=%d]\n", evCount, trailerCount)
	if assistant == "" {
		return "", fmt.Errorf("no assistant text captured")
	}
	return assistant, nil
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
