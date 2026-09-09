// cursorctl is the unified operator CLI. The initial surface intentionally
// focuses on the Sand dashboard work: status is read-only, while claim is an
// explicit mutating command. Existing cursor-* commands remain available
// until their shared helpers are extracted into stable packages.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/sand"
	sandide "github.com/router-for-me/cursor-proto/sand/ide"
	"github.com/router-for-me/cursor-proto/sdk/batch"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	args := os.Args[1:]
	switch args[0] {
	case "account":
		runAccount(args[1:])
	case "sand":
		runSand(args[1:])
	case "usage":
		fmt.Fprintln(os.Stderr, "cursorctl usage is not wired yet; use cursor-usage for the current snapshot")
		os.Exit(2)
	case "token", "login", "cpa", "catalog":
		fmt.Fprintf(os.Stderr, "cursorctl %s is reserved in this skeleton; use the existing cursor-%s command for now\n", args[0], args[0])
		os.Exit(2)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", args[0])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: cursorctl <account|sand|usage|token|login|cpa|catalog> ...")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  cursorctl account list [--pool-dir DIR]")
	fmt.Fprintln(os.Stderr, "  cursorctl account switch --credential-file FILE [--email EMAIL] [--restart]")
	fmt.Fprintln(os.Stderr, "  cursorctl sand status [--account FILE | --all --pool-dir DIR]")
	fmt.Fprintln(os.Stderr, "  cursorctl sand claim  [--account FILE | --all --pool-dir DIR]")
	fmt.Fprintln(os.Stderr, "  cursorctl sand ide <status|plan|provision-box|install|setup-all|uninstall> [--app /Applications/Cursor.app] [--restart]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  token/login/cpa/catalog are reserved for the next extraction pass")
}

func runAccount(args []string) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, "usage: cursorctl account <list|switch> ...")
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	if args[0] == "switch" {
		runAccountSwitch(args[1:])
		return
	}
	if args[0] != "list" {
		fatalf("unknown account command %q", args[0])
	}
	fs := flag.NewFlagSet("cursorctl account list", flag.ExitOnError)
	poolDir := fs.String("pool-dir", defaultPoolDir(), "account pool directory")
	jsonOut := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args[1:])
	entries, err := batch.LoadPool(*poolDir)
	if err != nil {
		fatalf("load pool: %v", err)
	}
	if *jsonOut {
		writeJSON(entries)
		return
	}
	fmt.Printf("%-36s %-28s %-12s %-10s\n", "EMAIL", "FILE", "EXPIRES", "STATUS")
	for _, entry := range entries {
		email, status := "<unreadable>", "error"
		if entry.Account != nil {
			email = entry.Account.Email
			status = "ready"
		}
		fmt.Printf("%-36s %-28s %-12s %-10s\n", email, filepath.Base(entry.Path), batch.ExpiresIn(entry, time.Now().UTC()), status)
	}
}

func runAccountSwitch(args []string) {
	fs := flag.NewFlagSet("cursorctl account switch", flag.ExitOnError)
	credentialFile := fs.String("credential-file", "", "file containing a session JWT, user_id::JWT, or exported account line; use - for stdin")
	refreshFile := fs.String("refresh-token-file", "", "optional file containing a separate refresh token")
	email := fs.String("email", "", "email override when the credential line does not contain one")
	dbPath := fs.String("db", "", "Cursor state.vscdb path (default: current user's IDE state)")
	restart := fs.Bool("restart", true, "close Cursor before the switch and reopen it afterward")
	_ = fs.Parse(args)
	if strings.TrimSpace(*credentialFile) == "" {
		fatalf("account switch requires --credential-file (use - for stdin)")
	}
	credentialText, err := readSecretInput(*credentialFile)
	if err != nil {
		fatalf("read credential: %v", err)
	}
	refreshText := ""
	if strings.TrimSpace(*refreshFile) != "" {
		refreshText, err = readSecretInput(*refreshFile)
		if err != nil {
			fatalf("read refresh token: %v", err)
		}
	}
	credential, err := auth.ParseIDELoginCredential(credentialText, *email, refreshText, time.Now())
	if err != nil {
		fatalf("parse credential: %v", err)
	}
	if strings.TrimSpace(*dbPath) == "" {
		*dbPath, err = auth.IDEStatePath()
		if err != nil {
			fatalf("resolve IDE state: %v", err)
		}
	}
	wasRunning := cursorProcessRunning()
	if wasRunning {
		if !*restart {
			fatalf("Cursor is running; close it first or leave --restart enabled")
		}
		if err := stopCursor(); err != nil {
			fatalf("stop Cursor: %v", err)
		}
	}
	if err := auth.WriteIDELoginCredential(*dbPath, credential); err != nil {
		fatalf("write IDE login: %v", err)
	}
	if *restart && (wasRunning || runtime.GOOS == "darwin") {
		if err := startCursor(); err != nil {
			fatalf("account switched, but Cursor restart failed: %v", err)
		}
	}
	fmt.Printf("switched Cursor IDE account to %s (user_id=%s, refresh_present=true)\n", credential.Email, credential.UserID)
}

