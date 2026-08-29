package webui

import (
	"encoding/json"
	"net/http"

	"github.com/dcotelo/calendar-bridge/internal/config"
)

// writeJSON writes v as JSON with the given status.
func (s *Server) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		s.logger.Error("webui: encoding response", "error", err)
	}
}

func (s *Server) writeError(w http.ResponseWriter, status int, msg string) {
	s.writeJSON(w, status, map[string]string{"error": msg})
}

// handleGetConfig returns the current config as JSON.
//
// Note on safety: the Config struct contains only account metadata and file
// PATHS (credentials_file, token_file) — never the credential contents. So
// returning it verbatim does not expose OAuth secrets. The one sensitive field
// is web_ui.auth_token; it is redacted before sending.
func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg, err := config.Load(s.configPath)
	if err != nil {
		s.writeError(w, http.StatusInternalServerError, "loading config: "+err.Error())
		return
	}
	// Redact the auth token: the browser doesn't need it back, and echoing it
	// would widen its exposure. An empty value on PUT means "leave unchanged".
	if cfg.WebUI.AuthToken != "" {
		cfg.WebUI.AuthToken = ""
	}
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
	// data; require the body to contain exactly one JSON object so a
	// "{...}{...}" or "{...}junk" payload is rejected, not partially applied.
	if dec.More() {
		s.writeError(w, http.StatusBadRequest, "invalid JSON: unexpected trailing data after the config object")
		return
	}

	// The browser form only edits account/sync fields, not the web_ui server
	// settings. Carry the existing web_ui fields forward when the client omits
	// them (zero value = "unchanged"), so saving from the UI can never disable
	// the UI or wipe its listen address / auth token and lock the operator out.
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
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Conservative CSP: everything is inline and self-hosted; block external
	// loads entirely.
	w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-inline'; connect-src 'self'; frame-ancestors 'none'; form-action 'self'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if _, err := w.Write(indexHTML); err != nil {
		s.logger.Error("webui: writing index", "error", err)
	}
}
