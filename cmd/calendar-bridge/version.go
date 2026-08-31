package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
	"strings"
)

// Build information. goreleaser injects these via -ldflags -X; a `go install`
// or `go build` leaves them at their defaults, in which case buildInfo falls
// back to the VCS stamps the Go toolchain embeds automatically.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// buildInfo returns the version, commit and build date, filling any blank from
// runtime/debug build info so a `go install`-ed binary still identifies itself.
func buildInfo() (v, c, d string) {
	v, c, d = version, commit, date
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return v, c, d
	}
	if v == "dev" && info.Main.Version != "" && info.Main.Version != "(devel)" {
		v = info.Main.Version
	}
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			if c == "" {
				c = s.Value
			}
		case "vcs.time":
			if d == "" {
				d = s.Value
			}
		case "vcs.modified":
			// Go already suffixes Main.Version with "+dirty" when it derives a
			// pseudo-version from a dirty tree, so only add it ourselves when
			// it isn't there — otherwise the version reads "…+dirty+dirty".
			if s.Value == "true" && !strings.HasSuffix(v, "+dirty") {
				v += "+dirty"
			}
		}
	}
	return v, c, d
}

// printVersion writes a one-line-per-field build report.
//
// Write errors are ignored deliberately: the only caller writes to stdout, and
// there is no useful recovery from a failed write to it — reporting the failure
// would need the same broken stream.
func printVersion(w io.Writer) {
	v, c, d := buildInfo()
	var b strings.Builder
	fmt.Fprintf(&b, "calendar-bridge %s\n", v)
	if c != "" {
		fmt.Fprintf(&b, "commit:   %s\n", c)
	}
	if d != "" {
		fmt.Fprintf(&b, "built:    %s\n", d)
	}
	fmt.Fprintf(&b, "go:       %s\n", runtime.Version())
	fmt.Fprintf(&b, "platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
	_, _ = io.WriteString(w, b.String())
}

// versionString is the short form used in JSON output.
func versionString() string {
	v, _, _ := buildInfo()
	return v
}
