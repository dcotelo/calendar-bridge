package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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
	// A distinctive directory, so any leak of the supplied path is unambiguous.
	dir := filepath.Join(t.TempDir(), "cOnFiGdIrThAtMuStNoTlEaK")
	missing := filepath.Join(dir, "config.yaml")

	cfg, err := Load(missing)
	if err == nil {
		t.Fatal("Load() error = nil, want error for missing config file")
	}
	// A failed Load must never hand back a partially-populated config: a caller
	// that ignored the error would otherwise run against defaults and no
	// accounts.
	if cfg != nil {
		t.Errorf("Load() returned a config (%+v) alongside an error; it must be nil", cfg)
	}
	// Load's error reaches the daemon's stderr, which under systemd is the
	// journal. The wrapped *fs.PathError embeds the full path regardless of the
	// format string, so this guards the stripping as well as the message.
	if strings.Contains(err.Error(), dir) {
		t.Errorf("Load() error discloses the config directory: %v", err)
	}
}

// Every Load failure mode must stay path-free, not just the missing-file one.
func TestLoad_ErrorsNeverContainTheSuppliedPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "aNoThErCoNfIgDiR")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := map[string]string{
		"unparseable YAML":              "accounts: [oops\n",
		"valid YAML but invalid config": "accounts: []\n",
		"wrong types":                   "accounts: 42\n",
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			_, err := Load(p)
			if err == nil {
				t.Fatalf("Load accepted %q", contents)
			}
			if strings.Contains(err.Error(), dir) {
				t.Errorf("error discloses the config directory: %v", err)
			}
		})
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

func TestLoad_WebhookChannelTTLValidation(t *testing.T) {
	cases := []struct {
		name    string
		ttl     string
		wantErr bool
	}{
		{"unset uses default", "", false},
		{"positive duration", "24h", false},
		{"zero rejected", "0s", true},
		{"negative rejected", "-1h", true},
		{"invalid duration rejected", "not-a-duration", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := twoAccounts + "\nwebhook:\n  enabled: true\n  verification_token: secret\n  public_url: https://cb.example.com\n"
			if tc.ttl != "" {
				body += "  channel_ttl: " + tc.ttl + "\n"
			}
			path := writeTempConfig(t, body)
			_, err := Load(path)
			if tc.wantErr && err == nil {
				t.Errorf("Load() error = nil, want error for channel_ttl %q", tc.ttl)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Load() error = %v, want nil for channel_ttl %q", err, tc.ttl)
			}
		})
	}
}

// A pathological config must fail fast rather than appear to hang.
//
// yaml.Unmarshal is super-quadratic in the number of DUPLICATE keys: on this
// struct, 500 repeated keys parse in ~23ms, 1000 in ~93ms, 2000 in ~447ms,
// 4000 in ~2.9s, and 20000 does not finish in any useful time. A daemon that
// wedges at startup parsing its own config is a much worse failure mode than
// one that exits with a message, because it looks like a network problem.
//
// This surfaced as a 1-in-6 CI failure in the fuzz job — "context deadline
// exceeded" with no failing input — because the fuzzer generates exactly this
// shape.
func TestLoad_RejectsAnOversizedConfigWithoutParsingIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// Comfortably over the cap, in the shape that is expensive to parse.
	if err := os.WriteFile(path, []byte(strings.Repeat("k: v\n", 300000)), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}

	start := time.Now()
	cfg, err := Load(path)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Load accepted a config file over the size limit")
	}
	if cfg != nil {
		t.Errorf("Load returned a config (%+v) alongside an error; it must be nil", cfg)
	}
	// The point is that it rejects on SIZE, before handing the bytes to the
	// parser. Parsing this input takes minutes, so anything near that means
	// the check moved after the parse.
	if elapsed > 2*time.Second {
		t.Errorf("Load took %v; the size check must run before parsing", elapsed)
	}
	// And the error must stay path-free like every other Load error.
	if strings.Contains(err.Error(), dir) {
		t.Errorf("error discloses the config directory: %v", err)
	}
}
