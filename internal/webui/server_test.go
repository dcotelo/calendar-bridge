package webui

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dcotelo/calendar-bridge/internal/config"
)

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

const validYAML = `
accounts:
  - name: personal
    credentials_file: a.json
    token_file: a-tok.json
    calendar_id: primary
  - name: work
    credentials_file: b.json
    token_file: b-tok.json
    calendar_id: primary
poll_interval: 5m
lookahead_days: 30
block_title: Busy
`

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func newTestServer(t *testing.T, token string, opts ...func(*Options)) *Server {
	t.Helper()
	o := Options{
		ConfigPath: writeConfig(t, validYAML),
		AuthToken:  token,
		ListenAddr: "127.0.0.1:0",
		Logger:     testLogger(),
	}
	for _, f := range opts {
		f(&o)
	}
	srv, err := New(o)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return srv
}

func req(t *testing.T, method, target, token, body string) *http.Request {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequestWithContext(context.Background(), method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequestWithContext(context.Background(), method, target, nil)
	}
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	// Default to a loopback Host so requests pass the DNS-rebinding guard;
	// tests that exercise the guard set r.Host explicitly.
	r.Host = "127.0.0.1:8090"
	return r
}

func TestNew_RefusesNonLoopbackWithoutToken(t *testing.T) {
	_, err := New(Options{
		ConfigPath: writeConfig(t, validYAML),
		ListenAddr: "0.0.0.0:8090",
		Logger:     testLogger(),
	})
	if err == nil {
		t.Fatal("New() error = nil, want refusal to bind non-loopback")
	}
}

func TestNew_RefusesNonLoopbackEvenWithToken(t *testing.T) {
	// The server is plaintext HTTP; a token does not make a non-loopback bind
	// safe (the token would travel in the clear), so it must still be refused.
	_, err := New(Options{
		ConfigPath: writeConfig(t, validYAML),
		ListenAddr: "0.0.0.0:8090",
		AuthToken:  "secret",
		Logger:     testLogger(),
	})
	if err == nil {
		t.Error("New() error = nil, want refusal to bind non-loopback even with a token (plaintext exposure)")
	}
}

func TestNew_AllowsLoopback(t *testing.T) {
	if _, err := New(Options{
		ConfigPath: writeConfig(t, validYAML),
		ListenAddr: "127.0.0.1:8090",
		Logger:     testLogger(),
	}); err != nil {
		t.Errorf("New() error = %v, want nil for loopback", err)
	}
}

func TestAuth_RejectsMissingToken(t *testing.T) {
	srv := newTestServer(t, "s3cret")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "", ""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for missing token", w.Code)
	}
}

func TestAuth_RejectsWrongToken(t *testing.T) {
	srv := newTestServer(t, "s3cret")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "wrong", ""))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for wrong token", w.Code)
	}
}

func TestAuth_AcceptsValidToken(t *testing.T) {
	srv := newTestServer(t, "s3cret")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "s3cret", ""))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for valid token", w.Code)
	}
}

func TestAuth_NoTokenConfiguredPassesThrough(t *testing.T) {
	srv := newTestServer(t, "") // loopback, no token
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "", ""))
	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no token configured => pass-through)", w.Code)
	}
}

func TestGetConfig_RedactsAuthToken(t *testing.T) {
	cfgWithToken := validYAML + "\nweb_ui:\n  enabled: true\n  auth_token: super-secret\n"
	srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = writeConfig(t, cfgWithToken) })

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "super-secret") {
		t.Error("GET /api/config leaked the auth token; want it redacted")
	}
}

func TestPutConfig_RoundTripPersists(t *testing.T) {
	path := writeConfig(t, validYAML)
	srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = path })

	body := `{"accounts":[
	  {"name":"personal","credentials_file":"a.json","token_file":"a-tok.json","calendar_id":"primary"},
	  {"name":"work","credentials_file":"b.json","token_file":"b-tok.json","calendar_id":"work-cal"}
	],"poll_interval":"10m","lookahead_days":60,"block_title":"DND","web_ui":{"auth_token":""}}`

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	// Confirm it persisted and revalidates on load.
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload after PUT: %v", err)
	}
	if reloaded.PollInterval != "10m" || reloaded.LookaheadDays != 60 || reloaded.BlockTitle != "DND" {
		t.Errorf("persisted config = %+v, want poll=10m lookahead=60 title=DND", reloaded)
	}
	if reloaded.Accounts[1].CalendarID != "work-cal" {
		t.Errorf("persisted work calendar_id = %q, want work-cal", reloaded.Accounts[1].CalendarID)
	}
}

