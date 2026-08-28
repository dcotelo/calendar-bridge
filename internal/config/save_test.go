package config

import (
	"os"
	"path/filepath"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Accounts: []Account{
			{Name: "personal", CredentialsFile: "a.json", TokenFile: "a-tok.json", CalendarID: "primary"},
			{Name: "work", CredentialsFile: "b.json", TokenFile: "b-tok.json", CalendarID: "primary"},
		},
		PollInterval:  "5m",
		LookaheadDays: 30,
		BlockTitle:    "Busy",
	}
}

func TestSave_RoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	orig := validConfig()
	orig.BlockTitle = "Do Not Disturb"
	if err := orig.Save(path); err != nil {
		t.Fatalf("Save() error = %v, want nil", err)
	}

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load() after Save error = %v", err)
	}
	if loaded.BlockTitle != "Do Not Disturb" {
		t.Errorf("round-tripped BlockTitle = %q, want %q", loaded.BlockTitle, "Do Not Disturb")
	}
	if len(loaded.Accounts) != 2 {
		t.Errorf("round-tripped accounts = %d, want 2", len(loaded.Accounts))
	}
}

func TestSave_WritesOwnerOnlyPermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := validConfig().Save(path); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("config file mode = %o, want 0600", got)
	}
}

func TestSave_RejectsInvalidWithoutClobbering(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")

	// Seed a valid file first.
	if err := validConfig().Save(path); err != nil {
		t.Fatalf("seeding valid config: %v", err)
	}
	// #nosec G304 -- test-only read of a temp file this test just created.
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded: %v", err)
	}

	// Attempt to save an invalid config (one account -> fails Validate).
	invalid := validConfig()
	invalid.Accounts = invalid.Accounts[:1]
	if err := invalid.Save(path); err == nil {
		t.Fatal("Save() error = nil, want error for invalid config")
	}

	// The original file must be untouched (no partial write / clobber).
	// #nosec G304 -- test-only read of a temp file this test just created.
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after failed save: %v", err)
	}
	if string(before) != string(after) {
		t.Error("config file was modified by a failed Save; want it left intact")
	}

	// And no stray temp files left behind.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	for _, e := range entries {
		if e.Name() != "config.yaml" {
			t.Errorf("stray file left after failed save: %q", e.Name())
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	cases := []struct {
		addr string
		want bool
	}{
		{"127.0.0.1:8090", true},
		{"localhost:8090", true},
		{"[::1]:8090", true},
		{"0.0.0.0:8090", false},
		{"192.168.1.10:8090", false},
		{":8090", false}, // all interfaces
		{"example.com:8090", false},
		{"garbage", false},
	}
	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := IsLoopbackAddr(tc.addr); got != tc.want {
				t.Errorf("IsLoopbackAddr(%q) = %v, want %v", tc.addr, got, tc.want)
			}
		})
	}
}

func TestLoad_WebUIDefaultListenAddr(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := validConfig().Save(path); err != nil {
		t.Fatalf("save: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.WebUI.ListenAddr != "127.0.0.1:8090" {
		t.Errorf("default WebUI.ListenAddr = %q, want 127.0.0.1:8090", cfg.WebUI.ListenAddr)
	}
}
