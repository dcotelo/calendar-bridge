package config

import (
	"os"
	"path/filepath"
	"testing"
)

func writeTempConfig(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

func TestLoad_Valid(t *testing.T) {
	path := writeTempConfig(t, `
accounts:
  - name: personal
    credentials_file: personal-creds.json
    token_file: personal-token.json
    calendar_id: primary
  - name: work
    credentials_file: work-creds.json
    token_file: work-token.json
    calendar_id: primary
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if len(cfg.Accounts) != 2 {
		t.Fatalf("len(Accounts) = %d, want 2", len(cfg.Accounts))
	}
	if cfg.PollInterval != "5m" {
		t.Errorf("default PollInterval = %q, want %q", cfg.PollInterval, "5m")
	}
	if cfg.LookaheadDays != 30 {
		t.Errorf("default LookaheadDays = %d, want 30", cfg.LookaheadDays)
	}
	if cfg.BlockTitle == "" {
		t.Error("default BlockTitle should not be empty")
	}
}

func TestLoad_TooFewAccounts(t *testing.T) {
	path := writeTempConfig(t, `
accounts:
  - name: personal
    credentials_file: personal-creds.json
    token_file: personal-token.json
    calendar_id: primary
`)

	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for single-account config")
	}
}

func TestLoad_DuplicateAccountNames(t *testing.T) {
	path := writeTempConfig(t, `
accounts:
  - name: personal
    credentials_file: a.json
    token_file: a-tok.json
    calendar_id: primary
  - name: personal
    credentials_file: b.json
    token_file: b-tok.json
    calendar_id: primary
`)

	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for duplicate account names")
	}
}

func TestLoad_MissingRequiredField(t *testing.T) {
	path := writeTempConfig(t, `
accounts:
  - name: personal
    token_file: a-tok.json
    calendar_id: primary
  - name: work
    credentials_file: b.json
    token_file: b-tok.json
    calendar_id: primary
`)

	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for missing credentials_file")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	if _, err := Load("/nonexistent/config.yaml"); err == nil {
		t.Error("Load() error = nil, want error for missing config file")
	}
}

func TestLoad_CustomOverrides(t *testing.T) {
	path := writeTempConfig(t, `
accounts:
  - name: personal
    credentials_file: a.json
    token_file: a-tok.json
    calendar_id: primary
  - name: work
    credentials_file: b.json
    token_file: b-tok.json
    calendar_id: primary
poll_interval: 10m
lookahead_days: 60
block_title: "Do Not Disturb"
`)

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if cfg.PollInterval != "10m" {
		t.Errorf("PollInterval = %q, want %q", cfg.PollInterval, "10m")
	}
	if cfg.LookaheadDays != 60 {
		t.Errorf("LookaheadDays = %d, want 60", cfg.LookaheadDays)
	}
	if cfg.BlockTitle != "Do Not Disturb" {
		t.Errorf("BlockTitle = %q, want %q", cfg.BlockTitle, "Do Not Disturb")
	}
}
