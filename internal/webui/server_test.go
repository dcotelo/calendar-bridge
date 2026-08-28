package webui

import (
	"context"
	"encoding/json"
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
	return r
}

func TestNew_RefusesNonLoopbackWithoutToken(t *testing.T) {
	_, err := New(Options{
		ConfigPath: writeConfig(t, validYAML),
		ListenAddr: "0.0.0.0:8090",
		Logger:     testLogger(),
	})
	if err == nil {
		t.Fatal("New() error = nil, want refusal to bind non-loopback without a token")
	}
}

func TestNew_AllowsNonLoopbackWithToken(t *testing.T) {
	_, err := New(Options{
		ConfigPath: writeConfig(t, validYAML),
		ListenAddr: "0.0.0.0:8090",
		AuthToken:  "secret",
		Logger:     testLogger(),
	})
	if err != nil {
		t.Errorf("New() error = %v, want nil (token provided)", err)
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
	if w.Header().Get("Content-Security-Policy") == "" {
		t.Error("index missing Content-Security-Policy header")
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("index missing X-Content-Type-Options: nosniff")
	}
}
