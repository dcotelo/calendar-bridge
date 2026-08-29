// Package webui serves a local, self-hosted configuration UI for
// calendar-bridge.
//
// # Security model
//
// The UI can read and rewrite config.yaml, so it is a privileged local admin
// surface and is treated as one:
//
//   - It binds loopback (127.0.0.1) only. It REFUSES to bind a non-loopback
//     address because it serves plaintext HTTP; reach it remotely via an SSH
//     tunnel or a TLS-terminating reverse proxy pointed at the loopback port.
//   - When an auth token is set, the API endpoints (/api/*) require it as a
//     "Authorization: Bearer <token>" header, compared in constant time. The
//     index page (GET /) is intentionally public — a browser can't attach the
//     header to a top-level navigation, so the page loads and then collects
//     the token for its API calls. The page carries no secrets.
//   - It never reads or serves the CONTENTS of credential or token files. The
//     UI edits config fields (including the file paths), exactly like editing
//     config.yaml by hand — the OAuth secrets themselves never enter the
//     browser. The interactive OAuth `auth` flow stays in the CLI.
//   - Config writes go through config.Validate and config.Save (atomic, 0600),
//     so the UI can never persist an invalid or world-readable config.
package webui

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
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
	listenAddr string // configured bind authority, used to reject rebinding
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
// binding any non-loopback address is refused (AuthToken notwithstanding),
// so the UI is never directly exposed to a network over plaintext HTTP.
func New(opts Options) (*Server, error) {
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	if opts.ConfigPath == "" {
		return nil, fmt.Errorf("webui: ConfigPath is required")
	}
	// Refuse any non-loopback bind. The server speaks plaintext HTTP, so a
	// non-loopback listener would send "Authorization: Bearer <token>" (and the
	// config) in the clear to any on-path observer. We do not terminate TLS
	// here. To reach the UI from another host, forward the loopback port over
	// an SSH tunnel or put a TLS-terminating reverse proxy in front of it (see
	// docs/web-ui.md) — the proxy connects to this loopback listener locally.
	if !config.IsLoopbackAddr(opts.ListenAddr) {
		return nil, fmt.Errorf("webui: refusing to bind non-loopback address %q: the UI serves plaintext HTTP and would "+
			"expose the auth token; bind a loopback address (e.g. 127.0.0.1:8090) and reach it via an SSH tunnel or a "+
			"TLS-terminating reverse proxy", opts.ListenAddr)
	}
	return &Server{
		configPath: opts.ConfigPath,
		authToken:  opts.AuthToken,
		listenAddr: opts.ListenAddr,
		logger:     logger,
		syncFn:     opts.Sync,
		status:     opts.Status,
	}, nil
}

// Handler returns the http.Handler serving the UI and API.
//
// GET / (the page itself) is served WITHOUT the auth gate: a browser cannot
// attach an Authorization header to a top-level navigation, so gating the page
// would make an authenticated deployment unreachable. The page contains no
// secrets — it collects the token from the operator and sends it on API calls.
// The auth gate and the CSRF origin check apply to /api/* only.
func (s *Server) Handler() http.Handler {
	api := http.NewServeMux()
	api.HandleFunc("GET /api/config", s.handleGetConfig)
	api.HandleFunc("PUT /api/config", s.handlePutConfig)
	api.HandleFunc("GET /api/status", s.handleGetStatus)
	api.HandleFunc("POST /api/sync", s.handleSync)

	root := http.NewServeMux()
	root.Handle("/api/", s.authMiddleware(s.csrfGuard(api)))
	root.HandleFunc("/", s.handleIndex)
	return root
}

// csrfGuard rejects cross-site state-changing requests. In the default no-token
// loopback mode authMiddleware is a pass-through, so without this a page the
// operator visits could drive a cross-origin form POST to /api/sync. We reject
// any request whose Sec-Fetch-Site is present and not same-origin/none, or
// whose Origin is set and doesn't match the request host.
func (s *Server) csrfGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DNS-rebinding defense: in the default no-token mode, an attacker page
		// on a rebound hostname can send matching Origin/Host to the loopback
		// listener. Require the request Host to be a loopback authority so a
		// rebound public hostname is rejected. When an auth token is
		// configured, the token itself is the guard (a rebinding attacker
		// can't obtain it), so we don't additionally constrain Host.
		if s.authToken == "" && !hostIsLoopbackAuthority(r.Host) {
			http.Error(w, "unexpected Host", http.StatusForbidden)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err != nil || u.Host != r.Host {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// hostIsLoopbackAuthority reports whether an HTTP Host header names the local
// loopback interface (127.0.0.0/8, ::1, or "localhost"), rejecting any public
// hostname a DNS-rebinding attacker might resolve to the loopback listener.
func hostIsLoopbackAuthority(host string) bool {
	if host == "" {
		return false
	}
	h, _, err := net.SplitHostPort(host)
	if err != nil {
		// No port present. Strip brackets from a bare IPv6 literal like "[::1]"
		// so net.ParseIP can recognize it — but only when balanced, so an
		// unbalanced "[::1" or "::1]" is rejected rather than silently accepted.
		switch {
		case strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]"):
			h = host[1 : len(host)-1]
		case strings.ContainsAny(host, "[]"):
			return false // unbalanced brackets
		default:
			h = host
		}
	}
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// authMiddleware enforces the Bearer token (when configured) in constant time.
// When no token is configured (loopback-only default), it is a pass-through.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.authToken != "" {
			if !s.validToken(r.Header.Get("Authorization")) {
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

// validToken checks an Authorization header value against the configured token.
// The scheme ("Bearer") is matched case-insensitively per RFC 7235, and the
// token is compared using fixed-size SHA-256 digests so neither length nor
// content leaks through timing.
func (s *Server) validToken(authHeader string) bool {
	scheme, token, ok := strings.Cut(authHeader, " ")
	if !ok || !strings.EqualFold(scheme, "bearer") {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(token)))
	want := sha256.Sum256([]byte(s.authToken))
	return subtle.ConstantTimeCompare(got[:], want[:]) == 1
}
