package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"testing"
)

// parseFlags routes help to stdout and parse errors to stderr, and must
// preserve the flag package's LEFT-TO-RIGHT ordering.
//
// An earlier version pre-scanned args for -h before parsing, which accepted
// `sync-once -bogus -h` and printed help — swallowing an invalid flag that
// should have failed. These assert the ordering directly, without exercising
// the os.Exit paths (which a unit test cannot).
//
// These pin the PREMISE parseFlags is built on — what flag.FlagSet does with
// each input — so a change in that behaviour is caught here rather than
// surfacing as a confusing failure elsewhere. parseFlags ITSELF, including the
// stream each outcome is written to and the exit code it produces, is covered
// in cli_test.go by running the real binary as a subprocess. Both are needed:
// these are fast and precise about ordering, those are the only way to observe
// an os.Exit.
func TestFlagParsing_OrderingAndErrorClassification(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantHelp bool
		wantErr  bool
	}{
		{"no args", nil, false, false},
		{"valid flag", []string{"-config", "x.yaml"}, false, false},
		{"help alone", []string{"-h"}, true, false},
		{"long help", []string{"--help"}, true, false},
		// The regression: the invalid flag comes first, so it must win.
		{"invalid flag before help", []string{"-bogus", "-h"}, false, true},
		// Help comes first, so parsing stops there — matching the flag
		// package's own behaviour rather than second-guessing it.
		{"help before invalid flag", []string{"-h", "-bogus"}, true, false},
		{"invalid flag alone", []string{"-bogus"}, false, true},
		{"terminator stops flag parsing", []string{"--", "-h"}, false, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("probe", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String("config", "config.yaml", "path to config file")

			err := fs.Parse(tc.args)

			gotHelp := errors.Is(err, flag.ErrHelp)
			gotErr := err != nil && !gotHelp

			if gotHelp != tc.wantHelp {
				t.Errorf("help = %v, want %v (err = %v)", gotHelp, tc.wantHelp, err)
			}
			if gotErr != tc.wantErr {
				t.Errorf("parse error = %v, want %v (err = %v)", gotErr, tc.wantErr, err)
			}
		})
	}
}

// The two outcomes must be distinguishable by the caller, because they go to
// different streams and different exit codes.
func TestFlagParsing_HelpAndErrorAreDistinguishable(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	var buf bytes.Buffer
	fs.SetOutput(&buf)
	fs.String("config", "config.yaml", "path to config file")

	if err := fs.Parse([]string{"-h"}); !errors.Is(err, flag.ErrHelp) {
		t.Errorf("-h gave %v, want flag.ErrHelp so it can be routed to stdout with exit 0", err)
	}

	fs2 := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs2.SetOutput(&buf)
	fs2.String("config", "config.yaml", "path to config file")

	err := fs2.Parse([]string{"-bogus"})
	if err == nil {
		t.Fatal("an unknown flag must produce an error")
	}
	if errors.Is(err, flag.ErrHelp) {
		t.Error("an unknown flag must NOT be classified as a help request; it exits 2, not 0")
	}
}

// flag.FlagSet.Parse stops at the first non-flag argument and leaves the rest
// in Args() without complaining. No subcommand takes positional arguments —
// auth names its account with -account — so anything left over is a typo.
//
// Ignoring it is dangerous, not merely untidy: parsing stops AT the stray
// argument, so every flag after it is silently dropped. In
// `sync-once typo -dry-run` the -dry-run never registers and a real sync
// writes to live calendars when the operator asked for a dry run. These
// assert what is left over; parseFlags turns a non-empty result into exit 2.
func TestFlagParsing_LeftoverPositionalArgumentsAreDetected(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"flags only", []string{"-config", "x.yaml", "-dry-run"}, nil},
		{"no args", nil, nil},
		{"trailing typo", []string{"-config", "x.yaml", "typo"}, []string{"typo"}},

		// The dangerous ordering: -dry-run is never parsed.
		{"typo before a flag", []string{"typo", "-dry-run"}, []string{"typo", "-dry-run"}},

		{"bare subcommand-looking word", []string{"status"}, []string{"status"}},
		{"terminator then a flag-like word", []string{"--", "-h"}, []string{"-h"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("probe", flag.ContinueOnError)
			fs.SetOutput(io.Discard)
			fs.String("config", "config.yaml", "path to config file")
			fs.Bool("dry-run", false, "dry run")

			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("Parse(%q): %v", tc.args, err)
			}

			got := fs.Args()
			if len(got) != len(tc.want) {
				t.Fatalf("Args() = %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("Args() = %q, want %q", got, tc.want)
				}
			}
		})
	}
}

// The specific failure this guards, spelled out on its own: a stray argument
// ahead of -dry-run leaves dry-run false, which would mean writing to live
// calendars. parseFlags rejects the command before it can run.
func TestFlagParsing_StrayArgumentSuppressesDryRun(t *testing.T) {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dryRun := fs.Bool("dry-run", false, "dry run")

	if err := fs.Parse([]string{"typo", "-dry-run"}); err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if *dryRun {
		t.Fatal("precondition failed: -dry-run parsed despite the leading stray argument")
	}
	if len(fs.Args()) == 0 {
		t.Fatal("no leftover arguments, so parseFlags would let this run as a live sync")
	}
}
