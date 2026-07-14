package main

// Read-only introspection of what actually flowed through this proxy
// recently. This is *observation* — not configuration; cursor-proxy
// doesn't own or manage the tools, MCP servers, or hooks the client
// declared, but it can report what it saw so cursor2api's UI can
// surface "your Claude Code / aider is using these MCP servers" and
// similar visibility without needing to reach into the client's own
// config files.
//
// Deliberate non-goals:
//   - Not persisted across proxy restarts. Ring buffer, in-memory.
//   - Not per-user / per-key. cursor-proxy is single-tenant.
//   - Not exact — the buffer holds the last N observations, so a
//     tool the model called just once, an hour ago, may have aged out.
//
// Data model: each observation is a snapshot of a single /v1/messages
// or /v1/chat/completions request's declared tools[] array — nothing
// more. We don't record prompts, message bodies, model names, or any
// output. Observations flow through recordRequest() into a ring buffer
// keyed by monotonic timestamp; aggregation queries scan backwards
// from now until the `since` cutoff.

import (
	"strings"
	"sync"
	"time"
)

// toolKind labels what kind of tool a client declared. We derive this
// purely from the tool name, since MCP tools get their `mcp__server__`
// prefix baked in by Claude Code / aider / cline before they land in
// the request body.
type toolKind string

const (
	toolKindCustom toolKind = "custom"
	toolKindMCP    toolKind = "mcp"
)

// toolObservation is one tool declared in one request. Kept as a value
// type (not pointer) so the ring buffer stores dense data instead of
// pointer-chasing across cache lines during aggregation.
type toolObservation struct {
	name string
	kind toolKind
	// mcpServer is the extracted server segment for MCP tools
	// ("filesystem" for "mcp__filesystem__read_file"). Empty for
	// custom tools.
	mcpServer string
	seenAt    time.Time
}

// classifyTool inspects a tool name and returns (kind, server). MCP
// tools follow Claude Code's `mcp__<server>__<tool>` convention; we
// tolerate either double-underscore or single-underscore-after-mcp
// so third-party clients that pick their own separator (aider ships
// `mcp_<server>_<tool>` variants in some builds) also classify
// correctly.
func classifyTool(name string) (toolKind, string) {
	// Fast path: the canonical form Claude Code emits.
	if strings.HasPrefix(name, "mcp__") {
		rest := name[len("mcp__"):]
		if idx := strings.Index(rest, "__"); idx > 0 {
			return toolKindMCP, rest[:idx]
		}
		// Single-segment mcp__foo — treat foo as the server, no
		// tool suffix. Rare but seen with hand-authored MCP shims.
		return toolKindMCP, rest
	}
	// Loose match: some clients drop one underscore. We only accept
	// this when the second segment looks like a server name (no
	// dots, no slashes) to avoid false positives on user tool names
	// like "mcp_upload_v2" that aren't actually MCP.
	if strings.HasPrefix(name, "mcp_") && !strings.HasPrefix(name, "mcp__") {
		rest := name[len("mcp_"):]
		if idx := strings.Index(rest, "_"); idx > 0 {
			server := rest[:idx]
			if server != "" && !strings.ContainsAny(server, "./\\") {
				return toolKindMCP, server
			}
		}
	}
	return toolKindCustom, ""
}

// toolRing is a fixed-size ring buffer of toolObservations, guarded by
// a Mutex. Not a channel: writers massively outnumber readers (every
// request writes N tools; queries happen at UI refresh rate), and a
// slice-backed ring is cheaper to scan for aggregation than draining
// a channel.
//
// Size: 4096 is enough for ~5 minutes of steady traffic at 10 tools/req
// and 1 req/s. Aggregation queries default to a 60s window; anything
// older ages out silently. When the buffer wraps, we simply overwrite
// the oldest slot — the aggregator uses seenAt to filter.
type toolRing struct {
	mu   sync.Mutex
	buf  []toolObservation
	head int  // next write index
	full bool // true once we've wrapped at least once
}

func newToolRing(size int) *toolRing {
	if size <= 0 {
		size = 4096
	}
	return &toolRing{buf: make([]toolObservation, size)}
}

// record stores a batch of observations (all from one request) with a
// shared timestamp. Taking the timestamp once per request instead of
// per observation matches how downstream consumers reason about "a
// request happened" — they group by request, not by tool.
func (r *toolRing) record(names []string, at time.Time) {
	if len(names) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		kind, server := classifyTool(n)
		r.buf[r.head] = toolObservation{
			name:      n,
			kind:      kind,
			mcpServer: server,
			seenAt:    at,
		}
		r.head++
		if r.head >= len(r.buf) {
			r.head = 0
			r.full = true
		}
	}
}

// snapshotSince returns a copy of all observations at-or-after cutoff,
// ordered newest first. Copy semantics keep the caller's aggregation
// off the ring's lock.
func (r *toolRing) snapshotSince(cutoff time.Time) []toolObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Walk the ring backwards from head-1 to head, stopping at cutoff.
	n := len(r.buf)
	end := r.head
	start := 0
	if r.full {
		start = r.head // full ring: everything before head is valid
	}
	// If not full, the used range is [0, head).
	count := r.head
	if r.full {
		count = n
	}
	out := make([]toolObservation, 0, count)
	// Walk newest-first: start at end-1 and step backwards.
	idx := end - 1
	if idx < 0 {
		idx += n
	}
	for i := 0; i < count; i++ {
		obs := r.buf[idx]
		if obs.seenAt.Before(cutoff) {
			break
		}
		out = append(out, obs)
		idx--
		if idx < 0 {
			idx += n
		}
	}
	_ = start
	return out
}
