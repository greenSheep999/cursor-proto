package kernel

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/cursor-proto/auth"
	"github.com/router-for-me/cursor-proto/sdk/cpaformat"
)

// Cursor bills three separate included-usage bars:
//
//	cursor  — Auto + Composer ("Cursor Models")
//	other   — Claude / GPT / Gemini / Kimi / … ("Other Models")
//	bot     — Grok Bot / Sand
//
// CPA's scheduler only sees the model list from model.for_auth. If we
// advertise Claude on an account whose Other Models bar is already at
// 100%, the host still routes there. Cursor then either 429s or, worse,
// overflows the call into the remaining Cursor Models bar. Both are
// wrong for a pool that still has Composer quota and no Other quota.
//
// The bot bar is separate and opt-in per account: an account only has it
// after claiming Sand eligibility. Grok models bill against it, NOT
// against the Auto+Composer bar — an earlier revision classified them as
// "cursor", which meant grok requests were gated on the wrong allowance
// and any Sand entitlement went unused.
const (
	quotaBucketCursor = "cursor"
	quotaBucketOther  = "other"
	quotaBucketBot    = "bot"
	quotaCacheTTL     = 45 * time.Second
	quotaFetchTimeout = 8 * time.Second
	// Cursor's Auto bar is a 0-1+ ratio (1.0 = 100%). We still gate the
	// "cursor" bucket on it because a team account really can burn through
	// its Composer/Grok allotment without leaving usage-based billing open
	// for those specific families.
	quotaExhaustedAt = 1.0
)

// accountQuota captures the signals we use to gate provider ownership.
//
// Older revisions read GetCurrentPeriodUsage.ApiPercentUsed and treated
// api_percent_used == 100 as "kill Claude for this account". That was
// wrong: Cursor returns api_percent_used=100 whenever total_spend >=
// plan_free_limit, even for accounts whose team plan actually pays for
// Other Models via usage-based billing. Real accounts on VPS had that
// field pinned to 100 while still serving Claude in the IDE, so CPA
// unregistered every Claude variant and cctest.ai's requests failed
// with "unknown provider for model claude-opus-4-8-medium".
//
// The Cursor server exposes the actual "no more API for you" state as
// two orthogonal flags: in_slow_pool (Auto/Composer are throttled but
// Other Models still work) and no_usage_based_allowed (the account
// cannot buy usage beyond the plan). Only when both are true does the
// pool truly refuse Other-bucket calls; that combination is what we now
// gate on.
//
// The bot fields come from GetSandUsageStatus. BotFetched distinguishes
// "the RPC failed" from "the RPC said this account has no Sand" — without
// it a transport blip would look identical to a locked bar and silently
// drop every grok model.
type accountQuota struct {
	Auto                float64
	InSlowPool          bool
	NoUsageBasedAllowed bool

	BotFetched      bool
	BotUnlocked     bool
	BotHasAvailable bool
	BotPercentUsed  float64
	BotPlanLabel    string
}

type quotaCacheEntry struct {
	quota accountQuota
	ok    bool
	at    time.Time
}

var quotaCache sync.Map // cacheKey -> quotaCacheEntry

// fetchAccountQuota loads the two Cursor billing bars for an account.
// Tests replace this. A false ok means "do not filter" — fail open so a
// usage-API blip cannot take a healthy account out of the composer pool.
var fetchAccountQuota = fetchAccountQuotaLive