func readSecretInput(path string) (string, error) {
	var (
		buf []byte
		err error
	)
	if strings.TrimSpace(path) == "-" {
		buf, err = os.ReadFile("/dev/stdin")
	} else {
		buf, err = os.ReadFile(path)
	}
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(buf))
	if value == "" {
		return "", errors.New("secret input is empty")
	}
	return value, nil
}

func cursorProcessRunning() bool {
	if runtime.GOOS != "darwin" {
		return false
	}
	return exec.Command("pgrep", "-x", "Cursor").Run() == nil
}

func stopCursor() error {
	if runtime.GOOS != "darwin" {
		return errors.New("automatic Cursor restart is currently supported on macOS only")
	}
	if out, err := exec.Command("osascript", "-e", `tell application "Cursor" to quit`).CombinedOutput(); err != nil {
		return fmt.Errorf("request quit: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	deadline := time.Now().Add(20 * time.Second)
	for cursorProcessRunning() && time.Now().Before(deadline) {
		time.Sleep(250 * time.Millisecond)
	}
	if cursorProcessRunning() {
		return errors.New("Cursor did not exit within 20 seconds")
	}
	return nil
}

func startCursor() error {
	if runtime.GOOS != "darwin" {
		return errors.New("automatic Cursor restart is currently supported on macOS only")
	}
	return exec.Command("open", "-a", "/Applications/Cursor.app").Run()
}

func runSand(args []string) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" {
		fmt.Fprintln(os.Stderr, "usage: cursorctl sand <status|claim|ide> ...")
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	subcommand := args[0]
	if subcommand == "ide" {
		runSandIDE(args[1:])
		return
	}
	if subcommand != "status" && subcommand != "claim" {
		fatalf("unknown sand command %q", subcommand)
	}
	fs := flag.NewFlagSet("cursorctl sand "+subcommand, flag.ExitOnError)
	accountPath := fs.String("account", "", "path to one account JSON (default: local Cursor IDE account)")
	all := fs.Bool("all", false, "operate on every account in --pool-dir")
	poolDir := fs.String("pool-dir", defaultPoolDir(), "account pool directory")
	baseURL := fs.String("base-url", "", "override cursor.com base URL (testing/proxy)")
	timeout := fs.Duration("timeout", 20*time.Second, "per-account timeout")
	jsonOut := fs.Bool("json", false, "emit JSON")
	_ = fs.Parse(args[1:])

	accounts, err := selectAccounts(*accountPath, *all, *poolDir)
	if err != nil {
		fatalf("select account: %v", err)
	}
	if len(accounts) == 0 {
		fatalf("no accounts selected")
	}
	ctx := context.Background()
	if subcommand == "status" {
		runSandStatus(ctx, accounts, *baseURL, *timeout, *jsonOut)
		return
	}
	runSandClaim(ctx, accounts, *baseURL, *timeout, *jsonOut)
}

func runSandIDE(args []string) {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(os.Stderr, "usage: cursorctl sand ide <status|plan|provision-box|install|setup-all|uninstall> [--app /Applications/Cursor.app] [--restart]")
		if len(args) == 0 {
			os.Exit(2)
		}
		return
	}
	operation := args[0]
	if operation != "status" && operation != "plan" && operation != "provision-box" && operation != "install" && operation != "setup-all" && operation != "uninstall" {
		fatalf("unknown sand ide command %q", operation)
	}
	fs := flag.NewFlagSet("cursorctl sand ide "+operation, flag.ExitOnError)
	appPath := fs.String("app", "", "Cursor.app path (default: auto-discover)")
	restart := fs.Bool("restart", false, "restart Cursor after a successful mutation")
	timeout := fs.Duration("timeout", 3*time.Minute, "maximum patch operation time")
	_ = fs.Parse(args[1:])
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, err := sandide.Run(ctx, operation, *appPath, *restart)
	if result.Output != "" {
		fmt.Print(result.Output)
	}
	if err != nil {
		fatalf("sand ide %s: %v", operation, err)
	}
	if result.ExitCode != 0 {
		os.Exit(result.ExitCode)
	}
}

type selectedAccount struct {
	Path    string        `json:"path,omitempty"`
	Account *auth.Account `json:"account"`
}

