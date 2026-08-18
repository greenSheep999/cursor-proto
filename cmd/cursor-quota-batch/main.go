package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

func load(path string) (*auth.Account, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if file, err := cpaformat.Unmarshal(raw); err == nil && file.Type == cpaformat.ProviderType {
		return file.ToAccount()
	}
	var a auth.Account
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.AccessToken) == "" {
		return nil, fmt.Errorf("empty token")
	}
	return &a, nil
}

func main() {
	dir := "/auths"
	if len(os.Args) > 1 {
		dir = os.Args[1]
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "cursor-*.json"))
	fmt.Printf("accounts=%d\n", len(matches))
	for _, path := range matches {
		acc, err := load(path)
		name := filepath.Base(path)
		if err != nil {
			fmt.Printf("FAIL file=%s err=%v\n", name, err)
			continue
		}
		email := ""
		if acc != nil {
			email = acc.Email
		}
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		client := usage.New(executor.NewClient(acc))
		snap, ferr := client.Fetch(ctx)
		cancel()
		if ferr != nil && snap == nil {
			fmt.Printf("FAIL file=%s email=%s err=%v\n", name, redact(email), ferr)
			continue
		}
		if snap == nil {
			fmt.Printf("FAIL file=%s email=%s empty snapshot\n", name, redact(email))
			continue
		}
		fmt.Printf("OK file=%s email=%s plan=%s country=%s raw_auto=%.6f raw_api=%.6f raw_total=%.6f auto=%d/%d api=%d/%d remain=%d spend24h=%d tokens24h_in=%d tokens24h_out=%d slow=%v ondemand=%v no_usage_based=%v fetched_period=%v errs=%d\n",
			name, redact(email), snap.SignUpType, snap.Country,
			snap.AutoPercentUsed, snap.APIPercentUsed, snap.TotalPercentUsed,
			snap.AutoSpend, snap.AutoLimit, snap.APISpend, snap.APILimit,
			snap.Remaining, snap.Spend24h, snap.Tokens24h.Input, snap.Tokens24h.Output,
			snap.InSlowPool, snap.UsageBasedPremiumRequestsEnabled,
			snap.NoUsageBasedAllowed, snap.Fetched.CurrentPeriodUsage, len(snap.Errors),
		)
		page, errEvents := client.ListEvents(ctx, usage.EventListOptions{Page: 1, PageSize: 6})
		if errEvents != nil {
			fmt.Printf("  events_err=%v\n", errEvents)
			continue
		}
		if page == nil || len(page.Events) == 0 {
			fmt.Printf("  events=0 total=%d\n", 0)
			continue
		}
		fmt.Printf("  events_total=%d recent=%d\n", page.TotalCount, len(page.Events))
		for _, ev := range page.Events {
			when := time.UnixMilli(ev.GetTimestamp()).UTC().Format("15:04:05")
			charged := float32(0)
			if ev.ChargedCents != nil {
				charged = *ev.ChargedCents
			}
			fmt.Printf("    %s model=%s kind=%v cost=%.3f charged_cents=%.2f token_based=%v\n",
				when, ev.GetModel(), ev.GetKind(), ev.GetRequestsCosts(), charged, ev.GetIsTokenBasedCall())
		}
	}
}