func TestPutConfig_RejectsInvalid(t *testing.T) {
	path := writeConfig(t, validYAML)
	srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = path })

	// One account -> fails Validate.
	body := `{"accounts":[{"name":"solo","credentials_file":"a.json","token_file":"t.json","calendar_id":"primary"}],"web_ui":{"auth_token":""}}`
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "", body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("PUT invalid status = %d, want 400", w.Code)
	}

	// Original file must be intact (2 accounts).
	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(reloaded.Accounts) != 2 {
		t.Errorf("accounts after rejected PUT = %d, want 2 (unchanged)", len(reloaded.Accounts))
	}
}

func TestPutConfig_PreservesExistingAuthToken(t *testing.T) {
	cfgWithToken := validYAML + "\nweb_ui:\n  enabled: true\n  auth_token: keep-me\n"
	path := writeConfig(t, cfgWithToken)
	srv := newTestServer(t, "keep-me", func(o *Options) {
		o.ConfigPath = path
		o.AuthToken = "keep-me"
	})

	// PUT with empty auth_token -> server should preserve the existing one.
	body := `{"accounts":[
	  {"name":"personal","credentials_file":"a.json","token_file":"a-tok.json","calendar_id":"primary"},
	  {"name":"work","credentials_file":"b.json","token_file":"b-tok.json","calendar_id":"primary"}
	],"poll_interval":"5m","lookahead_days":30,"block_title":"Busy","web_ui":{"auth_token":""}}`
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "keep-me", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.WebUI.AuthToken != "keep-me" {
		t.Errorf("auth token after empty-token PUT = %q, want keep-me (preserved)", reloaded.WebUI.AuthToken)
	}
}

func TestPutConfig_PreservesWebUIServerFields(t *testing.T) {
	// Config with the UI enabled on a custom addr and a token.
	cfgYAML := validYAML + "\nweb_ui:\n  enabled: true\n  listen_addr: 127.0.0.1:9999\n  auth_token: keep-me\n"
	path := writeConfig(t, cfgYAML)
	srv := newTestServer(t, "keep-me", func(o *Options) {
		o.ConfigPath = path
		o.AuthToken = "keep-me"
	})

	// Exactly what the browser's collectConfig sends: only web_ui.auth_token,
	// omitting enabled and listen_addr.
	body := `{"accounts":[
	  {"name":"personal","credentials_file":"a.json","token_file":"a-tok.json","calendar_id":"primary"},
	  {"name":"work","credentials_file":"b.json","token_file":"b-tok.json","calendar_id":"primary"}
	],"poll_interval":"5m","lookahead_days":30,"block_title":"Busy","web_ui":{"auth_token":""}}`
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "keep-me", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.WebUI.Enabled {
		t.Error("web_ui.enabled was wiped by a UI save; want it preserved (true)")
	}
	if reloaded.WebUI.ListenAddr != "127.0.0.1:9999" {
		t.Errorf("web_ui.listen_addr = %q, want 127.0.0.1:9999 (preserved)", reloaded.WebUI.ListenAddr)
	}
	if reloaded.WebUI.AuthToken != "keep-me" {
		t.Errorf("web_ui.auth_token = %q, want keep-me (preserved)", reloaded.WebUI.AuthToken)
	}
}

