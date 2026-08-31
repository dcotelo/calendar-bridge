package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The CLI's contract is made of things a unit test cannot observe: which
// stream output lands on, and the process exit code. parseFlags,
// reportSetupError and the -json emitters all end in os.Exit, so the only
// honest way to test them is to run the real thing.
//
// TestMain re-executes this test binary with cbCLIArgs set, dispatches through
// the real main(), and lets it exit however it would in production.
const cbCLIArgs = "CALENDAR_BRIDGE_TEST_CLI_ARGS"

func TestMain(m *testing.M) {
	if raw := os.Getenv(cbCLIArgs); raw != "" {
		var args []string
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			panic("decoding " + cbCLIArgs + ": " + err.Error())
		}
		os.Args = append([]string{"calendar-bridge"}, args...)
		main()
		// main() returns only on the success paths, which exit 0.
		os.Exit(exitOK)
	}
	os.Exit(m.Run())
}

type cliResult struct {
	stdout string
	stderr string
	code   int
}

// runCLI runs the real command in a child process and reports what a shell
// would have seen.
func runCLI(t *testing.T, args ...string) cliResult {
	t.Helper()

	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("encoding args: %v", err)
	}

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("locating the test binary: %v", err)
	}

	// -test.run matches nothing, so the child does no test work: TestMain
	// dispatches into main() and never reaches m.Run().
	//
	// The deadline bounds a child that hangs instead of exiting, so a
	// regression shows up as a failed test rather than a stuck suite.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	// #nosec G204 -- self is this test binary's own path from os.Executable,
	// not attacker-controlled input, and the arguments are a fixed literal.
	// The command under test is reached through the environment, not argv.
	cmd := exec.CommandContext(ctx, self, "-test.run=XXX_NO_SUCH_TEST_XXX")
	cmd.Env = append(os.Environ(), cbCLIArgs+"="+string(encoded))

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	code := 0
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) {
			t.Fatalf("running %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return cliResult{stdout: stdout.String(), stderr: stderr.String(), code: code}
}

// Help goes to STDOUT with exit 0 so `calendar-bridge sync-once -h | less`
// works; every other parse failure goes to STDERR with exit 2. Neither is
// observable without running the process.
func TestCLI_FlagRoutingAndExitCodes(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		wantCode   int
		wantStdout string
		wantStderr string
	}{
		{
			name:       "help goes to stdout and exits 0",
			args:       []string{"sync-once", "-h"},
			wantCode:   exitOK,
			wantStdout: "Usage of sync-once:",
		},
		{
			name:       "an invalid flag goes to stderr and exits 2",
			args:       []string{"sync-once", "-bogus"},
			wantCode:   exitUsage,
			wantStderr: "flag provided but not defined: -bogus",
		},
		{
			// The ordering regression: a pre-scan for -h would print help and
			// exit 0, swallowing the invalid flag.
			name:       "an invalid flag before -h still fails",
			args:       []string{"sync-once", "-bogus", "-h"},
			wantCode:   exitUsage,
			wantStderr: "flag provided but not defined: -bogus",
		},
		{
			name:       "a stray positional argument is rejected",
			args:       []string{"sync-once", "-dry-run", "typo"},
			wantCode:   exitUsage,
			wantStderr: `unexpected argument "typo"`,
		},
		{
			// The dangerous ordering: parsing stops at the stray argument, so
			// -dry-run is never seen and a real sync would run.
			name:       "a stray argument ahead of -dry-run is rejected",
			args:       []string{"sync-once", "typo", "-dry-run"},
			wantCode:   exitUsage,
			wantStderr: `unexpected argument "typo"`,
		},
		{
			name:       "an unknown subcommand exits 2",
			args:       []string{"nonesuch"},
			wantCode:   exitUsage,
			wantStderr: `unknown command "nonesuch"`,
		},
		{
			name:       "top-level help goes to stdout and exits 0",
			args:       []string{"--help"},
			wantCode:   exitOK,
			wantStdout: "calendar-bridge - self-hosted busy-time sync",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runCLI(t, tc.args...)

			if got.code != tc.wantCode {
				t.Errorf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s",
					got.code, tc.wantCode, got.stdout, got.stderr)
			}
			if tc.wantStdout != "" {
				if !strings.Contains(got.stdout, tc.wantStdout) {
					t.Errorf("stdout missing %q, got:\n%s", tc.wantStdout, got.stdout)
				}
				// Help on stdout means stdout ONLY — the point of the routing.
				if strings.Contains(got.stderr, tc.wantStdout) {
					t.Errorf("help also written to stderr:\n%s", got.stderr)
				}
			}
			if tc.wantStderr != "" {
				if !strings.Contains(got.stderr, tc.wantStderr) {
					t.Errorf("stderr missing %q, got:\n%s", tc.wantStderr, got.stderr)
				}
				if strings.Contains(got.stdout, tc.wantStderr) {
					t.Errorf("error text also written to stdout:\n%s", got.stdout)
				}
			}
		})
	}
}

