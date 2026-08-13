package auth

import (
	"encoding/base64"
	"fmt"
	"time"
)

// GenerateChecksum computes the x-cursor-checksum HTTP header value using the
// exact algorithm extracted from Cursor 3.10.20 workbench.desktop.main.js
// (function nVg + tVg). The algorithm is unchanged through 3.15.19
// (the minifier symbol is Bvg in that build), so the same code serves all
// supported Cursor lines. See docs/checksum-algorithm.md for the
// derivation.
//
// Format: <base64(obfuscated 6-byte timestamp)><machineId>[/<macMachineId>]
//
// IMPORTANT: In Cursor IDE, the timestamp is snapshotted once at IDE startup
// and reused for the lifetime of the session — it is NOT regenerated per
// request. Callers should cache the result across requests within the same
// session.
//
//   - snapshot: the moment when the "session" started (equivalent of IDE launch).
//     Choose a fresh time.Now() and reuse it.
//
//   - machineID: Cursor releaseHash (64-char hex). Version-dependent:
//
//   - 3.10.20: 4071c661bcb367c518becc7b3d4d57cbd69d2291d8b302c558d79080f8fd4f75
//
//   - 3.11.19: bf249e6efb5b097f23d7e21d7283429f0760b74a
//
//   - 3.15.19: de07bee81cefe43461ebf4f40c3d2d78d15052aa
//
//     See KnownReleaseHash_* in auth/machineid.go.
//
//   - macMachineID: SHA-256(IOPlatformUUID) on macOS, empty string for the
//     shorter no-mac form.
func GenerateChecksum(snapshot time.Time, machineID, macMachineID string) string {
	// JS: E = Math.floor(Date.now() / 1e6)
	// Date.now() is milliseconds since epoch; dividing by 1e6 gives ~kiloseconds.
	E := snapshot.UnixMilli() / 1_000_000

	// JS bitwise operators are 32-bit signed. `E >> 40` reduces to `E >> (40%32)`
	// = `E >> 8`. `E >> 32` reduces to `E >> 0` = `E`.
	// So the six emitted bytes are, in JS semantics:
	//   [ (E>>8)&0xff, E&0xff, (E>>24)&0xff, (E>>16)&0xff, (E>>8)&0xff, E&0xff ]
	// We must replicate this exactly.
	e32 := uint32(E) // JS truncates to 32-bit for bitwise ops
	raw := [6]byte{
		byte((e32 >> 8) & 0xff),  // JS: E >> 40
		byte(e32 & 0xff),         // JS: E >> 32
		byte((e32 >> 24) & 0xff), // JS: E >> 24
		byte((e32 >> 16) & 0xff), // JS: E >> 16
		byte((e32 >> 8) & 0xff),  // JS: E >> 8
		byte(e32 & 0xff),         // JS: E
	}

	// tVg obfuscation:
	// t = 165
	// for i in 0..len(e):
	//     e[i] = ((e[i] ^ t) + i) & 0xff
	//     t    = e[i]
	var obf [6]byte
	t := byte(165)
	for n := 0; n < 6; n++ {
		obf[n] = byte(((int(raw[n]) ^ int(t)) + n) & 0xff)
		t = obf[n]
	}

	b64 := base64.StdEncoding.EncodeToString(obf[:])
	// Strip trailing '=' padding (Cursor uses unpadded base64).
	for len(b64) > 0 && b64[len(b64)-1] == '=' {
		b64 = b64[:len(b64)-1]
	}

	if macMachineID == "" {
		return fmt.Sprintf("%s%s", b64, machineID)
	}
	return fmt.Sprintf("%s%s/%s", b64, machineID, macMachineID)
}