func TestPutConfig_PreservesWebhookBlock(t *testing.T) {
	// Config with push notifications configured. The UI form has no fields
	// for the webhook block at all, so a save from the browser must never
	// wipe it out.
	cfgYAML := validYAML + "\nwebhook:\n  enabled: true\n  public_url: https://cb.example.com\n" +
		"  listen_addr: 127.0.0.1:8080\n  verification_token: secret-token\n" +
		"  channel_ttl: 24h\n  debounce_interval: 5s\n"
	path := writeConfig(t, cfgYAML)
	srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = path })

	// Exactly what the browser's collectConfig sends: no "webhook" key.
	body := `{"accounts":[
	  {"name":"personal","credentials_file":"a.json","token_file":"a-tok.json","calendar_id":"primary"},
	  {"name":"work","credentials_file":"b.json","token_file":"b-tok.json","calendar_id":"primary"}
	],"poll_interval":"5m","lookahead_days":30,"block_title":"Busy","web_ui":{"auth_token":""}}`
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "", body))
	if w.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, want 200; body=%s", w.Code, w.Body.String())
	}

	reloaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.Webhook.Enabled {
		t.Error("webhook.enabled was wiped by a UI save; want it preserved (true)")
	}
	if reloaded.Webhook.VerificationToken != "secret-token" {
		t.Errorf("webhook.verification_token = %q, want secret-token (preserved)", reloaded.Webhook.VerificationToken)
	}
	if reloaded.Webhook.PublicURL != "https://cb.example.com" {
		t.Errorf("webhook.public_url = %q, want preserved", reloaded.Webhook.PublicURL)
	}
}

func TestGetConfig_RedactsWebhookVerificationToken(t *testing.T) {
	cfgYAML := validYAML + "\nwebhook:\n  enabled: true\n  public_url: https://cb.example.com\n" +
		"  listen_addr: 127.0.0.1:8080\n  verification_token: secret-token\n"
	path := writeConfig(t, cfgYAML)
	srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = path })

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want 200", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret-token") {
		t.Error("GET /api/config leaked webhook.verification_token; want it redacted")
	}
}

func TestSync_RejectsConcurrent(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	srv := newTestServer(t, "", func(o *Options) {
		o.Sync = func() error {
			close(entered)
			<-release // hold the sync open
			return nil
		}
	})

	// First sync in a goroutine, held open.
	go func() {
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req(t, http.MethodPost, "/api/sync", "", ""))
	}()
	<-entered // ensure the first sync holds the lock

	// Second concurrent sync must be rejected with 409.
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPost, "/api/sync", "", ""))
	if w.Code != http.StatusConflict {
		t.Errorf("concurrent sync status = %d, want 409", w.Code)
	}
	close(release)
}

func TestSync_CallbackErrorReturns500(t *testing.T) {
	srv := newTestServer(t, "", func(o *Options) {
		o.Sync = func() error { return errors.New("calendar api unavailable") }
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPost, "/api/sync", "", ""))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("failed sync status = %d, want 500", w.Code)
	}
}

func TestIndex_ServedWithoutAuthEvenWhenTokenSet(t *testing.T) {
	// The critical fix: a browser can't send Authorization on a top-level
	// navigation, so GET / must return the page even when a token is configured.
	srv := newTestServer(t, "s3cret")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/", "", "")) // no token header
	if w.Code != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200 (page must load so it can collect the token)", w.Code)
	}
	if !strings.Contains(w.Body.String(), "<html") {
		t.Error("GET / did not return HTML")
	}
}

func TestAPI_StillGatedWhenTokenSet(t *testing.T) {
	srv := newTestServer(t, "s3cret")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "", "")) // no token
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/config status = %d, want 401 (API stays gated)", w.Code)
	}
}

func TestAuth_BearerSchemeCaseInsensitive(t *testing.T) {
	srv := newTestServer(t, "s3cret")
	w := httptest.NewRecorder()
	r := req(t, http.MethodGet, "/api/config", "", "")
	r.Header.Set("Authorization", "bearer s3cret") // lowercase scheme
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Errorf("lowercase bearer scheme status = %d, want 200 (RFC 7235 case-insensitive)", w.Code)
	}
}

func TestPutConfig_FailsWhenExistingConfigUnloadable(t *testing.T) {
	// Point at a non-existent config path: the merge-load must fail closed
	// (500) rather than save a payload missing the web_ui server fields.
	srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = "/nonexistent/dir/config.yaml" })
	body := `{"accounts":[
	  {"name":"a","credentials_file":"a.json","token_file":"at.json","calendar_id":"primary"},
	  {"name":"b","credentials_file":"b.json","token_file":"bt.json","calendar_id":"primary"}
	],"poll_interval":"5m","block_title":"Busy","web_ui":{"auth_token":""}}`
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "", body))
	if w.Code != http.StatusInternalServerError {
		t.Errorf("PUT with unloadable existing config = %d, want 500", w.Code)
	}
}

