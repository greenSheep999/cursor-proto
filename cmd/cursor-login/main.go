// cursor-login runs the Cursor OAuth device flow, prompts the user to open a
// URL in their browser, then persists the resulting tokens + local machine
// identifiers to a JSON account file.
//
// Usage:
//
//	cursor-login -email you@example.com -out ~/.cursor-proto
//	cursor-login -no-browser          # print URL only, don't try to open
//	cursor-login -credential-file account.txt -cookie-header-file cookies.txt
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/usage"
)

func main() {
	email := flag.String("email", "", "email to associate with the account (used for filename)")
	outDir := flag.String("out", defaultOutDir(), "directory to store account json")
	noBrowser := flag.Bool("no-browser", false, "just print the URL; do not try to open a browser")
	credentialFile := flag.String("credential-file", "", "file containing user_id::web-JWT or email----password----user_id::web-JWT")
	cookieHeaderFile := flag.String("cookie-header-file", "", "file containing the full cursor.com Cookie header from a normal signed-in browser session")
	timeout := flag.Duration("timeout", 5*time.Minute, "how long to wait for the browser flow")
	interval := flag.Duration("interval", 3*time.Second, "poll interval")
	flag.Parse()

	var webCredential *auth.WebSessionCredential
	if strings.TrimSpace(*credentialFile) != "" {
		credentialEmail, rawCredential, err := readCredentialFile(*credentialFile)
		if err != nil {
			log.Fatalf("read credential file: %v", err)
		}
		if strings.TrimSpace(*email) == "" {
			*email = credentialEmail
		}
		webCredential, err = auth.ParseWebSessionCredential(rawCredential)
		if err != nil {
			log.Fatalf("parse web session credential: %v", err)
		}
	}

	if strings.TrimSpace(*email) == "" {
		fmt.Fprintln(os.Stderr, "-email is required")
		os.Exit(2)
	}

	sess, err := auth.StartLogin()
	if err != nil {
		log.Fatalf("start login: %v", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()

	if webCredential != nil {
		if strings.TrimSpace(*cookieHeaderFile) == "" {
			log.Fatal("-cookie-header-file is required with -credential-file; first sign in normally and export the full cursor.com Cookie header")
		}
		cookieHeader, err := readSecretFile(*cookieHeaderFile)
		if err != nil {
			log.Fatalf("read cookie header file: %v", err)
		}
		if override := strings.TrimSpace(os.Getenv("CURSOR_LOGIN_CALLBACK_URL_OVERRIDE")); override != "" {
			sess.LoginCallbackURL = override
		}
		fmt.Println("Authorizing PKCE login with the existing browser cookie environment...")
		if err := sess.AuthorizeWithWebSession(ctx, webCredential, cookieHeader); err != nil {
			log.Fatalf("authorize web session: %v", err)
		}
	} else {
		fmt.Println("=====================================================")
		fmt.Println("Open this URL in your browser to authorize:")
		fmt.Println(sess.LoginURL)
		fmt.Println("=====================================================")

		if !*noBrowser {
			if err := openURL(sess.LoginURL); err != nil {
				fmt.Fprintf(os.Stderr, "(couldn't open browser automatically: %v)\n", err)
			}
		}
	}

	fmt.Printf("Polling every %s, timeout %s...\n", *interval, *timeout)
	result, err := sess.WaitForLogin(ctx, *interval, *timeout)
	if err != nil {
		log.Fatalf("wait: %v", err)
	}

	fmt.Println("✓ Login successful")

	acc, err := auth.NewAccountFromPoll(result, *email)
	if err != nil {
		log.Fatalf("build account: %v", err)
	}

	// Backfill team_id from GetMe. The OAuth poll response only carries
	// tokens + auth id, while the IDE persists team membership and sends it
	// as request metadata. This improves account fidelity but does not unlock
	// Claude models: the current edge independently gates those on the client
	// transport stack. A failure here is warning-only because the tokens are
	// still usable and a later usage.Fetch can retry the identity lookup.
	backfillTeamID(context.Background(), acc)

	path, err := auth.SaveAccount(*outDir, acc)
	if err != nil {
		log.Fatalf("save: %v", err)
	}

	fmt.Println("Saved account to:")
	fmt.Println(" ", path)
	fmt.Println()
	fmt.Println("Details:")
	fmt.Printf("  email:            %s\n", acc.Email)
	fmt.Printf("  user_id:          %s\n", acc.UserID)
	fmt.Printf("  auth_type:        %s\n", acc.AuthType)
	if acc.TeamID != "" {
		fmt.Printf("  team_id:          %s\n", acc.TeamID)
	}
	fmt.Printf("  machine_id:       %s...\n", head(acc.MachineID, 16))
	fmt.Printf("  mac_machine_id:   %s...\n", head(acc.MacMachineID, 16))
	fmt.Printf("  checksum_sess:    %s...\n", head(acc.ChecksumSession, 24))
}

// backfillTeamID calls GetMe once with a short timeout and copies team_id
// (and the canonical email, if the poll's email differs from what Cursor
// records) onto acc. Personal accounts leave TeamID empty and this is a
// no-op. Errors are logged and swallowed — the login flow already
// succeeded and downstream calls will retry.
func backfillTeamID(ctx context.Context, acc *auth.Account) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := usage.New(executor.NewClient(acc))
	snap, err := client.Fetch(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warn: GetMe failed, team_id left empty: %v\n", err)
		return
	}
	if snap == nil || !snap.Fetched.Me {
		if reason, ok := snap.Errors["me"]; ok {
			fmt.Fprintf(os.Stderr, "warn: GetMe did not return an identity: %s\n", reason)
		}
		return
	}
	if snap.ApplyToAccount(acc) && acc.TeamID != "" {
		fmt.Fprintf(os.Stderr, "note: detected team account (team_id=%s, team=%q)\n", acc.TeamID, snap.TeamName)
	}
}

func readCredentialFile(path string) (email, credential string, err error) {
	raw, err := readSecretFile(path)
	if err != nil {
		return "", "", err
	}
	line := ""
	for _, candidate := range strings.Split(raw, "\n") {
		if strings.TrimSpace(candidate) != "" {
			line = strings.TrimSpace(candidate)
			break
		}
	}
	if line == "" {
		return "", "", fmt.Errorf("credential file is empty")
	}
	parts := strings.SplitN(line, "----", 3)
	if len(parts) == 3 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[2]), nil
	}
	return "", line, nil
}

func readSecretFile(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return value, nil
}

func defaultOutDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return filepath.Join(h, ".cursor-proto")
	}
	return "."
}

func openURL(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	return cmd.Start()
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
