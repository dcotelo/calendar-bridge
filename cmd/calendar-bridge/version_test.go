package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintVersion_ReportsBuildAndPlatform(t *testing.T) {
	var buf bytes.Buffer
	printVersion(&buf)
	out := buf.String()

	for _, want := range []string{"calendar-bridge ", "go:", "platform:"} {
		if !strings.Contains(out, want) {
			t.Errorf("version output is missing %q:\n%s", want, out)
		}
	}
}

// The ldflags in .goreleaser.yml target these exact symbols. If they are ever
// renamed, released binaries silently lose their version, which is how the
// project got here in the first place.
func TestBuildInfo_LinkerTargetsExist(t *testing.T) {
	orig := version
	t.Cleanup(func() { version = orig })

	version = "v9.9.9"
	if got := versionString(); !strings.HasPrefix(got, "v9.9.9") {
		t.Errorf("versionString() = %q, want it to start with the linker-injected version", got)
	}
}

func TestUsage_IsWrittenInFull(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	out := buf.String()

	// Every subcommand main() dispatches on must appear in the help text.
	for _, cmd := range []string{"auth", "sync-once", "run", "ui", "version"} {
		if !strings.Contains(out, "calendar-bridge "+cmd) {
			t.Errorf("usage text does not document the %q subcommand", cmd)
		}
	}
	for _, flag := range []string{"-config", "-dry-run", "-json"} {
		if !strings.Contains(out, flag) {
			t.Errorf("usage text does not document the %q flag", flag)
		}
	}
	if !strings.Contains(out, "Exit codes:") {
		t.Error("usage text does not document the exit codes")
	}
}
