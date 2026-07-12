package kernel

// Tiny helpers shared by the OTP flow files. Keeps otp_flow.go focused
// on the wire logic.

import (
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
)

// uuidNewString wraps google/uuid to avoid a direct import in
// otp_flow.go — see the newUUID indirection there.
func uuidNewString() string { return uuid.NewString() }

// decodeAuthMeJSON tries strict JSON first, then falls back to
// scanning the raw body for a JSON object (some transports wrap it in
// prose). Returns an error only when nothing parseable is found.
func decodeAuthMeJSON(body []byte) (map[string]any, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		return m, nil
	}
	// Loose fallback — some responses are HTML-ish with an embedded
	// <script> that carries the JSON. Look for the first "{ ... }" run.
	start := -1
	for i, b := range body {
		if b == '{' {
			start = i
			break
		}
	}
	if start >= 0 {
		if err := json.Unmarshal(body[start:], &m); err == nil {
			return m, nil
		}
	}
	return nil, fmt.Errorf("body is not JSON")
}
