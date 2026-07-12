package kernel

// Cursor OTP browser-fingerprint signals payload.
//
// Cursor's authenticator posts a base64-encoded JSON blob in the
// `1_signals` multipart field. The blob is a stable browser
// fingerprint — canvas / audio / font hashes, screen metrics, WebGL
// renderer, timezone, etc. The Turnstile bot-detector correlates it
// with the token it just solved: mismatched shapes get 403'd.
//
// The plugin has no browser. We ship a plausible desktop-Chrome-on-
// macOS fingerprint captured from a real successful login and stamp
// fresh createdAtMs / submittedAtMs values per-request so the payload
// looks live. If Cursor tightens the check we may need to rotate this
// fixture (or start solving with a proxied residential IP); for now
// it clears the current authenticator.

import (
	"encoding/base64"
	"encoding/json"
	"time"
)

// signalsFixture is the canonical fingerprint blob. Keep the JSON
// field order deterministic so replays match byte-for-byte. Values
// are lifted from a real Playwright capture; see docs/plugin-cursor-otp-manual-test.md
// for how to regenerate the fixture if Cursor rejects it.
var signalsFixture = map[string]any{
	"timezone":            "Asia/Shanghai",
	"language":            "en-US",
	"hardwareConcurrency": 8,
	"webdriver":           false,
	"userAgent":           "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
	"appVersion":          "5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
	"platform":            "MacIntel",
	"screen": map[string]any{
		"width":             2560,
		"height":            1440,
		"availWidth":        2560,
		"availHeight":       1363,
		"windowOuterWidth":  1200,
		"windowOuterHeight": 1319,
		"colorDepth":        24,
		"pixelDepth":        24,
	},
	"rangeErrorLength":        0,
	"evalStringLength":        33,
	"playwrightDetected":      false,
	"phantomDetected":         false,
	"nightmareDetected":       false,
	"seleniumDetected":        false,
	"puppeteerDetected":       false,
	"maxTouchPoints":          0,
	"deviceMemory":            32,
	"permissionsState":        "prompt",
	"notificationPermission":  "default",
	"devicePixelRatio":        2,
	"pluginsLength":           5,
	"mimeTypesCount":          2,
	"documentHidden":          false,
	"documentVisibilityState": "visible",
	"mediaPreferences": map[string]any{
		"colorScheme":         "light",
		"reducedMotion":       false,
		"reducedTransparency": false,
		"contrast":            "no-preference",
		"colorGamut":          "srgb",
		"hdr":                 false,
		"forcedColors":        false,
		"invertedColors":      false,
	},
	"webGLVendor":   "Google Inc. (AMD)",
	"webGLRenderer": "ANGLE (AMD, ANGLE Metal Renderer: AMD Radeon Pro Vega 56, Unspecified Version)",
	"minimalSurface": map[string]any{
		"windowFeaturesHash":   "7ZlDkS4XSzhc7j_SiDfD7-1GMaX2AI0ZyRUOAU4NMRU",
		"windowFeaturesCount":  1232,
		"cssKeysHash":          "UAhS1k0c42liTkXoIE2izXONILW_9ICgFPXFaJu57Io",
		"cssKeysCount":         1417,
		"voicesHash":           "kVsqtlSDdjG5FhBOTCCWZvRTHXO2HPi_hWGXwhMxjKs",
		"voicesLocalCount":     157,
		"voicesRemoteCount":    0,
		"voicesLanguagesCount": 44,
		"mediaMimeHash":        "kZhx7yaWeGtKjuD7WOEr3WTtuDBSuluHtsXg3uGZGSw",
		"mediaMimeCount":       9,
		"fontsHash":            "KKOH-MadTM3NkNbZFFNzb0z9rfm31vV1-jm4nnzLXME",
		"fontsCount":           12,
	},
	"worker": map[string]any{
		"ok":                  true,
		"hardwareConcurrency": 8,
		"platform":            "MacIntel",
		"userAgent":           "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36",
		"language":            "en-US",
		"webGLRenderer":       "ANGLE (AMD, ANGLE Metal Renderer: AMD Radeon Pro Vega 56, Unspecified Version)",
		"webGLVendor":         "Google Inc. (AMD)",
	},
	"canvasHash":                    "4YhLamQElolKxGl8v1tiN2249GslEkFQ82NszH8EwmAY",
	"audioHash":                     "bb1tAVZjFvSo0N2J7OH_wPkZ454736NgEx4JFFKrk2g",
	"mathHash":                      "zK9Dnt1lDRxVL5kry2U6ScxY6YdeZxVK_WYAmIyFCC8",
	"intlHash":                      "PgvUrG5mEHTuhCKMFlus_FpuRL5C5HWvZNCcvBTk5rA",
	"webGLParamsHash":               "PVoZZyt4y2NyTssh4HhiAHybsMZh32ofGi_rgd4izjU",
	"puppeteerDocumentNotAvailable": false,
}

// buildSignals renders the signals fixture as base64-encoded JSON.
// createdAtMs is set 10-15s before submittedAtMs so the payload looks
// like a page that mounted, waited for the user to type an email, and
// submitted — not a bot that fired the form in a single tick.
func buildSignals(now time.Time) []byte {
	submittedMs := now.UnixMilli()
	// 14.4s of "user thinking time" — a bit under the Python reference's
	// captured value (14.385s) and inside the 10-30s human-ish band.
	createdMs := submittedMs - 14385

	payload := make(map[string]any, len(signalsFixture)+2)
	for k, v := range signalsFixture {
		payload[k] = v
	}
	payload["createdAtMs"] = createdMs
	payload["submittedAtMs"] = submittedMs

	raw, err := json.Marshal(payload)
	if err != nil {
		// Fixture is a static map[string]any of strings/numbers/bools —
		// json.Marshal cannot fail here. If it does, callers get an
		// empty payload and the authenticator will 400 the request,
		// which is a clearer signal than a panic.
		return nil
	}
	// Base64 as URL-safe? The Python capture used standard base64 with
	// padding; keep it identical.
	enc := base64.StdEncoding.EncodeToString(raw)
	return []byte(enc)
}
