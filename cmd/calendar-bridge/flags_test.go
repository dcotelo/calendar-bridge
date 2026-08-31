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
