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

// Cursor bills two separate included-usage bars:
//
//	cursor  — Auto + Composer + Grok ("Cursor Models")
//	other   — Claude / GPT / Gemini / Kimi / … ("Other Models")
//
// CPA's scheduler only sees the model list from model.for_auth. If we
// advertise Claude on an account whose Other Models bar is already at
// 100%, the host still routes there. Cursor then either 429s or, worse,
// overflows the call into the remaining Cursor Models bar. Both are
// wrong for a pool that still has Composer quota and no Other quota.
const (
	quotaBucketCursor = "cursor"
	quotaBucketOther  = "other"
	quotaCacheTTL     = 45 * time.Second
	quotaFetchTimeout = 8 * time.Second
	// Cursor's PlanUsage percents are inconsistent across fields: Auto is
	// 0–1+ (1.0 = 100%), API is 0–100 (100 = 100%). Anything above 2 is
	// treated as a 0–100 scale so a live API=100 still means exhausted.
	quotaExhaustedAt = 1.0
)

type accountQuota struct {
	Auto float64
	API  float64
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
	return accountQuota{Auto: snap.AutoPercentUsed, API: snap.APIPercentUsed}, true
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

// normalizeQuotaPercent maps Cursor's mixed percent scales onto 0–1+.
func normalizeQuotaPercent(p float64) float64 {
	if p > 2 {
		return p / 100
	}
	return p
}

func quotaExhausted(p float64) bool {
	return normalizeQuotaPercent(p) >= quotaExhaustedAt
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

// modelQuotaBucket classifies a request model against Cursor's two
// included-usage bars. Bracketed CPA variants and provider prefixes are
// stripped first so composer-2.5[fast=true] and cursor/claude-sonnet-4-6
// land in the same buckets as their base ids.
func modelQuotaBucket(model string) string {
	m := normalizeModelID(model)
	switch {
	case m == "" || m == "auto" || m == "auto-smart" || m == "default":
		return quotaBucketCursor
	case strings.HasPrefix(m, "composer-"), strings.HasPrefix(m, "cursor-"),
		strings.HasPrefix(m, "grok-"), strings.Contains(m, "grok"):
		return quotaBucketCursor
	default:
		return quotaBucketOther
	}
}

func filterModelsForQuota(authID string, acc *auth.Account, names []string) []string {
	if len(names) == 0 {
		return names
	}
	quota, ok := loadAccountQuota(authID, acc)
	if !ok {
		return names
	}
	dropCursor := quotaExhausted(quota.Auto)
	dropOther := quotaExhausted(quota.API)
	if !dropCursor && !dropOther {
		return names
	}
	out := make([]string, 0, len(names))
	droppedCursor, droppedOther := 0, 0
	for _, name := range names {
		switch modelQuotaBucket(name) {
		case quotaBucketCursor:
			if dropCursor {
				droppedCursor++
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
	fmt.Fprintf(os.Stderr, "[cursor-plugin] quota_route for_auth auth=%s email=%s auto=%.4f api=%.4f kept=%d dropped_cursor=%d dropped_other=%d\n",
		strings.TrimSpace(authID), email, quota.Auto, quota.API, len(out), droppedCursor, droppedOther)
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
	used := quota.API
	if bucket == quotaBucketCursor {
		used = quota.Auto
	}
	if !quotaExhausted(used) {
		return false, ""
	}
	fmt.Fprintf(os.Stderr, "[cursor-plugin] quota_route skip email=%s model=%s bucket=%s auto=%.4f api=%.4f\n",
		acc.Email, model, bucket, quota.Auto, quota.API)
	return true, fmt.Sprintf("cursor %s-models quota exhausted for %s (auto=%.4f api=%.4f)", bucket, model, quota.Auto, quota.API)
}
