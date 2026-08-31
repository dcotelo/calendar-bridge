package webui

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/dcotelo/calendar-bridge/internal/config"
)

// writeJSON writes v as JSON with the given status.
//
// Every API response carries no-store: the config and status payloads describe
// a live local admin surface and must never sit in a disk cache or be replayed
// from one after the operator has moved on.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Error("webui: encoding response", "error", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// plainError is http.Error plus the two headers every response from this
// server carries. http.Error sets nosniff itself but not Cache-Control, and a
// rejection is exactly the response least worth serving from a cache: an
// intermediary that stored a 401 or a 403 would keep answering with it after
// the operator fixed the token or the origin.
//
// Used for the rejections that fire before a handler runs (auth, Host,
// cross-site) and for non-JSON handler errors; JSON responses go through
// writeJSON, which sets the same headers.
func plainError(w http.ResponseWriter, msg string, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.Error(w, msg, status)
}

// handleGetConfig returns the current config as JSON.
//
// Note on safety: the Config struct contains only account metadata and file
// PATHS (credentials_file, token_file) — never the credential contents. So
// returning it verbatim does not expose OAuth secrets. The sensitive fields
// are web_ui.auth_token and webhook.verification_token; both are redacted
// before sending.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		// The response stays generic: the browser has no use for the server's
		// filesystem layout, and the operator already knows the path they
		// passed. The error still goes to the log, where the operator needs
		// it to tell "missing" from "malformed" — config.Load is path-free
		// (it strips the *fs.PathError cause), so logging it discloses
		// nothing the response deliberately withholds.
		s.logger.Warn("webui: could not load config for GET /api/config", "error", err)
		s.writeError(w, http.StatusInternalServerError,
			"could not load the configuration file (check the -config path and its contents)")
		return
	}
	// Redact both secrets: the browser doesn't need them back, and echoing
	// them would widen their exposure. An empty value on PUT means "leave
	// unchanged" for auth_token; the UI never sends a webhook block at all,
	// so handlePutConfig always carries the existing one forward.
	cfg.WebUI.AuthToken = ""
	cfg.Webhook.VerificationToken = ""
	s.writeJSON(w, http.StatusOK, cfg)
}

// handlePutConfig validates and atomically saves a new config.
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	var incoming config.Config
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)) // 1MiB cap
	dec.DisallowUnknownFields()
	if err := dec.Decode(&incoming); err != nil {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	// Decoder.Decode stops after the first value and silently ignores trailing
	// data; require the body to contain exactly one JSON value. dec.More()
	// alone isn't enough — it reports false for a stray "]" following a
	// complete top-level object, since there's no open array to close. Decode
	// once more and require io.EOF, which catches that case along with
	// "{...}{...}" and "{...}junk".
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: unexpected trailing data after the config object")
		return
	}

	// The browser form only edits account/sync fields, not the web_ui server
	// settings or the webhook block at all. Carry the existing web_ui fields
	// forward when the client omits them (zero value = "unchanged"), so
	// saving from the UI can never disable the UI or wipe its listen address
	// / auth token and lock the operator out; carry the whole webhook block
	// forward whenever the client sends none of it, so a UI save can never
	// silently disable push notifications or blank the verification token.
	// If the existing config can't be loaded, refuse rather than persist a
	// payload missing those fields (which would set enabled:false / empty addr).
	existing, err := config.Load(s.configPath)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "cannot load current config to merge server settings")
		return
	}
	if incoming.WebUI.AuthToken == "" {
		incoming.WebUI.AuthToken = existing.WebUI.AuthToken
	}
	if !incoming.WebUI.Enabled {
		incoming.WebUI.Enabled = existing.WebUI.Enabled
	}
	if incoming.WebUI.ListenAddr == "" {
		incoming.WebUI.ListenAddr = existing.WebUI.ListenAddr
	}
	if incoming.Webhook == (config.Webhook{}) {
		incoming.Webhook = existing.Webhook
	}

	// Save validates first and writes atomically at 0600; an invalid config is
	// rejected without clobbering the existing file.
	if err := incoming.Save(s.configPath); err != nil {
		s.writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	s.logger.Info("webui: config updated", "remote", r.RemoteAddr)
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

// handleGetStatus returns the runtime status snapshot.
func (s *Server) handleGetStatus(w http.ResponseWriter, r *http.Request) {
	if s.status == nil {
		s.writeJSON(w, http.StatusOK, Status{})
		return
	}
	s.writeJSON(w, http.StatusOK, s.status())
}

// handleSync triggers a one-off sync pass. Concurrent invocations are rejected
// (409) rather than allowed to race on the same calendars.
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) {
	if s.syncFn == nil {
		s.writeError(w, http.StatusServiceUnavailable, "sync not available in this mode")
		return
	}
	if !s.syncing.TryLock() {
		s.writeError(w, http.StatusConflict, "a sync is already running")
		return
	}
	defer s.syncing.Unlock()
	if err := s.syncFn(); err != nil {
		s.writeError(w, http.StatusInternalServerError, "sync failed: "+err.Error())
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]string{"status": "sync complete"})
}

// handleIndex serves the embedded single-page UI.
func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		plainError(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	nonce, err := newNonce()
	if err != nil {
		s.logger.Error("webui: generating CSP nonce", "error", err)
		plainError(w, "internal error", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Strict CSP. The page is fully self-contained — no external assets, no
	// CDN — and the inline style and script blocks are authorised by a
	// per-response nonce rather than 'unsafe-inline', so injected markup
	// cannot execute even if something ever managed to reach the DOM.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; "+
			"style-src 'nonce-"+nonce+"'; "+
			"script-src 'nonce-"+nonce+"'; "+
			"connect-src 'self'; "+
			"img-src 'self' data:; "+
			"base-uri 'none'; "+
			"frame-ancestors 'none'; "+
			"form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Legacy equivalent of frame-ancestors, for agents predating CSP level 2.
	w.Header().Set("X-Frame-Options", "DENY")
	// The page URL can carry no secrets, but there is no reason to hand any
	// referrer to anything the operator navigates to from here.
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")

	if _, err := w.Write(pageWithNonce(nonce)); err != nil {
		s.logger.Error("webui: writing index", "error", err)
	}
}

// noncePlaceholder is the token the embedded page carries wherever a CSP nonce
// must be substituted in.
const noncePlaceholder = "__CSP_NONCE__"

// newNonce returns a fresh base64 CSP nonce.
func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// pageWithNonce returns the embedded page with its nonce placeholders filled
// in. The page is a few tens of kilobytes and this is a loopback admin UI, so
// substituting per request costs nothing worth optimising away.
func pageWithNonce(nonce string) []byte {
	return bytes.ReplaceAll(indexHTML, []byte(noncePlaceholder), []byte(nonce))
}