func fetchAccountQuotaLive(acc *auth.Account) (accountQuota, bool) {
	if acc == nil || strings.TrimSpace(acc.AccessToken) == "" {
		return accountQuota{}, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaFetchTimeout)
	defer cancel()
	snap, err := defaultSnapshotFetcher(ctx, acc)
	if err != nil || snap == nil || !snap.Fetched.CurrentPeriodUsage {
		return accountQuota{}, false
	}
	// Require both slow-pool and hard-limit signals to actually be
	// populated. Either one missing means we did not see the ground
	// truth and should fail open — pool churn is worse than the odd
	// wasted upstream 429.
	if !snap.Fetched.SlowPoolStatus || !snap.Fetched.HardLimit {
		return accountQuota{}, false
	}
	// The Sand bar is deliberately NOT part of the fail-open guard above.
	// Most accounts have no Sand entitlement at all, and on those the RPC
	// can legitimately fail (permission denied) — treating that as "don't
	// filter anything" would throw away the cursor/other gating we did
	// successfully read. Instead BotFetched records whether we saw it, and
	// the bot bucket is only gated when we did.
	return accountQuota{
		Auto:                snap.AutoPercentUsed,
		InSlowPool:          snap.InSlowPool,
		NoUsageBasedAllowed: snap.NoUsageBasedAllowed,
		BotFetched:          snap.Fetched.SandUsage,
		BotUnlocked:         snap.BotUnlocked,
		BotHasAvailable:     snap.BotHasAvailable,
		BotPercentUsed:      snap.BotPercentUsed,
		BotPlanLabel:        snap.BotPlanLabel,
	}, true
}

func quotaCacheKey(authID string, acc *auth.Account) string {
	if acc != nil {
		if email := strings.ToLower(strings.TrimSpace(acc.Email)); email != "" {
			return email
		}
		if id := strings.TrimSpace(acc.AuthID); id != "" {
			return id
		}
	}
	return strings.TrimSpace(authID)
}

func loadAccountQuota(authID string, acc *auth.Account) (accountQuota, bool) {
	key := quotaCacheKey(authID, acc)
	if key != "" {
		if raw, ok := quotaCache.Load(key); ok {
			entry := raw.(quotaCacheEntry)
			if time.Since(entry.at) < quotaCacheTTL {
				return entry.quota, entry.ok
			}
		}
	}
	quota, ok := fetchAccountQuota(acc)
	if key != "" {
		quotaCache.Store(key, quotaCacheEntry{quota: quota, ok: ok, at: time.Now()})
	}
	return quota, ok
}

func resetQuotaCache() {
	quotaCache.Range(func(key, _ any) bool {
		quotaCache.Delete(key)
		return true
	})
}

func normalizeAutoPercent(p float64) float64 {
	if p > 2 {
		return p / 100
	}
	return p
}

func quotaExhaustedAuto(p float64) bool {
	return normalizeAutoPercent(p) >= quotaExhaustedAt
}

// quotaExhaustedOther is the API/Other-Models gate. Cursor's Other Models
// bar can sit at 100+ % while the account still bills to usage-based spend
// (which is the case for every healthy team account on the VPS pool). Only
// a slow-pool state combined with no_usage_based_allowed genuinely means
// "no more Claude/GPT/Gemini here".
func quotaExhaustedOther(q accountQuota) bool {
	return q.InSlowPool && q.NoUsageBasedAllowed
}

func normalizeModelID(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	if i := strings.IndexByte(m, '['); i >= 0 {
		m = strings.TrimSpace(m[:i])
	}
	if i := strings.LastIndexByte(m, '/'); i >= 0 {
		m = strings.TrimSpace(m[i+1:])
	}
	return m
}

// modelQuotaBucket classifies a request model against Cursor's three
// included-usage bars. Bracketed CPA variants and provider prefixes are
// stripped first so composer-2.5[fast=true] and cursor/claude-sonnet-4-6
// land in the same buckets as their base ids.
//
// Grok goes to the bot bar, not the Auto+Composer bar. Order matters: the
// grok test runs before the composer/cursor prefixes so a hypothetical
// "cursor-grok-*" id still bills to bot.
func modelQuotaBucket(model string) string {
	m := normalizeModelID(model)
	switch {
	case m == "" || m == "auto" || m == "auto-smart" || m == "default":
		return quotaBucketCursor
	case strings.Contains(m, "grok"):
		return quotaBucketBot
	case strings.HasPrefix(m, "composer-"), strings.HasPrefix(m, "cursor-"):
		return quotaBucketCursor
	default:
		return quotaBucketOther
	}
}

