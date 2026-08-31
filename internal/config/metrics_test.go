package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeConfig writes a config file with two valid accounts plus whatever extra
// YAML the test needs, and returns its path.
func writeConfig(t *testing.T, extra string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	body := `accounts:
  - name: personal
    credentials_file: /secrets/personal-credentials.json
    token_file: /secrets/personal-token.json
    calendar_id: primary
  - name: work-acme
    credentials_file: /secrets/work-acme-credentials.json
    token_file: /secrets/work-acme-token.json
    calendar_id: primary
` + extra
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("writing config: %v", err)
	}
	return path
}

func TestLoad_MetricsDisabledByDefault(t *testing.T) {
	cfg, err := Load(writeConfig(t, ""))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Metrics.Enabled {
		t.Error("metrics must be off unless explicitly enabled")
	}
	// No defaults are applied to a disabled block: nothing is served, so there
	// is nothing to configure.
	if cfg.Metrics.ListenAddr != "" || cfg.Metrics.ReadyMaxAge != "" {
		t.Errorf("a disabled metrics block got defaults applied: %+v", cfg.Metrics)
	}
}

func TestLoad_MetricsDefaults(t *testing.T) {
	cases := []struct {
		name           string
		extra          string
		wantAddr       string
		wantReadyMaxAg string
	}{
		{
			name:           "all defaults, default poll interval",
			extra:          "metrics:\n  enabled: true\n",
			wantAddr:       "127.0.0.1:9090",
			wantReadyMaxAg: "15m0s", // 3 x the 5m default
		},
		{
			name:           "ready_max_age tracks a custom poll_interval",
			extra:          "poll_interval: 2m\nmetrics:\n  enabled: true\n",
			wantAddr:       "127.0.0.1:9090",
			wantReadyMaxAg: "6m0s",
		},
		{
			name:           "explicit values are not overridden",
			extra:          "metrics:\n  enabled: true\n  listen_addr: \"0.0.0.0:9999\"\n  ready_max_age: \"90s\"\n",
			wantAddr:       "0.0.0.0:9999",
			wantReadyMaxAg: "90s",
		},
		{
			name:           "zero explicitly disables the staleness check",
			extra:          "metrics:\n  enabled: true\n  ready_max_age: \"0\"\n",
			wantAddr:       "127.0.0.1:9090",
			wantReadyMaxAg: "0",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := Load(writeConfig(t, tc.extra))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.Metrics.ListenAddr != tc.wantAddr {
				t.Errorf("listen_addr = %q, want %q", cfg.Metrics.ListenAddr, tc.wantAddr)
			}
			if cfg.Metrics.ReadyMaxAge != tc.wantReadyMaxAg {
				t.Errorf("ready_max_age = %q, want %q", cfg.Metrics.ReadyMaxAge, tc.wantReadyMaxAg)
			}
		})
	}
}

func TestLoad_MetricsReadyMaxAgeValidation(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		wantErr string
	}{
		{"not a duration", "\"soon\"", "not a valid duration"},
		{"bare number", "\"15\"", "not a valid duration"},
		// Negative would make /readyz fail forever; "0" is the documented way
		// to disable the check.
		{"negative", "\"-5m\"", "must not be negative"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "metrics:\n  enabled: true\n  ready_max_age: "+tc.value+"\n"))
			if err == nil {
				t.Fatalf("Load accepted ready_max_age %s", tc.value)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// A disabled metrics block must not be validated at all — an operator who left
// a half-finished block behind should not be blocked from starting.
func TestLoad_MetricsDisabledSkipsValidation(t *testing.T) {
	if _, err := Load(writeConfig(t, "metrics:\n  enabled: false\n  ready_max_age: \"nonsense\"\n")); err != nil {
		t.Errorf("Load rejected an invalid but DISABLED metrics block: %v", err)
	}
}

func TestThreePollIntervals(t *testing.T) {
	cases := map[string]string{
		"5m":  "15m0s",
		"2m":  "6m0s",
		"30s": "1m30s",
		"1h":  "3h0m0s",
		// Unparseable and non-positive fall back to three times the 5m default.
		// Validate rejects these before applyDefaults runs; the fallback covers
		// the empty case at the call site.
		"":        "15m",
		"garbage": "15m",
		"0s":      "15m",
		"-1m":     "15m",
	}
	for in, want := range cases {
		if got := threePollIntervals(in); got != want {
			t.Errorf("threePollIntervals(%q) = %q, want %q", in, got, want)
		}
	}
}

// The metrics block must survive a save/load round trip, since the web UI
// rewrites the whole file.
func TestSave_PreservesMetricsBlock(t *testing.T) {
	path := writeConfig(t, "metrics:\n  enabled: true\n  listen_addr: \"127.0.0.1:9191\"\n  ready_max_age: \"20m\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Metrics != cfg.Metrics {
		t.Errorf("metrics block changed across a save/load round trip: %+v -> %+v", cfg.Metrics, reloaded.Metrics)
	}
}
