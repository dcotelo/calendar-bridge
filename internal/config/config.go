// Package config loads calendar-bridge configuration from a YAML file and
// environment variable overrides.
package config

import (
	"fmt"
	"net"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Account represents one Google account whose calendar we sync.
type Account struct {
	// Name is a short human-readable identifier for logs (e.g. "personal",
	// "work-acme"). Not the email address.
	Name string `yaml:"name"`

	// CredentialsFile is the path to the OAuth2 client credentials JSON
	// downloaded from Google Cloud Console (Desktop app type).
	CredentialsFile string `yaml:"credentials_file"`

	// TokenFile is where the OAuth2 token (obtained via the auth flow) is
	// stored/read for this account. One token file per account.
	TokenFile string `yaml:"token_file"`

	// CalendarID is the calendar to read/write on this account. Use
	// "primary" for the account's default calendar.
	CalendarID string `yaml:"calendar_id"`
}

// Config is the top-level calendar-bridge configuration.
type Config struct {
	// Accounts to sync busy time across. Minimum 2.
	Accounts []Account `yaml:"accounts"`

	// PollInterval controls how often each calendar is polled for changes,
	// expressed as a Go duration string (e.g. "5m").
	PollInterval string `yaml:"poll_interval"`

	// LookaheadDays controls how many days into the future events are
	// synced.
	LookaheadDays int `yaml:"lookahead_days"`

	// BlockTitle is the title used for synced busy blocks.
	BlockTitle string `yaml:"block_title"`

	// WebUI configures the optional local configuration web UI (see the `ui`
	// subcommand and internal/webui). Disabled unless explicitly enabled.
	WebUI WebUI `yaml:"web_ui"`
}

// WebUI configures the local configuration management web UI.
//
// Security model: the UI can read and write config.yaml, so it is treated as
// a privileged local admin surface. It binds loopback (127.0.0.1) by default
// and REFUSES to bind a non-loopback address unless AuthToken is set, so it is
// never silently exposed to a network without authentication. Even so, it
// never reads or serves credential/token file *contents* — those stay on disk;
// the UI only edits the file paths, exactly like editing config.yaml by hand.
type WebUI struct {
	// Enabled turns the `ui` server on. When false the subcommand refuses to
	// start.
	Enabled bool `yaml:"enabled"`

	// ListenAddr is the address the UI binds. Defaults to "127.0.0.1:8090".
	// A non-loopback host (e.g. "0.0.0.0:8090") is only permitted when
	// AuthToken is set.
	ListenAddr string `yaml:"listen_addr"`

	// AuthToken, when set, is required as a Bearer token on every request
	// (compared in constant time). It is mandatory to bind a non-loopback
	// address. Treat it as a credential.
	AuthToken string `yaml:"auth_token"`
}

// Load reads and parses a YAML config file at path.
func Load(path string) (*Config, error) {
	// #nosec G304 -- path is an explicit CLI flag the operator passes to
	// their own calendar-bridge invocation, not untrusted external input.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file %s: %w", path, err)
	}

	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parsing config file %s: %w", path, err)
	}

	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config: %w", err)
	}

	cfg.applyDefaults()
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.PollInterval == "" {
		c.PollInterval = "5m"
	}
	if c.LookaheadDays == 0 {
		c.LookaheadDays = 30
	}
	if c.BlockTitle == "" {
		c.BlockTitle = "Busy (calendar-bridge)"
	}
	if c.WebUI.ListenAddr == "" {
		c.WebUI.ListenAddr = "127.0.0.1:8090"
	}
}

// Save validates the config and writes it back to path as YAML, atomically
// and with owner-only (0600) permissions.
//
// It writes to a temporary file in the same directory and renames it into
// place, so a crash mid-write can never leave a truncated or half-written
// config. It validates BEFORE touching disk, so an invalid config (e.g.
// fewer than 2 accounts, missing required fields) is rejected without
// clobbering the existing file. The file is written 0600 because it may
// contain the WebUI auth token and always references credential/token paths.
func (c *Config) Save(path string) error {
	if err := c.Validate(); err != nil {
		return fmt.Errorf("refusing to save invalid config: %w", err)
	}

	data, err := yaml.Marshal(c)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".config-*.yaml.tmp")
	if err != nil {
		return fmt.Errorf("creating temp config file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds.
	defer func() { _ = os.Remove(tmpName) }()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp config file to 0600: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp config file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp config file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomically replacing config file %s: %w", path, err)
	}
	return nil
}

// IsLoopbackAddr reports whether host:port addr binds only the loopback
// interface. Used to decide whether the WebUI may start without an auth
// token. An empty host (":8090") binds all interfaces and is NOT loopback.
func IsLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "" {
		return false // ":port" binds all interfaces
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Validate checks the config for obvious mistakes before the sync engine
// starts.
func (c *Config) Validate() error {
	if len(c.Accounts) < 2 {
		return fmt.Errorf("need at least 2 accounts to sync, got %d", len(c.Accounts))
	}

	seen := make(map[string]bool, len(c.Accounts))
	for i, a := range c.Accounts {
		if a.Name == "" {
			return fmt.Errorf("accounts[%d]: name is required", i)
		}
		if seen[a.Name] {
			return fmt.Errorf("accounts[%d]: duplicate account name %q", i, a.Name)
		}
		seen[a.Name] = true

		if a.CredentialsFile == "" {
			return fmt.Errorf("accounts[%d] (%s): credentials_file is required", i, a.Name)
		}
		if a.TokenFile == "" {
			return fmt.Errorf("accounts[%d] (%s): token_file is required", i, a.Name)
		}
		if a.CalendarID == "" {
			return fmt.Errorf("accounts[%d] (%s): calendar_id is required", i, a.Name)
		}
	}
	return nil
}