// quotaExhaustedBot reports whether the Sand bar can still take calls.
//
// Unknown (RPC not fetched) is deliberately "not exhausted" — fail open,
// same principle as the other two bars. A locked bar (never claimed Sand)
// IS exhausted: advertising grok on an account with no Sand entitlement
// just sends the host somewhere Cursor will refuse.
func quotaExhaustedBot(q accountQuota) bool {
	if !q.BotFetched {
		return false
	}
	if !q.BotUnlocked {
		return true
	}
	return !q.BotHasAvailable
}

func filterModelsForQuota(authID string, acc *auth.Account, names []string) []string {
	if len(names) == 0 {
		return names
	}
	quota, ok := loadAccountQuota(authID, acc)
	if !ok {
		return names
	}
	dropCursor := quotaExhaustedAuto(quota.Auto)
	dropOther := quotaExhaustedOther(quota)
	dropBot := quotaExhaustedBot(quota)
	if !dropCursor && !dropOther && !dropBot {
		return names
	}
	out := make([]string, 0, len(names))
	droppedCursor, droppedOther, droppedBot := 0, 0, 0
	for _, name := range names {
		switch modelQuotaBucket(name) {
		case quotaBucketCursor:
			if dropCursor {
				droppedCursor++
				continue
			}
		case quotaBucketBot:
			if dropBot {
				droppedBot++
				continue
			}
		default:
			if dropOther {
				droppedOther++
				continue
			}
		}
		out = append(out, name)
	}
	email := ""
	if acc != nil {
		email = acc.Email
	}
	fmt.Fprintf(os.Stderr, "[cursor-plugin] quota_route for_auth auth=%s email=%s auto=%.4f slow_pool=%v no_usage_based=%v bot_fetched=%v bot_unlocked=%v bot_avail=%v bot_pct=%.4f kept=%d dropped_cursor=%d dropped_other=%d dropped_bot=%d\n",
		strings.TrimSpace(authID), email, quota.Auto, quota.InSlowPool, quota.NoUsageBasedAllowed,
		quota.BotFetched, quota.BotUnlocked, quota.BotHasAvailable, quota.BotPercentUsed,
		len(out), droppedCursor, droppedOther, droppedBot)
	return out
}

func quotaSkipFromStorage(storage []byte, model string) (bool, string) {
	if len(storage) == 0 {
		return false, ""
	}
	file, err := cpaformat.Unmarshal(storage)
	if err != nil {
		return false, ""
	}
	acc, err := file.ToAccount()
	if err != nil || acc == nil {
		return false, ""
	}
	quota, ok := loadAccountQuota(acc.Email, acc)
	if !ok {
		return false, ""
	}
	bucket := modelQuotaBucket(model)
	var exhausted bool
	switch bucket {
	case quotaBucketCursor:
		exhausted = quotaExhaustedAuto(quota.Auto)
	case quotaBucketBot:
		exhausted = quotaExhaustedBot(quota)
	default:
		exhausted = quotaExhaustedOther(quota)
	}
	if !exhausted {
		return false, ""
	}
	if bucket == quotaBucketBot {
		fmt.Fprintf(os.Stderr, "[cursor-plugin] quota_route skip email=%s model=%s bucket=bot unlocked=%v avail=%v pct=%.4f plan=%q\n",
			acc.Email, model, quota.BotUnlocked, quota.BotHasAvailable, quota.BotPercentUsed, quota.BotPlanLabel)
		if !quota.BotUnlocked {
			return true, fmt.Sprintf("cursor bot-models quota unavailable for %s (account has no Sand/Grok Bot entitlement)", model)
		}
		return true, fmt.Sprintf("cursor bot-models quota exhausted for %s (pct=%.4f plan=%q)", model, quota.BotPercentUsed, quota.BotPlanLabel)
	}
	fmt.Fprintf(os.Stderr, "[cursor-plugin] quota_route skip email=%s model=%s bucket=%s auto=%.4f slow_pool=%v no_usage_based=%v\n",
		acc.Email, model, bucket, quota.Auto, quota.InSlowPool, quota.NoUsageBasedAllowed)
	return true, fmt.Sprintf("cursor %s-models quota exhausted for %s (auto=%.4f slow_pool=%v no_usage_based=%v)", bucket, model, quota.Auto, quota.InSlowPool, quota.NoUsageBasedAllowed)
}
