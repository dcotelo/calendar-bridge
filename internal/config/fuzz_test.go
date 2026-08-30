package config

import (
	"net"
	"os"
	"path/filepath"
	"testing"
)

// FuzzLoad throws arbitrary bytes at the config parser. The contract under
// test is not "every input parses" — most won't — but that Load never panics
// and never returns a config that Validate would reject. A config file is
// operator-controlled, so this is about robustness against a corrupted or
// half-written file rather than against an attacker.
func FuzzLoad(f *testing.F) {
	f.Add(`accounts:
  - name: a
    credentials_file: c.json
    token_file: t.json
    calendar_id: primary
  - name: b
    credentials_file: c2.json
    token_file: t2.json
    calendar_id: primary
poll_interval: 5m
lookahead_days: 30
`)
	f.Add("accounts: []\n")
	f.Add("accounts:\n  - name: a\n")
	f.Add("poll_interval: -1s\n")
	f.Add("lookahead_days: -99999999999999\n")
	f.Add("webhook:\n  enabled: true\n  public_url: \"http://x\"\n")
	f.Add("web_ui:\n  listen_addr: \"[::1\"\n")
	f.Add("\x00\x00\x00")
	f.Add("!!binary |\n  aGVsbG8=\n")
	f.Add("a: &x [*x]\n") // self-referential anchor

	dir := f.TempDir()
	path := filepath.Join(dir, "config.yaml")

	f.Fuzz(func(t *testing.T, contents string) {
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Skip()
		}
		cfg, err := Load(path)
		if err != nil {
			if cfg != nil {
				t.Fatalf("Load returned both a config and an error (%v)", err)
			}
			return
		}
		if cfg == nil {
			t.Fatal("Load returned a nil config and a nil error")
		}
		// A successfully loaded config must always be valid, and defaults must
		// always have been applied.
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Load accepted a config that fails Validate: %v", err)
		}
		if cfg.PollInterval == "" || cfg.LookaheadDays == 0 || cfg.BlockTitle == "" {
			t.Fatalf("Load returned a config with unapplied defaults: %+v", cfg)
		}
		if cfg.WebUI.ListenAddr == "" {
			t.Fatal("Load returned a config with no web_ui.listen_addr default")
		}
	})
}

// FuzzIsLoopbackAddr checks the bind guard never panics and never reports a
// non-loopback address as loopback. This one IS security-relevant: it is the
// check that stops the admin UI binding a public interface.
func FuzzIsLoopbackAddr(f *testing.F) {
	for _, s := range []string{
		"127.0.0.1:8090", "localhost:8090", "[::1]:8090", "0.0.0.0:8090",
		":8090", "", "[::1", "::1]", "example.com:80", "127.0.0.1",
		"[::ffff:127.0.0.1]:8090", "[::ffff:8.8.8.8]:80", "999.999.999.999:1",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, addr string) {
		if !IsLoopbackAddr(addr) {
			return
		}
		// If it says loopback, it must genuinely parse as a loopback authority.
		host, _, err := splitHostPortForTest(addr)
		if err != nil {
			t.Fatalf("IsLoopbackAddr(%q) = true, but the address does not split into host:port", addr)
		}
		if host == "" {
			t.Fatalf("IsLoopbackAddr(%q) = true for an empty host, which binds every interface", addr)
		}
		if host != "localhost" && !parseIPIsLoopbackForTest(host) {
			t.Fatalf("IsLoopbackAddr(%q) = true but host %q is not a loopback address", addr, host)
		}
	})
}

func splitHostPortForTest(addr string) (string, string, error) { return net.SplitHostPort(addr) }

func parseIPIsLoopbackForTest(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
