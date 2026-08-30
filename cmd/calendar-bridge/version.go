package main

import (
	"fmt"
	"io"
	"runtime"
	"runtime/debug"
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
			if s.Value == "true" {
				v += "+dirty"
			}
		}
	}
	return v, c, d
}

// printVersion writes a one-line-per-field build report.
func printVersion(w io.Writer) {
	v, c, d := buildInfo()
	fmt.Fprintf(w, "calendar-bridge %s\n", v)
	if c != "" {
		fmt.Fprintf(w, "commit:   %s\n", c)
	}
	if d != "" {
		fmt.Fprintf(w, "built:    %s\n", d)
	}
	fmt.Fprintf(w, "go:       %s\n", runtime.Version())
	fmt.Fprintf(w, "platform: %s/%s\n", runtime.GOOS, runtime.GOARCH)
}

// versionString is the short form used in JSON output.
func versionString() string {
	v, _, _ := buildInfo()
	return v
}