func TestPutConfig_RejectsTrailingData(t *testing.T) {
	// Decoder.Decode alone would silently ignore trailing data after the
	// first JSON value, and dec.More() alone misses a stray "]" following a
	// complete top-level object (there's no open array for it to close).
	cases := []struct {
		name string
		body string
	}{
		{"second JSON value", `{"accounts":[],"poll_interval":"5m"}{"extra":true}`},
		{"stray closing bracket", `{"accounts":[],"poll_interval":"5m"}]`},
		{"trailing garbage", `{"accounts":[],"poll_interval":"5m"}junk`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeConfig(t, validYAML)
			srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = path })
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "", tc.body))
			if w.Code != http.StatusBadRequest {
				t.Errorf("trailing-data status = %d, want 400", w.Code)
			}
			if cfg, err := config.Load(path); err != nil || len(cfg.Accounts) != 2 {
				t.Error("config changed after trailing-data PUT; want unchanged")
			}
		})
	}
}

func TestPutConfig_RejectsMalformedJSON(t *testing.T) {
	path := writeConfig(t, validYAML)
	srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = path })
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "", "{not json"))
	if w.Code != http.StatusBadRequest {
		t.Errorf("malformed JSON status = %d, want 400", w.Code)
	}
	if cfg, err := config.Load(path); err != nil || len(cfg.Accounts) != 2 {
		t.Error("config changed after malformed PUT; want unchanged")
	}
}

func TestPutConfig_RejectsUnknownField(t *testing.T) {
	path := writeConfig(t, validYAML)
	srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = path })
	body := `{"accounts":[],"bogus_field":true}`
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPut, "/api/config", "", body))
	if w.Code != http.StatusBadRequest {
		t.Errorf("unknown-field status = %d, want 400", w.Code)
	}
	if cfg, err := config.Load(path); err != nil || len(cfg.Accounts) != 2 {
		t.Error("config changed after unknown-field PUT; want unchanged")
	}
}

func TestHostIsLoopbackAuthority(t *testing.T) {
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1:8090", true},
		{"127.0.0.1", true},
		{"localhost:8090", true},
		{"localhost", true},
		{"[::1]:8090", true},
		{"[::1]", true},
		{"192.168.1.5:8090", false},
		{"attacker.example:8090", false},
		{"", false},
		{"[::1", false}, // unbalanced
		{"::1]", false}, // unbalanced
		{"[::1]extra", false},
	}
	for _, tc := range cases {
		t.Run(tc.host, func(t *testing.T) {
			if got := hostIsLoopbackAuthority(tc.host); got != tc.want {
				t.Errorf("hostIsLoopbackAuthority(%q) = %v, want %v", tc.host, got, tc.want)
			}
		})
	}
}

func TestCSRF_Rejects(t *testing.T) {
	cases := []struct {
		name         string
		host         string
		origin       string
		secFetchSite string
	}{
		{
			// No-token loopback mode: a request with a public Host (as in a
			// DNS rebinding attack) must be rejected even though Origin
			// matches Host.
			name:         "rebound host",
			host:         "attacker.example:8090",
			origin:       "http://attacker.example:8090",
			secFetchSite: "same-origin",
		},
		{
			name:         "cross-site Sec-Fetch-Site",
			host:         "127.0.0.1:8090",
			origin:       "http://127.0.0.1:8090",
			secFetchSite: "cross-site",
		},
		{
			name:   "origin authority differs from host",
			host:   "127.0.0.1:8090",
			origin: "http://evil.example",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newTestServer(t, "")
			r := req(t, http.MethodGet, "/api/config", "", "")
			r.Host = tc.host
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.secFetchSite != "" {
				r.Header.Set("Sec-Fetch-Site", tc.secFetchSite)
			}
			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Errorf("status = %d, want 403", w.Code)
			}
		})
	}
}

func TestSync_CallsCallback(t *testing.T) {
	called := false
	srv := newTestServer(t, "", func(o *Options) {
		o.Sync = func() error { called = true; return nil }
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPost, "/api/sync", "", ""))
	if w.Code != http.StatusOK {
		t.Errorf("sync status = %d, want 200", w.Code)
	}
	if !called {
		t.Error("sync callback not invoked")
	}
}

func TestSync_NilCallbackReturns503(t *testing.T) {
	srv := newTestServer(t, "") // no Sync set
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodPost, "/api/sync", "", ""))
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("sync status = %d, want 503 when no callback", w.Code)
	}
}