// -json must produce exactly one decodable object on stdout on EVERY exit,
// including the failures that happen before a pass can start. A consumer that
// has to parse stderr, or infer from the exit code alone, does not have a
// machine-readable interface.
func TestCLI_JSONReportOnSetupFailures(t *testing.T) {
	t.Run("missing config file", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "sEcReTdIr", "config.yaml")
		got := runCLI(t, "sync-once", "-config", missing, "-json")

		if got.code != exitConfig {
			t.Errorf("exit code = %d, want %d (stderr: %s)", got.code, exitConfig, got.stderr)
		}

		var rep passReport
		dec := json.NewDecoder(strings.NewReader(got.stdout))
		if err := dec.Decode(&rep); err != nil {
			t.Fatalf("stdout is not one decodable JSON object: %v\nstdout:\n%s", err, got.stdout)
		}
		if dec.More() {
			t.Error("stdout carries more than one JSON object")
		}

		if rep.OK {
			t.Error("ok = true for a failed setup")
		}
		if rep.Error == "" {
			t.Error("error is empty; a consumer has nothing to act on")
		}
		if rep.Version == "" {
			t.Error("version is empty")
		}
		// The report must not disclose the server's filesystem layout.
		if strings.Contains(got.stdout, "sEcReTdIr") {
			t.Errorf("the JSON report discloses the config path:\n%s", got.stdout)
		}
	})

	t.Run("account needs authorization", func(t *testing.T) {
		dir := t.TempDir()
		cfg := filepath.Join(dir, "config.yaml")
		creds := filepath.Join(dir, "creds.json")

		if err := os.WriteFile(creds, []byte(`{"installed":{"client_id":"x","client_secret":"y","redirect_uris":["http://localhost"],"auth_uri":"https://example.invalid/auth","token_uri":"https://example.invalid/token"}}`), 0o600); err != nil {
			t.Fatalf("seed credentials: %v", err)
		}
		// Both accounts reference a token file that does not exist, so setup
		// fails with ErrNeedsAuth before any network call.
		body := "accounts:\n" +
			"  - name: a\n    credentials_file: " + creds + "\n    token_file: " + filepath.Join(dir, "a.json") + "\n    calendar_id: primary\n" +
			"  - name: b\n    credentials_file: " + creds + "\n    token_file: " + filepath.Join(dir, "b.json") + "\n    calendar_id: primary\n"
		if err := os.WriteFile(cfg, []byte(body), 0o600); err != nil {
			t.Fatalf("seed config: %v", err)
		}

		got := runCLI(t, "sync-once", "-config", cfg, "-json")

		if got.code != exitAuth {
			t.Errorf("exit code = %d, want %d (stderr: %s)", got.code, exitAuth, got.stderr)
		}

		var rep passReport
		if err := json.Unmarshal([]byte(got.stdout), &rep); err != nil {
			t.Fatalf("stdout is not a decodable JSON object: %v\nstdout:\n%s", err, got.stdout)
		}
		if rep.OK {
			t.Error("ok = true for an account that needs authorization")
		}
		if rep.Error == "" {
			t.Error("error is empty")
		}
		// The operator-facing guidance belongs on stderr, not in the object.
		if !strings.Contains(got.stderr, "calendar-bridge auth") {
			t.Errorf("stderr missing the remediation hint:\n%s", got.stderr)
		}
	})
}
