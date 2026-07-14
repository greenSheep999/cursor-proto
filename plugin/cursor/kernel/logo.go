// logo.go: embed the plugin logo as a base64 data URI.
//
// CPA's management UI renders plugin.metadata.Logo directly as an
// <img src> — data URIs let us ship the icon inside the plugin
// binary without hosting it elsewhere. The source PNG lives beside
// this file at cursor-logo.png (114x114, matches the size kiro
// plugin uses, keeping the payload under 4KB).

package kernel

import (
	_ "embed"
	"encoding/base64"
	"sync"
)

//go:embed cursor-logo.png
var cursorLogoPNG []byte

var (
	logoOnce sync.Once
	logoURI  string
)

// CursorLogoDataURI returns the plugin logo encoded as a
// `data:image/png;base64,...` URI. Lazily built and cached.
func CursorLogoDataURI() string {
	logoOnce.Do(func() {
		if len(cursorLogoPNG) == 0 {
			return
		}
		logoURI = "data:image/png;base64," + base64.StdEncoding.EncodeToString(cursorLogoPNG)
	})
	return logoURI
}
