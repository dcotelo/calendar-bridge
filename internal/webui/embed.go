package webui

import _ "embed"

// indexHTML is the single-page configuration UI, embedded at build time so the
// binary is fully self-contained (no external assets, no CDN).
//
//go:embed static/index.html
var indexHTML []byte
