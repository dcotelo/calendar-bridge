// Package config loads calendar-bridge configuration from a YAML file and
// environment variable overrides.
package config

import (
	"fmt"
	"net/url"
	"os"
	"time"

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

	// Webhook, when enabled, opts into Google Calendar push notifications
	// (events.watch) as a near-real-time alternative/supplement to polling.
	// When disabled (the default), calendar-bridge behaves exactly as before:
	// pure polling at PollInterval. See internal/webhook for the design.
	Webhook Webhook `yaml:"webhook"`
}

// Webhook configures Google Calendar push notifications. Push requires a
// publicly reachable HTTPS endpoint that Google can POST change
// notifications to; each watch channel expires and must be renewed. Because
// that adds real infrastructure requirements, it is strictly opt-in.
type Webhook struct {
	// Enabled turns push notifications on. When false, all other fields are
	// ignored and calendar-bridge polls only.
	Enabled bool `yaml:"enabled"`

	// PublicURL is the externally reachable HTTPS base URL Google will POST
	// notifications to (e.g. "https://cb.example.com"). The receiver listens
	// at PublicURL + "/webhook".
	PublicURL string `yaml:"public_url"`

	// ListenAddr is the local address the receiver binds (e.g. "127.0.0.1:8080").
	// Typically behind a TLS-terminating reverse proxy that forwards to it —
	// prefer a loopback address so the plaintext receiver isn't reachable
	// directly, bypassing the proxy's TLS; bind wider only when the proxy
	// runs elsewhere and network-level isolation covers the port instead.
	ListenAddr string `yaml:"listen_addr"`

	// VerificationToken is an opaque secret echoed back by Google in the
	// X-Goog-Channel-Token header of every notification, letting the receiver
	// reject forged requests. Generate a long random string and keep it
	// secret (it is a credential — see SECURITY.md).
	VerificationToken string `yaml:"verification_token"`

	// ChannelTTL is how long each watch channel lives before renewal, as a Go
	// duration (e.g. "24h"). Google caps this; calendar-bridge renews ahead
	// of expiry. Empty means use the provider/library default.
	ChannelTTL string `yaml:"channel_ttl"`

	// DebounceInterval coalesces a burst of notifications into a single sync
	// (e.g. "5s"), so a flurry of edits triggers one reconcile, not dozens.
	DebounceInterval string `yaml:"debounce_interval"`
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
	if c.Webhook.Enabled {
		if c.Webhook.ListenAddr == "" {
			c.Webhook.ListenAddr = ":8080"
		}
		if c.Webhook.ChannelTTL == "" {
			c.Webhook.ChannelTTL = "24h"
		}
		if c.Webhook.DebounceInterval == "" {
			c.Webhook.DebounceInterval = "5s"
		}
	}
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

	if c.Webhook.Enabled {
		if c.Webhook.PublicURL == "" {
			return fmt.Errorf("webhook.public_url is required when webhook.enabled is true")
		}
		u, err := url.Parse(c.Webhook.PublicURL)
		if err != nil {
			return fmt.Errorf("webhook.public_url is not a valid URL: %w", err)
		}
		// Google refuses to deliver push notifications to non-HTTPS URLs.
		if u.Scheme != "https" {
			return fmt.Errorf("webhook.public_url must use the https scheme, got %q", u.Scheme)
		}
		if u.Host == "" {
			return fmt.Errorf("webhook.public_url must include a host")
		}
		// Reject embedded credentials and query/fragment: a public_url is logged
		// operationally, so it must not carry secrets, and Google's callback is a
		// plain path — query/fragment/userinfo have no legitimate use here.
		if u.User != nil {
			return fmt.Errorf("webhook.public_url must not contain user info (credentials)")
		}
		if u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("webhook.public_url must not contain a query string or fragment")
		}
		if c.Webhook.VerificationToken == "" {
			return fmt.Errorf("webhook.verification_token is required when webhook.enabled is true (used to reject forged notifications)")
		}
		// Validate explicitly-set durations here (before defaults are applied),
		// so an invalid value fails at config load rather than mid-startup.
		if c.Webhook.DebounceInterval != "" {
			if _, err := time.ParseDuration(c.Webhook.DebounceInterval); err != nil {
				return fmt.Errorf("webhook.debounce_interval %q is not a valid duration: %w", c.Webhook.DebounceInterval, err)
			}
		}
		if c.Webhook.ChannelTTL != "" {
			d, err := time.ParseDuration(c.Webhook.ChannelTTL)
			if err != nil {
				return fmt.Errorf("webhook.channel_ttl %q is not a valid duration: %w", c.Webhook.ChannelTTL, err)
			}
			// time.ParseDuration accepts "0s" and negatives; googleWatcher
			// treats a non-positive TTL as "omit Channel.Expiration", silently
			// falling back to Google's own default instead of the operator's
			// explicit value. Leaving channel_ttl unset (which uses the "24h"
			// default) is the way to get provider-default behavior.
			if d <= 0 {
				return fmt.Errorf("webhook.channel_ttl must be positive, got %q", c.Webhook.ChannelTTL)
			}
		}
	}
	return nil
}
