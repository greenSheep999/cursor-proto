package kernel

// Tests for the per-call fingerprint synthesis in otp_signals.go.
//
// Real Cursor logins go out over the wire and cost real credit, so
// these are all in-process: we drive the generator directly and
// assert the shape/consistency of what comes out.

import (
	"encoding/base64"
	"encoding/json"
	mrand "math/rand"
	"strings"
	"testing"
	"time"
)

// TestBuildSignals_RandomizesEachCall drives buildSignals 20 times and
// asserts that every canvasHash is distinct. Collisions on a 32-byte
// hash space would be astronomically unlikely — a failure here means
// the RNG is stuck (e.g. reused seed) rather than a chance collision.
func TestBuildSignals_RandomizesEachCall(t *testing.T) {
	seen := map[string]struct{}{}
	for i := 0; i < 20; i++ {
		raw := buildSignals(time.Now())
		decoded, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			t.Fatalf("iter %d: base64 decode: %v", i, err)
		}
		var out map[string]any
		if err := json.Unmarshal(decoded, &out); err != nil {
			t.Fatalf("iter %d: json decode: %v", i, err)
		}
		h, ok := out["canvasHash"].(string)
		if !ok {
			t.Fatalf("iter %d: canvasHash missing/not string: %v", i, out["canvasHash"])
		}
		if _, dup := seen[h]; dup {
			t.Fatalf("iter %d: duplicate canvasHash %q — generator not randomizing", i, h)
		}
		seen[h] = struct{}{}
	}
}

// TestGenerateFingerprintProfile_Consistent runs the validator once
// with a fixed seed and then 100 times with fresh seeds. All profiles
// must satisfy the internal-consistency contract (worker mirrors
// top-level, WebGL matches platform, hashes decode to 32 bytes).
func TestGenerateFingerprintProfile_Consistent(t *testing.T) {
	// Fixed seed first — a quick regression trap for future edits.
	fixed := mrand.New(mrand.NewSource(42))
	p := generateFingerprintProfile(fixed)
	if err := validateFingerprintProfile(p); err != nil {
		t.Fatalf("fixed-seed profile invalid: %v", err)
	}

	for i := 0; i < 100; i++ {
		rng := mrand.New(mrand.NewSource(int64(i) * 1_000_003))
		p := generateFingerprintProfile(rng)
		if err := validateFingerprintProfile(p); err != nil {
			t.Fatalf("seed %d: profile invalid: %v", i, err)
		}
	}
}

// TestFingerprintPlatform_MacUsesMacUA asserts the platform/UA/webGL
// cross-consistency over 100 fresh profiles. This is the property
// Cursor's bot-detector actually looks for.
func TestFingerprintPlatform_MacUsesMacUA(t *testing.T) {
	sawMac := false
	sawWin := false
	for i := 0; i < 100; i++ {
		rng := mrand.New(mrand.NewSource(int64(i) + 7919))
		p := generateFingerprintProfile(rng)
		platform, _ := p["platform"].(string)
		ua, _ := p["userAgent"].(string)
		renderer, _ := p["webGLRenderer"].(string)

		switch platform {
		case "MacIntel":
			sawMac = true
			if !strings.Contains(ua, "Macintosh") {
				t.Errorf("seed %d: MacIntel with non-Mac UA: %q", i, ua)
			}
			if !strings.Contains(renderer, "Metal Renderer") {
				t.Errorf("seed %d: MacIntel with non-Metal renderer: %q", i, renderer)
			}
		case "Win32":
			sawWin = true
			if !strings.Contains(ua, "Windows NT") {
				t.Errorf("seed %d: Win32 with non-Windows UA: %q", i, ua)
			}
			if !strings.Contains(renderer, "Direct3D11") {
				t.Errorf("seed %d: Win32 with non-D3D11 renderer: %q", i, renderer)
			}
		default:
			t.Fatalf("seed %d: unexpected platform %q", i, platform)
		}
	}
	if !sawMac {
		t.Error("100 seeds and never rolled MacIntel — pool broken?")
	}
	if !sawWin {
		t.Error("100 seeds and never rolled Win32 — pool broken?")
	}
}

// TestFingerprintHashes_ValidBase64URL decodes every hash field and
// asserts they are 32 random bytes wearing a 43-char base64url coat.
func TestFingerprintHashes_ValidBase64URL(t *testing.T) {
	rng := mrand.New(mrand.NewSource(1234567))
	p := generateFingerprintProfile(rng)

	topKeys := []string{"canvasHash", "audioHash", "mathHash", "intlHash", "webGLParamsHash"}
	for _, k := range topKeys {
		v, ok := p[k].(string)
		if !ok {
			t.Fatalf("%s missing or not a string: %v", k, p[k])
		}
		if len(v) != 43 {
			t.Errorf("%s length = %d, want 43", k, len(v))
		}
		dec, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			t.Errorf("%s not base64url: %v", k, err)
		}
		if len(dec) != 32 {
			t.Errorf("%s decoded to %d bytes, want 32", k, len(dec))
		}
	}

	surface, ok := p["minimalSurface"].(map[string]any)
	if !ok {
		t.Fatal("minimalSurface missing")
	}
	surfaceKeys := []string{"windowFeaturesHash", "cssKeysHash", "voicesHash", "mediaMimeHash", "fontsHash"}
	for _, k := range surfaceKeys {
		v, ok := surface[k].(string)
		if !ok {
			t.Fatalf("minimalSurface.%s missing or not a string: %v", k, surface[k])
		}
		if len(v) != 43 {
			t.Errorf("minimalSurface.%s length = %d, want 43", k, len(v))
		}
		dec, err := base64.RawURLEncoding.DecodeString(v)
		if err != nil {
			t.Errorf("minimalSurface.%s not base64url: %v", k, err)
		}
		if len(dec) != 32 {
			t.Errorf("minimalSurface.%s decoded to %d bytes, want 32", k, len(dec))
		}
	}
}