func TestStatus_ReturnsSnapshot(t *testing.T) {
	srv := newTestServer(t, "", func(o *Options) {
		o.Status = func() Status { return Status{AccountsNum: 3, LastSync: "2026-01-01T00:00:00Z"} }
	})
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/status", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("status code = %d, want 200", w.Code)
	}
	var got Status
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if got.AccountsNum != 3 {
		t.Errorf("status accounts = %d, want 3", got.AccountsNum)
	}
}

func TestIndex_ServedWithSecurityHeaders(t *testing.T) {
	srv := newTestServer(t, "")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/", "", ""))
	if w.Code != http.StatusOK {
		t.Fatalf("index status = %d, want 200", w.Code)
	}

	csp := w.Header().Get("Content-Security-Policy")
	// Asserted directive by directive rather than as one exact string, so
	// adding a directive doesn't fail the test but weakening one does.
	for _, want := range []string{
		"default-src 'none'",
		"connect-src 'self'",
		"base-uri 'none'",
		"frame-ancestors 'none'",
		"form-action 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP is missing %q; got %q", want, csp)
		}
	}
	// 'unsafe-inline' would defeat the point of the nonce.
	if strings.Contains(csp, "unsafe-inline") || strings.Contains(csp, "unsafe-eval") {
		t.Errorf("CSP contains an unsafe- directive: %q", csp)
	}

	for _, tc := range []struct{ header, want string }{
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Referrer-Policy", "no-referrer"},
		{"Cache-Control", "no-store"},
	} {
		if got := w.Header().Get(tc.header); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.header, got, tc.want)
		}
	}
}

// The nonce in the header must match the one in the served body, and must
// differ on every response — a fixed nonce is no better than 'unsafe-inline'.
func TestIndex_CSPNonceMatchesTheBodyAndRotates(t *testing.T) {
	srv := newTestServer(t, "")

	fetch := func() (nonce, body string) {
		t.Helper()
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/", "", ""))
		csp := w.Header().Get("Content-Security-Policy")
		const marker = "script-src 'nonce-"
		i := strings.Index(csp, marker)
		if i < 0 {
			t.Fatalf("CSP has no script-src nonce: %q", csp)
		}
		rest := csp[i+len(marker):]
		j := strings.Index(rest, "'")
		if j < 0 {
			t.Fatalf("unterminated nonce in CSP: %q", csp)
		}
		return rest[:j], w.Body.String()
	}

	n1, body1 := fetch()
	if n1 == "" {
		t.Fatal("empty CSP nonce")
	}
	if strings.Contains(body1, "__CSP_NONCE__") {
		t.Error("the served page still contains the nonce placeholder; its style and script would be blocked")
	}
	if got := strings.Count(body1, `nonce="`+n1+`"`); got != 2 {
		t.Errorf("body carries the header's nonce %d times, want 2 (the inline style and the inline script)", got)
	}

	n2, _ := fetch()
	if n1 == n2 {
		t.Error("the CSP nonce is identical across responses; it must be per-response to be worth anything")
	}
}

// The config path must not reach the browser, matching how the CLI keeps it out
// of logs.
func TestGetConfig_ErrorDoesNotLeakTheConfigPath(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "definitely-not-here", "config.yaml")
	srv, err := New(Options{ConfigPath: missing, ListenAddr: "127.0.0.1:0", Logger: testLogger()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "", ""))
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), dir) {
		t.Errorf("the response leaks the config path: %s", w.Body.String())
	}
}

// validConfigJSON is a well-formed PUT body: the request must be rejected for
// the reason under test (cross-site, bad Origin), not for its contents.
const validConfigJSON = `{"accounts":[
  {"name":"personal","credentials_file":"a.json","token_file":"a-tok.json","calendar_id":"primary"},
  {"name":"work","credentials_file":"b.json","token_file":"b-tok.json","calendar_id":"work-cal"}
],"poll_interval":"10m","lookahead_days":60,"block_title":"DND","web_ui":{"auth_token":""}}`