func selectAccounts(path string, all bool, poolDir string) ([]selectedAccount, error) {
	if all && strings.TrimSpace(path) != "" {
		return nil, errors.New("--account and --all are mutually exclusive")
	}
	if all {
		entries, err := batch.LoadPool(poolDir)
		if err != nil {
			return nil, err
		}
		out := make([]selectedAccount, 0, len(entries))
		for _, entry := range entries {
			if entry.Account != nil {
				out = append(out, selectedAccount{Path: entry.Path, Account: entry.Account})
			}
		}
		return out, nil
	}
	if strings.TrimSpace(path) != "" {
		account, err := auth.LoadAccount(path)
		if err != nil {
			return nil, err
		}
		return []selectedAccount{{Path: path, Account: account}}, nil
	}
	account, err := loadAccountFromIDE()
	if err != nil {
		return nil, err
	}
	return []selectedAccount{{Account: account}}, nil
}

type statusRow struct {
	Path  string       `json:"path,omitempty"`
	Email string       `json:"email,omitempty"`
	Value *sand.Status `json:"status,omitempty"`
	Error string       `json:"error,omitempty"`
}

func runSandStatus(ctx context.Context, accounts []selectedAccount, baseURL string, timeout time.Duration, jsonOut bool) {
	rows := make([]statusRow, 0, len(accounts))
	for _, selected := range accounts {
		row := statusRow{Path: selected.Path, Email: selected.Account.Email}
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		clientOpts := []sand.Option{}
		if baseURL != "" {
			clientOpts = append(clientOpts, sand.WithBaseURL(baseURL))
		}
		value, err := sand.New(selected.Account, clientOpts...).Status(callCtx)
		cancel()
		row.Value, row.Error = value, errorText(err)
		rows = append(rows, row)
	}
	if jsonOut {
		writeJSON(rows)
		return
	}
	for _, row := range rows {
		if row.Error != "" {
			fmt.Printf("%s error=%s\n", displayAccount(row), row.Error)
			continue
		}
		usage := "unknown"
		if row.Value != nil && row.Value.Usage != nil {
			usage = fmt.Sprintf("%.1f%%", row.Value.Usage.UsagePercent*100)
			if !row.Value.Usage.Unlocked {
				usage += " locked"
			} else if !row.Value.Usage.HasAvailableUsage {
				usage += " exhausted"
			}
		}
		fmt.Printf("%s sand=%s team=%s errors=%d\n", displayAccount(row), usage, statusTeam(row.Value), len(statusErrors(row.Value)))
	}
}

func runSandClaim(ctx context.Context, accounts []selectedAccount, baseURL string, timeout time.Duration, jsonOut bool) {
	results := make([]struct {
		Path   string           `json:"path,omitempty"`
		Email  string           `json:"email,omitempty"`
		Result sand.ClaimResult `json:"result"`
		Error  string           `json:"error,omitempty"`
	}, 0, len(accounts))
	for _, selected := range accounts {
		callCtx, cancel := context.WithTimeout(ctx, timeout)
		clientOpts := []sand.Option{}
		if baseURL != "" {
			clientOpts = append(clientOpts, sand.WithBaseURL(baseURL))
		}
		result, err := sand.New(selected.Account, clientOpts...).Claim(callCtx)
		cancel()
		results = append(results, struct {
			Path   string           `json:"path,omitempty"`
			Email  string           `json:"email,omitempty"`
			Result sand.ClaimResult `json:"result"`
			Error  string           `json:"error,omitempty"`
		}{Path: selected.Path, Email: selected.Account.Email, Result: result, Error: errorText(err)})
	}
	if jsonOut {
		writeJSON(results)
		return
	}
	for _, row := range results {
		fmt.Printf("%s outcome=%s detail=%s\n", row.Email, row.Result.Outcome, row.Result.Detail)
		if row.Error != "" {
			fmt.Printf("  error=%s\n", row.Error)
		}
	}
}

func displayAccount(row statusRow) string {
	if row.Email != "" {
		return row.Email
	}
	return row.Path
}

func statusTeam(value *sand.Status) string {
	if value == nil || value.Me == nil {
		return "-"
	}
	return value.Me.TeamID
}

func statusErrors(value *sand.Status) map[string]string {
	if value == nil || value.Errors == nil {
		return nil
	}
	return value.Errors
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func writeJSON(value any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(value); err != nil {
		fatalf("encode JSON: %v", err)
	}
}

func defaultPoolDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".cursor-pool")
	}
	return "."
}

func loadAccountFromIDE() (*auth.Account, error) {
	return auth.LoadAccountFromIDE()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "cursorctl: "+format+"\n", args...)
	os.Exit(1)
}

var _ = sort.Strings
