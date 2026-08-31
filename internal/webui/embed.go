package webui

import (
	"bytes"
	_ "embed"
	"fmt"
)

// indexHTML is the single-page configuration UI, embedded at build time so the
// binary is fully self-contained (no external assets, no CDN).
//
//go:embed static/index.html
var indexHTML []byte

// The page's inline <style> and <script> are authorised by a per-response CSP
// nonce, which is substituted into these placeholders. If an edit ever removes
// them, the CSP would block the page's own style and script and the UI would
// render as unstyled, inert HTML — fail at startup instead, loudly.
func init() {
	if n := bytes.Count(indexHTML, []byte(noncePlaceholder)); n != 2 {
		panic(fmt.Sprintf("webui: static/index.html has %d %s placeholders, want 2 "+
			"(one on the inline <style>, one on the inline <script>)", n, noncePlaceholder))
	}
}
