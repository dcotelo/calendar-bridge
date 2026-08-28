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

const twoAccounts = `
accounts:
  - name: personal
    credentials_file: a.json
    token_file: a-tok.json
    calendar_id: primary
  - name: work
    credentials_file: b.json
    token_file: b-tok.json
    calendar_id: primary
`

func TestLoad_WebhookEnabledAppliesDefaults(t *testing.T) {
	path := writeTempConfig(t, twoAccounts+`
webhook:
  enabled: true
  public_url: https://cb.example.com
  verification_token: super-secret
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}
	if !cfg.Webhook.Enabled {
		t.Fatal("Webhook.Enabled = false, want true")
	}
	if cfg.Webhook.ListenAddr != ":8080" {
		t.Errorf("default ListenAddr = %q, want :8080", cfg.Webhook.ListenAddr)
	}
	if cfg.Webhook.ChannelTTL != "24h" {
		t.Errorf("default ChannelTTL = %q, want 24h", cfg.Webhook.ChannelTTL)
	}
	if cfg.Webhook.DebounceInterval != "5s" {
		t.Errorf("default DebounceInterval = %q, want 5s", cfg.Webhook.DebounceInterval)
	}
}

func TestLoad_WebhookRequiresHTTPSPublicURL(t *testing.T) {
	path := writeTempConfig(t, twoAccounts+`
webhook:
  enabled: true
  public_url: http://insecure.example.com
  verification_token: super-secret
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for non-https webhook.public_url")
	}
}

func TestLoad_WebhookRequiresToken(t *testing.T) {
	path := writeTempConfig(t, twoAccounts+`
webhook:
  enabled: true
  public_url: https://cb.example.com
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for missing webhook.verification_token")
	}
}

func TestLoad_WebhookRequiresPublicURL(t *testing.T) {
	path := writeTempConfig(t, twoAccounts+`
webhook:
  enabled: true
  verification_token: super-secret
`)
	if _, err := Load(path); err == nil {
		t.Error("Load() error = nil, want error for missing webhook.public_url")
	}
}

func TestLoad_WebhookDisabledSkipsValidation(t *testing.T) {
	// A disabled webhook block with no fields must not trigger validation.
	path := writeTempConfig(t, twoAccounts+`
webhook:
  enabled: false
`)
	if _, err := Load(path); err != nil {
		t.Errorf("Load() error = %v, want nil (disabled webhook skips validation)", err)
	}
}

func TestLoad_WebhookPublicURLValidation(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"valid https", "https://cb.example.com", false},
		{"valid https with path", "https://cb.example.com/hook", false},
		{"http rejected", "http://cb.example.com", true},
		{"missing host", "https://", true},
		{"user info rejected", "https://user:pass@cb.example.com", true},
		{"query rejected", "https://cb.example.com/?token=abc", true},
		{"fragment rejected", "https://cb.example.com/#frag", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, twoAccounts+"\nwebhook:\n  enabled: true\n  verification_token: secret\n  public_url: "+tc.url+"\n")
			_, err := Load(path)
			if tc.wantErr && err == nil {
				t.Errorf("Load() error = nil, want error for public_url %q", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Load() error = %v, want nil for public_url %q", err, tc.url)
			}
		})
	}
}
