// simcache_wiring.go plumbs the simcache.Store into the request path of
// cursor-proxy.
//
// The simulator exists so downstream dashboards (CPA, new-api) that key off
// `cached_tokens` see monotonic, prefix-stable behaviour instead of Cursor's
// noisy internal cache counter. Everything here is a no-op when the store is
// nil (i.e. `-simulate-cache=false`).
package main

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/router-for-me/cursor-proto/executor"
	"github.com/router-for-me/cursor-proto/executor/simcache"
	"github.com/router-for-me/cursor-proto/translator"
)

// simCacheDecision captures what the simulator concluded before RunChat is
// called. The proxy uses it to (a) set the response header and (b) rewrite
// the `Usage` before rendering it to the client.
type simCacheDecision struct {
	// Active is false when simcache is disabled globally; all other fields
	// are ignored in that case.
	Active bool
	// Hit is true when the prefix was already in the store within TTL.
	Hit bool
	// SimulatedTokens is the estimated token count for the prefix — set on
	// both hits and misses (the entry's TokenCount is populated at record
	// time).
	SimulatedTokens int
}

// header returns the value for the `x-cursor-cache-source` response header
// that applies BEFORE we've seen the real cache_read from Cursor. Used for
// streaming responses where headers must be flushed up front.
func (d simCacheDecision) headerBeforeStream() string {
	if !d.Active {
		return "real"
	}
	if d.Hit {
		return "simulated"
	}
	return "real"
}

// headerAfter returns the value for the `x-cursor-cache-source` header once
// Cursor's real cache_read is known. Used for non-streaming responses.
// `realCacheRead` is Cursor's own cache_read_tokens for this turn.
func (d simCacheDecision) headerAfter(realCacheRead int64) string {
	if !d.Active {
		return "real"
	}
	if !d.Hit {
		return "real"
	}
	if realCacheRead > 0 {
		return "mixed"
	}
	return "simulated"
}

// applyToUsage rewrites `u` in place so the emitted `cached_tokens` reflects
// max(realCacheRead, simulatedTokens) on a hit. On a first miss for
// Anthropic-shaped responses, the caller can additionally set
// `CacheWriteTokens = simulatedTokens` so the client sees an
// Anthropic-lifecycle `cache_creation_input_tokens` value.
func (d simCacheDecision) applyToUsage(u *translator.Usage, markCreation bool) {
	if u == nil || !d.Active {
		return
	}
	if d.Hit {
		if int64(d.SimulatedTokens) > u.CacheReadTokens {
			u.CacheReadTokens = int64(d.SimulatedTokens)
		}
		return
	}
	// Miss. Optionally advertise Anthropic-style cache creation on the first
	// pass so the client can watch the lifecycle.
	if markCreation && u.CacheWriteTokens == 0 {
		u.CacheWriteTokens = int64(d.SimulatedTokens)
	}
}

// prefixForSimCache builds a cache namespace plus the stable prompt prefix.
// Account, protocol, model, and tool schema are included so a dashboard-only
// simulated hit cannot leak across identities or semantically different
// requests. The last user turn is intentionally omitted as the fresh input.
func prefixForSimCache(protocol, model string, tools []executor.ToolDefinition, systemPrompt string, history []executor.HistoryTurn) string {
	var b strings.Builder
	b.WriteString("account: ")
	b.WriteString(wireAccountEmailString())
	b.WriteString("\nprotocol: ")
	b.WriteString(protocol)
	b.WriteString("\nmodel: ")
	b.WriteString(model)
	b.WriteString("\ntools: ")
	if raw, err := json.Marshal(tools); err == nil {
		b.Write(raw)
	}
	b.WriteString("\n")
	if s := strings.TrimSpace(systemPrompt); s != "" {
		b.WriteString("system: ")
		b.WriteString(s)
		b.WriteString("\n")
	}
	for _, t := range history {
		b.WriteString(t.Role)
		b.WriteString(": ")
		b.WriteString(t.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// decideSimCache looks the prefix up and returns the decision. Passing a nil
// store yields an inactive decision (Active=false) — safe to call
// unconditionally from handlers.
//
// On a miss, LookupOrRecord returns cachedTokens=0 by contract; we still
// surface the entry's TokenCount so the Anthropic path can populate
// `cache_creation_input_tokens` on this first pass.
func decideSimCache(store *simcache.Store, prefix string) simCacheDecision {
	if store == nil {
		return simCacheDecision{Active: false}
	}
	hit, cachedTokens, ent := store.LookupOrRecord(prefix)
	simulated := cachedTokens
	if !hit && ent != nil {
		simulated = ent.TokenCount
	}
	return simCacheDecision{
		Active:          true,
		Hit:             hit,
		SimulatedTokens: simulated,
	}
}

// parseCacheTTL parses a duration string with a fallback to 10 minutes on
// error or empty input. Extracted so the flag/env parsing in main stays
// short.
func parseCacheTTL(s string) time.Duration {
	if s == "" {
		return 10 * time.Minute
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 10 * time.Minute
	}
	return d
}
