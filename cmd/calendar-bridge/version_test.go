package main

import (
	"bytes"
	"encoding/json"
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

// The -json report must agree with the exit code. An interrupted pass exits 0
// because SIGINT/SIGTERM is an intentional shutdown, not a failure — so
// reporting ok=false with an error there would leave a consumer unable to tell
// it apart from a genuine sync failure, which exits 5.
func TestPassReport_InterruptedIsNotAFailure(t *testing.T) {
	cases := []struct {
		name        string
		rep         passReport
		wantOK      bool
		wantErrText bool
	}{
		{
			name:   "clean pass",
			rep:    passReport{OK: true},
			wantOK: true,
		},
		{
			name:        "sync failure",
			rep:         passReport{OK: false, Error: "listing events for account personal: 401"},
			wantOK:      false,
			wantErrText: true,
		},
		{
			// Exits 0. Must not look like the row above.
			name:   "interrupted",
			rep:    passReport{OK: true, Interrupted: true},
			wantOK: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.rep)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got["ok"] != tc.wantOK {
				t.Errorf("ok = %v, want %v", got["ok"], tc.wantOK)
			}
			_, hasErr := got["error"]
			if hasErr != tc.wantErrText {
				t.Errorf("error field present = %v, want %v", hasErr, tc.wantErrText)
			}
			// interrupted is omitempty, so it appears only when true — that is
			// what lets a consumer branch on it.
			_, hasInterrupted := got["interrupted"]
			if hasInterrupted != tc.rep.Interrupted {
				t.Errorf("interrupted field present = %v, want %v", hasInterrupted, tc.rep.Interrupted)
			}
		})
	}
}
