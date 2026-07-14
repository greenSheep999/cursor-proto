package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"strings"
)

// apiKeysEnv is the environment variable that provides comma-separated API keys
// when the -api-keys flag is not set. Kept as a constant so tests can reference
// the same name.
const apiKeysEnv = "CURSOR_PROXY_API_KEYS"

// LoadAPIKeys resolves the effective set of API keys given a raw flag value
// (which may be empty). The flag value takes precedence over the environment
// variable. Whitespace around each entry is trimmed and empty entries are
// dropped. Returns nil when no keys are configured.
func LoadAPIKeys(flagValue string) []string {
	raw := flagValue
	if raw == "" {
		raw = os.Getenv(apiKeysEnv)
	}
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	keys := make([]string, 0, len(parts))
	for _, p := range parts {
		if k := strings.TrimSpace(p); k != "" {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	return keys
}

// RequireAPIKeys wraps next with an API-key gate. When keys is empty the
// wrapper is a passthrough so existing deployments (no auth) keep working.
// When keys is non-empty, every request must present one of the configured
// keys via one of the following channels, checked in priority order:
//
//  1. Authorization: Bearer <key>   — OpenAI/Anthropic/Cursor default
//  2. x-api-key: <key>              — Anthropic SDK, Kilo Code, others
//  3. x-goog-api-key: <key>         — Gemini SDK
//  4. ?key=<key>                    — Gemini SDK query fallback
//
// Additional headers callers may send (e.g. OpenRouter's `HTTP-Referer`,
// `X-Title`) are ignored, not rejected — Kilo Code and Cline occasionally
// tag requests with them and they don't affect auth.
//
// Requests that fail the check are answered with an OpenAI-compatible
// 401 error body.
//
// The comparison against each configured key uses crypto/subtle.ConstantTimeCompare
// so that a wrong key does not leak information about how much of it matched.
func RequireAPIKeys(keys []string, next http.Handler) http.Handler {
	if len(keys) == 0 {
		return next
	}
	// Pre-convert keys to byte slices once so the hot path stays cheap.
	keyBytes := make([][]byte, len(keys))
	for i, k := range keys {
		keyBytes[i] = []byte(k)
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Metadata endpoints are always reachable — cursor2api's sidecar
		// supervisor probes /v1/proxy-info before it has the API key
		// wired up, and there's no secret in the response.
		if r.URL.Path == "/v1/proxy-info" {
			next.ServeHTTP(w, r)
			return
		}
		presented, ok := extractAPIKey(r)
		if !ok {
			writeInvalidAPIKey(w, "Missing API key. Provide one via Authorization: Bearer, x-api-key, x-goog-api-key, or ?key=.")
			return
		}
		presentedBytes := []byte(presented)
		matched := false
		for _, kb := range keyBytes {
			if subtle.ConstantTimeCompare(presentedBytes, kb) == 1 {
				matched = true
				// Do not break: keep comparing so timing does not depend on
				// which position matched.
			}
		}
		if !matched {
			writeInvalidAPIKey(w, "Invalid API key provided.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// extractAPIKey returns the first non-empty API key surfaced by the request
// across the four supported channels, in priority order:
//
//  1. Authorization: Bearer <key>
//  2. x-api-key
//  3. x-goog-api-key
//  4. ?key= query parameter
func extractAPIKey(r *http.Request) (string, bool) {
	if tok, ok := extractBearer(r.Header.Get("Authorization")); ok {
		return tok, true
	}
	if v := strings.TrimSpace(r.Header.Get("x-api-key")); v != "" {
		return v, true
	}
	if v := strings.TrimSpace(r.Header.Get("x-goog-api-key")); v != "" {
		return v, true
	}
	if v := strings.TrimSpace(r.URL.Query().Get("key")); v != "" {
		return v, true
	}
	return "", false
}

// extractBearer returns the token portion of an "Authorization: Bearer <token>"
// header, or ok=false if the header is missing or malformed. The scheme match
// is case-insensitive to match common OpenAI-style clients.
func extractBearer(header string) (string, bool) {
	if header == "" {
		return "", false
	}
	const prefix = "bearer "
	if len(header) < len(prefix) {
		return "", false
	}
	if !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// writeInvalidAPIKey emits an OpenAI-compatible 401 error body.
func writeInvalidAPIKey(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// Encoding a static map cannot fail in practice; ignoring the error keeps
	// the surface small and matches the style of the rest of this file.
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{
			"message": msg,
			"type":    "invalid_request_error",
			"param":   nil,
			"code":    "invalid_api_key",
		},
	})
}
