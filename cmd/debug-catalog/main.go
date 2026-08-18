// Command debug-catalog prints the three model lists the plugin can hand to
// CPA for one account, so an operator can see exactly which IDs get registered
// for routing versus which ones Cursor will actually accept.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
)

func main() {
	acc, err := loadAccount()
	if err != nil {
		fmt.Fprintln(os.Stderr, "load account:", err)
		os.Exit(1)
	}
	options := []executor.Option{}
	if proxy := strings.TrimSpace(os.Getenv("CURSOR_UPSTREAM_PROXY")); proxy != "" {
		options = append(options, executor.WithProxyURL(proxy))
	}
	client := executor.NewClient(acc, options...)
	resp, err := client.ListModels()
	if err != nil {
		fmt.Fprintln(os.Stderr, "list models:", err)
		os.Exit(1)
	}

	available := executor.AvailableModelIDs(resp)
	routable := executor.RoutableModelIDs(resp)
	static := executor.StableRoutableModelFallbackIDs()

	fmt.Printf("AvailableModelIDs      (compact/base): %d\n", len(available))
	fmt.Printf("RoutableModelIDs       (model.for_auth): %d\n", len(routable))
	fmt.Printf("StableRoutableFallback (model.static):   %d\n\n", len(static))

	fmt.Println("== routable IDs NOT in the base catalog (variant expansion) ==")
	baseSet := toSet(available)
	extra := diff(routable, baseSet)
	fmt.Printf("%d ids\n", len(extra))
	for _, id := range extra {
		fmt.Println("  ", id)
	}

	fmt.Println("\n== static-fallback IDs this account cannot even name ==")
	routableSet := toSet(routable)
	phantom := diff(static, routableSet)
	fmt.Printf("%d ids advertised by model.static but absent from the live catalog\n", len(phantom))
	for _, id := range phantom {
		if strings.HasPrefix(id, "claude") {
			fmt.Println("  ", id)
		}
	}
}

func toSet(items []string) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, item := range items {
		set[item] = true
	}
	return set
}

func diff(items []string, exclude map[string]bool) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if !exclude[item] {
			out = append(out, item)
		}
	}
	sort.Strings(out)
	return out
}

func loadAccount() (*auth.Account, error) {
	acc := &auth.Account{}
	if path := strings.TrimSpace(os.Getenv("CURSOR_TOKEN_FILE")); path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		var file struct {
			AccessToken  string `json:"access_token"`
			MachineID    string `json:"machine_id"`
			MacMachineID string `json:"mac_machine_id"`
		}
		if err := json.Unmarshal(raw, &file); err != nil {
			return nil, err
		}
		acc.AccessToken = file.AccessToken
		acc.MachineID = file.MachineID
		acc.MacMachineID = file.MacMachineID
	} else {
		token, err := readIDEAccessToken()
		if err != nil {
			return nil, err
		}
		acc.AccessToken = token
	}
	if acc.AccessToken == "" {
		return nil, fmt.Errorf("no access token found")
	}
	if acc.MachineID == "" {
		acc.MachineID, _ = auth.GetMachineID()
	}
	if acc.MacMachineID == "" {
		acc.MacMachineID, _ = auth.GetMacMachineID()
	}
	acc.FillSessionDefaults(time.Now())
	return acc, nil
}

func readIDEAccessToken() (string, error) {
	path := os.Getenv("HOME") + "/Library/Application Support/Cursor/User/globalStorage/state.vscdb"
	db, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var token string
	if err := db.QueryRow(
		`SELECT value FROM ItemTable WHERE key = 'cursorAuth/accessToken'`,
	).Scan(&token); err != nil {
		return "", err
	}
	return token, nil
}
