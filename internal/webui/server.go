// Package webui serves a local, self-hosted configuration UI for
// calendar-bridge.
//
// # Security model
//
// The UI can read and rewrite config.yaml, so it is a privileged local admin
// surface and is treated as one:
//
//   - It binds loopback (127.0.0.1) by default. It REFUSES to start on a
//     non-loopback address unless an auth token is configured, so it can never
//     be silently exposed to a network without authentication.
//   - When an auth token is set, every request must carry it as a
//     "Authorization: Bearer <token>" header, compared in constant time.
//   - It never reads or serves the CONTENTS of credential or token files. The
//     UI edits config fields (including the file paths), exactly like editing
//     config.yaml by hand — the OAuth secrets themselves never enter the
//     browser. The interactive OAuth `auth` flow stays in the CLI.
//   - Config writes go through config.Validate and config.Save (atomic, 0600),
//     so the UI can never persist an invalid or world-readable config.
package webui

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"github.com/dcotelo/calendar-bridge/internal/config"
)

// SyncFunc triggers a one-off sync pass. It returns an error if the sync
// failed. The webui package takes this as a callback so it doesn't depend on
// the sync engine directly.
type SyncFunc func() error

// StatusFunc returns a snapshot of current sync status for display.
type StatusFunc func() Status

// Status is the read-only runtime snapshot shown in the UI.
type Status struct {
	Running     bool   `json:"running"`
	LastSync    string `json:"last_sync,omitempty"`    // RFC3339, empty if never
	LastError   string `json:"last_error,omitempty"`   // last sync error, if any
	AccountsNum int    `json:"accounts"`               // number of configured accounts
	PushEnabled bool   `json:"push_enabled,omitempty"` // reserved for webhook integration
}

// Server is the webui HTTP handler set. Construct with New and mount Handler()
// or call Serve.
type Server struct {
	configPath string
	authToken  string
	logger     *slog.Logger
	syncFn     SyncFunc
	status     StatusFunc

	// syncing guards against overlapping sync passes triggered via the UI:
	// concurrent POST /api/sync calls would otherwise race on the same
	// calendars. A non-blocking TryLock lets a second request fail fast (409)
	// rather than queue.
	syncing sync.Mutex
}

// Options configures a Server.
type Options struct {
	// ConfigPath is the path to config.yaml the UI reads and writes.
	ConfigPath string
	// AuthToken, if non-empty, is required as a Bearer token on every request.
	AuthToken string
	// ListenAddr is used only for the non-loopback safety check in New.
	ListenAddr string
	// Sync triggers a sync pass (POST /api/sync). May be nil (endpoint 503s).
	Sync SyncFunc
	// Status returns a runtime snapshot (GET /api/status). May be nil.
	Status StatusFunc
	// Logger for request/security logging. Defaults to slog.Default().
	Logger *slog.Logger
}

// New builds a Server. It returns an error if the configuration is unsafe:
// binding a non-loopback address without an auth token is refused, so the UI
// is never exposed to a network unauthenticated.
func New(opts Options) (*Server, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.ConfigPath == "" {
		return nil, fmt.Errorf("webui: ConfigPath is required")
	}
	if !config.IsLoopbackAddr(opts.ListenAddr) && opts.AuthToken == "" {
		return nil, fmt.Errorf("webui: refusing to bind non-loopback address %q without an auth token; "+
			"set web_ui.auth_token or bind a loopback address (127.0.0.1)", opts.ListenAddr)
	}
	return &Server{
		configPath: opts.ConfigPath,
		authToken:  opts.AuthToken,
		logger:     logger,
		syncFn:     opts.Sync,
		status:     opts.Status,
	}, nil
}

// Handler returns the http.Handler serving the UI and API, with auth applied.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/config", s.handleGetConfig)
	mux.HandleFunc("PUT /api/config", s.handlePutConfig)
	mux.HandleFunc("GET /api/status", s.handleGetStatus)
	mux.HandleFunc("POST /api/sync", s.handleSync)
	mux.HandleFunc("GET /", s.handleIndex)
	return s.authMiddleware(mux)
}

// authMiddleware enforces the Bearer token (when configured) in constant time.
// When no token is configured (loopback-only default), it is a pass-through.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authToken != "" {
			const prefix = "Bearer "
			got := r.Header.Get("Authorization")
			// Constant-time compare of the full "Bearer <token>" value avoids
			// leaking token length/content through timing.
			want := prefix + s.authToken
			if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
				s.logger.Warn("webui: rejected request with missing/invalid token",
					"remote", r.RemoteAddr, "path", r.URL.Path)
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