// Every response this server produces — success or rejection — must carry
// no-store and nosniff. Error paths matter MORE than success paths here: an
// intermediary that cached a 401 or a 403 would keep answering with it after
// the operator fixed the token or the origin, and the rejections fire in
// middleware, before any handler that would otherwise set the headers.
//
// http.Error sets nosniff on its own but never Cache-Control, which is why the
// rejections go through plainError instead.
func TestAPIResponses_AlwaysCarrySafetyHeaders(t *testing.T) {
	cases := []struct {
		name       string
		server     func(t *testing.T) *Server
		method     string
		path       string
		token      string
		body       string
		mutate     func(r *http.Request)
		wantStatus int
	}{
		{
			name:       "GET /api/config succeeds",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodGet,
			path:       "/api/config",
			wantStatus: http.StatusOK,
		},
		{
			name:       "GET /api/status succeeds",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodGet,
			path:       "/api/status",
			wantStatus: http.StatusOK,
		},
		{
			name: "a config that cannot be loaded",
			server: func(t *testing.T) *Server {
				missing := filepath.Join(t.TempDir(), "absent", "config.yaml")
				return newTestServer(t, "", func(o *Options) { o.ConfigPath = missing })
			},
			method:     http.MethodGet,
			path:       "/api/config",
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "a request with no token when one is required",
			server:     func(t *testing.T) *Server { return newTestServer(t, "s3cret") },
			method:     http.MethodGet,
			path:       "/api/config",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a request with the wrong token",
			server:     func(t *testing.T) *Server { return newTestServer(t, "s3cret") },
			method:     http.MethodGet,
			path:       "/api/config",
			token:      "wrong",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "a state-changing request rejected as cross-site",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodPut,
			path:       "/api/config",
			body:       validConfigJSON,
			mutate:     func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a state-changing request rejected by Origin",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodPut,
			path:       "/api/config",
			body:       validConfigJSON,
			mutate:     func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "a request with a rebound Host",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodGet,
			path:       "/api/config",
			mutate:     func(r *http.Request) { r.Host = "attacker.example" },
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "malformed JSON on PUT",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodPut,
			path:       "/api/config",
			body:       "{not json",
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "the index rejects a non-GET method",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodPost,
			path:       "/",
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			// http.NotFound writes its own response and never reaches the
			// per-handler helpers, which is why the headers are set by an
			// outermost wrapper rather than at each call site.
			name:       "an unknown path 404s",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodGet,
			path:       "/nope",
			wantStatus: http.StatusNotFound,
		},
		{
			name:       "an unknown API path 404s",
			server:     func(t *testing.T) *Server { return newTestServer(t, "") },
			method:     http.MethodGet,
			path:       "/api/nope",
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.server(t)
			r := req(t, tc.method, tc.path, tc.token, tc.body)
			if tc.mutate != nil {
				tc.mutate(r)
			}

			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, r)

			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := w.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

// An error response must never describe the server's filesystem. The UI is a
// local admin surface, but the browser is still the wrong place to learn where
// on disk the operator keeps their config and credentials — and the response
// may be pasted into an issue or a screenshot.
//
// This asserts the HTTP body directly rather than trusting config.Load to stay
// path-free, so a regression in either layer is caught here.
func TestGetConfig_ErrorResponseNeverDisclosesTheConfigPath(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"missing config file": func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), "sEcReTdIrNaMe")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("seed: %v", err)
			}
			return filepath.Join(dir, "config.yaml")
		},
		"unparseable config file": func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), "sEcReTdIrNaMe")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("seed: %v", err)
			}
			p := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(p, []byte("accounts: [oops\n"), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			return p
		},
		"config file that fails validation": func(t *testing.T) string {
			dir := filepath.Join(t.TempDir(), "sEcReTdIrNaMe")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatalf("seed: %v", err)
			}
			p := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(p, []byte("accounts: []\n"), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			return p
		},
	}

	for name, seed := range cases {
		t.Run(name, func(t *testing.T) {
			path := seed(t)
			srv := newTestServer(t, "", func(o *Options) { o.ConfigPath = path })

			w := httptest.NewRecorder()
			srv.Handler().ServeHTTP(w, req(t, http.MethodGet, "/api/config", "", ""))

			if w.Code != http.StatusInternalServerError {
				t.Fatalf("status = %d, want %d", w.Code, http.StatusInternalServerError)
			}
			body := w.Body.String()
			for _, leak := range []string{path, filepath.Dir(path), "sEcReTdIrNaMe"} {
				if strings.Contains(body, leak) {
					t.Errorf("response discloses %q:\n%s", leak, body)
				}
			}
		})
	}
}
