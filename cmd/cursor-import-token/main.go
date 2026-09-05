// Command cursor-import-token writes one CPA cursor auth file from a
// session token (raw JWT or user_<id>::<jwt>) and optionally fills
// identity from DashboardService.GetMe.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
	"github.com/router-for-me/cursor-proto/usage"
)

func redact(email string) string {
	email = strings.TrimSpace(email)
	if i := strings.IndexByte(email, '@'); i > 0 {
		keep := 3
		if i < keep {
			keep = i
		}
		return email[:keep] + "***@" + email[i+1:]
	}
	if len(email) > 12 {
		return email[:8] + "..."
	}
	return email
}

func splitAccessToken(raw string) (userID, jwt string) {
	raw = strings.TrimSpace(raw)
	if i := strings.LastIndex(raw, "::"); i >= 0 {
		return strings.TrimSpace(raw[:i]), strings.TrimSpace(raw[i+2:])
	}
	return "", raw
}

func main() {
	raw := strings.TrimSpace(os.Getenv("CURSOR_ACCESS_TOKEN"))
	if raw == "" {
		fmt.Fprintln(os.Stderr, "CURSOR_ACCESS_TOKEN is required")
		os.Exit(2)
	}
	outDir := strings.TrimSpace(os.Getenv("CURSOR_AUTH_DIR"))
	if outDir == "" {
		outDir = "/auths"
	}
	note := strings.TrimSpace(os.Getenv("CURSOR_NOTE"))
	if note == "" {
		note = "sub-team imported " + time.Now().UTC().Format("2006-01-02")
	}

	userID, jwt := splitAccessToken(raw)
	if jwt == "" {
		fmt.Fprintln(os.Stderr, "token is empty after stripping user prefix")
		os.Exit(2)
	}
	claims, err := auth.DecodeJWTClaims(jwt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "decode jwt: %v\n", err)
		os.Exit(1)
	}
	if typ := strings.ToLower(strings.TrimSpace(claims.Type)); typ != "" && typ != "session" {
		fmt.Fprintf(os.Stderr, "refuse token type %q (want session)\n", typ)
		os.Exit(1)
	}

	machineID, _ := auth.GetMachineID()
	macID, _ := auth.GetMacMachineID()
	now := time.Now().UTC()
	acc := &auth.Account{
		UserID:       userID,
		AccessToken:  jwt,
		AuthID:       claims.Sub,
		AuthType:     "Auth_0",
		IssuedAt:     auth.IssuedAtFromJWT(jwt),
		ExpiresAt:    auth.ExpiresAtFromJWT(jwt),
		MachineID:    machineID,
		MacMachineID: macID,
		Refreshable:  false,
		RefreshLead:  30 * time.Minute,
	}
	if acc.UserID == "" {
		acc.UserID = authUserID(claims.Sub)
	}
	if acc.IssuedAt.IsZero() {
		acc.IssuedAt = now
	}
	acc.FillSessionDefaults(now)

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	snap, fetchErr := usage.New(executor.NewClient(acc)).Fetch(ctx)
	if fetchErr != nil {
		fmt.Fprintf(os.Stderr, "getme/usage: %v\n", fetchErr)
	}
	if snap != nil {
		snap.ApplyToAccount(acc)
		if acc.Email == "" {
			acc.Email = snap.Email
		}
		if acc.TeamID == "" {
			acc.TeamID = snap.TeamID
		}
	}
	if acc.Email == "" {
		acc.Email = acc.UserID
	}

	file, err := cpaformat.FromAccount(acc)
	if err != nil {
		fmt.Fprintf(os.Stderr, "convert: %v\n", err)
		os.Exit(1)
	}
	file.Disabled = false
	file.Priority = 40
	file.Note = note
	file.RequestRetry = 3

	path, err := file.WriteToDir(outDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "write: %v\n", err)
		os.Exit(1)
	}

	auto, api := 0.0, 0.0
	if snap != nil {
		auto, api = snap.AutoPercentUsed, snap.APIPercentUsed
	}
	fmt.Printf("wrote file=%s email=%s user=%s team=%s auto=%.4f api=%.4f disabled=%v refreshable=%v\n",
		baseName(path), redact(acc.Email), acc.UserID, acc.TeamID, auto, api, file.Disabled, file.Refreshable)
}

func authUserID(sub string) string {
	if i := strings.Index(sub, "|"); i >= 0 {
		return sub[i+1:]
	}
	return sub
}

func baseName(path string) string {
	if i := strings.LastIndex(path, "/"); i >= 0 {
		return path[i+1:]
	}
	return path
}
