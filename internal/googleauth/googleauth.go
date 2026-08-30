// Package googleauth handles the OAuth2 authorization flow and token
// persistence for a single Google account.
package googleauth

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"

	"github.com/dcotelo/calendar-bridge/internal/atomicfile"
)

// Scopes requested for calendar-bridge.
//
// calendar.events is the narrowest scope that permits what the engine must do:
// read events to detect busy time, and create/update/delete the busy blocks it
// owns. No narrower Google scope allows event writes. It does also grant read
// access to event titles, attendees and descriptions — more than the engine
// uses. That excess is contained structurally rather than by scope: the neutral
// sync.Event model carries no content fields at all, so there is nowhere for
// event content to flow even if a future change tried.
//
// Deliberately NOT requested: the full "calendar" scope (which would also
// permit calendar creation, deletion and ACL changes).
var Scopes = []string{calendar.CalendarEventsScope}

// ErrNeedsAuth is returned when no token is cached for an account and the
// interactive authorization flow must be run first.
var ErrNeedsAuth = errors.New("account not yet authorized, run: calendar-bridge auth -account <account-name>")

// ErrTokenUnreadable is returned when a token file exists but cannot be parsed
// as an OAuth2 token. It is deliberately distinct from ErrNeedsAuth: a
// truncated or corrupted token file is a different problem with a different
// fix, and reporting it as "not yet authorized" sends the operator down the
// wrong path.
var ErrTokenUnreadable = errors.New("token file exists but could not be read as an OAuth2 token")

// Client returns an authenticated Calendar API client for one account, using
// the OAuth2 client credentials at credentialsFile and the cached token at
// tokenFile.
//
// The token source persists refreshed tokens back to tokenFile (see
// persistingTokenSource), so a rotated refresh token or a renewed access token
// survives a restart instead of going stale on disk.
//
// If tokenFile does not exist, Client returns ErrNeedsAuth so the caller can
// run the interactive authorization flow (see Authorize). If it exists but is
// unparseable, Client returns ErrTokenUnreadable.
//
// logger, if non-nil, is used to warn when a credentials or token file has
// insecure (group/world-accessible) permissions. Pass nil to use the default
// logger.
func Client(ctx context.Context, credentialsFile, tokenFile string, logger *slog.Logger) (*calendar.Service, error) {
	if logger == nil {
		logger = slog.Default()
	}
	// Warn loudly if either secret file is readable beyond its owner. These
	// are live credentials.
	warnIfInsecurePerms(logger, "credentials", credentialsFile)
	warnIfInsecurePerms(logger, "token", tokenFile)

	config, err := loadOAuthConfig(credentialsFile)
	if err != nil {
		return nil, err
	}

	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		if errors.Is(err, ErrTokenUnreadable) {
			// Don't include the decode error: it can quote file contents.
			return nil, fmt.Errorf("%w: %s", ErrTokenUnreadable, tokenFile)
		}
		return nil, fmt.Errorf("%w: %s", ErrNeedsAuth, tokenFile)
	}
	if tok.RefreshToken == "" {
		logger.Warn("token file has no refresh token; this account will stop working when its access token expires. "+
			"Re-run `calendar-bridge auth` for it — calendar-bridge now forces the consent prompt, which makes Google issue one",
			"token_file", filepath.Base(tokenFile))
	}

	src := &persistingTokenSource{
		inner:  config.TokenSource(ctx, tok),
		path:   tokenFile,
		last:   tok,
		logger: logger,
	}
	svc, err := calendar.NewService(ctx, option.WithHTTPClient(oauth2.NewClient(ctx, src)))
	if err != nil {
		return nil, fmt.Errorf("creating calendar service: %w", err)
	}
	return svc, nil
}

// Authorize runs the interactive OAuth2 flow for one account and persists the
// resulting token to tokenFile. Intended to be invoked from a CLI subcommand,
// not from the sync loop.
//
// Three things about this flow are load-bearing:
//
//   - oauth2.ApprovalForce (prompt=consent). Google issues a refresh token for
//     an installed-app flow only on the FIRST authorization of a given
//     client/user pair. Without forcing the consent prompt, re-authorizing an
//     already-granted account returns an access token alone, and the account
//     stops working an hour later — which is exactly the trap someone falls
//     into when they re-run `auth` to fix an expired token.
//   - PKCE. A per-run code verifier binds the authorization code to this
//     process, so an intercepted code cannot be redeemed elsewhere. Google
//     recommends it for installed apps.
//   - A random, verified state. When the operator pastes the full redirect URL
//     back (the common case), its state parameter is checked against the one
//     generated here, so a code from a different authorization — including an
//     attacker's, which would bind calendar-bridge to the attacker's calendar —
//     is rejected rather than silently exchanged.
func Authorize(ctx context.Context, credentialsFile, tokenFile string) error {
	warnIfInsecurePerms(nil, "credentials", credentialsFile)

	config, err := loadOAuthConfig(credentialsFile)
	if err != nil {
		return err
	}

	state, err := randomURLSafe(24)
	if err != nil {
		return fmt.Errorf("generating OAuth state: %w", err)
	}
	verifier := oauth2.GenerateVerifier()

	authURL := config.AuthCodeURL(state,
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier),
	)
	fmt.Printf("Open this URL in a browser and authorize access:\n\n%s\n\n", authURL)
	fmt.Print("Paste the authorization code, or the full redirect URL, here: ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("reading authorization code: %w", err)
	}
	code, err := extractAuthCode(line, state)
	if err != nil {
		return fmt.Errorf("could not find a usable authorization code in the pasted input: %w", err)
	}

	tok, err := config.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return fmt.Errorf("exchanging authorization code: %w", err)
	}
	if tok.RefreshToken == "" {
		return fmt.Errorf("Google returned no refresh token; without one this account would stop working within the hour. " +
			"Revoke calendar-bridge's access at https://myaccount.google.com/permissions and run `auth` again")
	}

	if err := saveToken(tokenFile, tok); err != nil {
		return fmt.Errorf("saving token to %s: %w", tokenFile, err)
	}
	fmt.Printf("Token saved to %s\n", tokenFile)
	return nil
}

// loadOAuthConfig reads and parses an OAuth2 client credentials JSON file.
func loadOAuthConfig(credentialsFile string) (*oauth2.Config, error) {
	// #nosec G304 -- credentialsFile comes from the user's own config.yaml,
	// not untrusted external input.
	credBytes, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("reading credentials file %s: %w", credentialsFile, err)
	}
	config, err := google.ConfigFromJSON(credBytes, Scopes...)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials file %s: %w", credentialsFile, err)
	}
	return config, nil
}

// randomURLSafe returns n cryptographically random bytes, base64url-encoded.
func randomURLSafe(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// extractAuthCode accepts either a bare OAuth2 authorization code or the full
// redirect URL Google sends the browser to after consent
// (e.g. "http://localhost:1/?code=4/0A...&state=...&scope=..."), and returns
// just the code. Most users copy the whole URL rather than picking the code out
// by hand, so both forms must work.
//
// When the input is a URL, its state parameter (if present) must match
// wantState. A bare code carries no state to check — the operator vouches for
// it by pasting it — but a URL that carries a MISMATCHED state is rejected,
// since that is the shape a swapped-authorization attack takes.
func extractAuthCode(input, wantState string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", errors.New("empty input")
	}

	if strings.Contains(trimmed, "://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("parsing as URL: %w", err)
		}
		q := u.Query()
		if errParam := q.Get("error"); errParam != "" {
			return "", fmt.Errorf("the authorization was refused (error=%q)", errParam)
		}
		code := q.Get("code")
		if code == "" {
			return "", errors.New("URL has no ?code= parameter")
		}
		if got := q.Get("state"); got != "" && got != wantState {
			return "", errors.New("the redirect URL's state parameter does not match this authorization request; " +
				"it is from a different (possibly someone else's) `auth` run — start over rather than pasting it")
		}
		return code, nil
	}

	return trimmed, nil
}

// persistingTokenSource wraps an oauth2.TokenSource and writes the token back
// to disk whenever it changes.
//
// The underlying source refreshes in memory only. Without this, a refreshed
// access token is discarded at process exit (costing a refresh round-trip on
// every start), and — more seriously — a ROTATED refresh token is lost
// entirely, leaving the on-disk copy permanently stale. Google rotates refresh
// tokens for apps whose consent screen is in "Testing" publishing status, where
// they also expire after seven days.
//
// A write failure is logged, never returned: the token in hand is still valid,
// and failing the API call because the cache could not be updated would turn a
// recoverable disk problem into an outage.
type persistingTokenSource struct {
	inner  oauth2.TokenSource
	path   string
	logger *slog.Logger

	mu   sync.Mutex
	last *oauth2.Token
}

func (s *persistingTokenSource) Token() (*oauth2.Token, error) {
	tok, err := s.inner.Token()
	if err != nil {
		return nil, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if !tokenChanged(s.last, tok) {
		return tok, nil
	}
	// Never persist a token whose refresh token is empty over one that has
	// it: oauth2 omits the refresh token from a refresh response when the
	// server doesn't rotate it, and writing that through would destroy the
	// only copy of a credential we cannot re-obtain without another consent.
	toSave := tok
	if toSave.RefreshToken == "" && s.last != nil && s.last.RefreshToken != "" {
		clone := *toSave
		clone.RefreshToken = s.last.RefreshToken
		toSave = &clone
	}
	if err := saveToken(s.path, toSave); err != nil {
		s.logger.Warn("could not persist refreshed OAuth token; it will be refreshed again next start",
			"token_file", filepath.Base(s.path), "error", err)
		return tok, nil
	}
	s.last = toSave
	return tok, nil
}

// tokenChanged reports whether anything worth re-persisting differs.
func tokenChanged(old, new *oauth2.Token) bool {
	if old == nil {
		return true
	}
	if old.AccessToken != new.AccessToken || !old.Expiry.Equal(new.Expiry) {
		return true
	}
	// A rotated refresh token is the case that most needs persisting.
	return new.RefreshToken != "" && new.RefreshToken != old.RefreshToken
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	// #nosec G304 -- path comes from the user's own config.yaml (see
	// internal/config), not from untrusted external input.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only handle; nothing to flush

	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrTokenUnreadable, path)
	}
	if tok.AccessToken == "" && tok.RefreshToken == "" {
		// Valid JSON that is not a token (e.g. "{}" left by an interrupted
		// write from an older, non-atomic version of this code).
		return nil, fmt.Errorf("%w: %s", ErrTokenUnreadable, path)
	}
	return tok, nil
}

// saveToken persists tok to path atomically at 0600.
//
// Atomicity matters here as much as it does for config.yaml: an interrupted
// in-place write leaves a truncated token file, which reads back as an
// unusable account until the operator re-runs the whole OAuth flow.
func saveToken(path string, tok *oauth2.Token) error {
	// #nosec G117 -- this file IS the on-disk token store; persisting the
	// OAuth2 token locally at 0600 is the entire purpose of this function.
	data, err := json.Marshal(tok)
	if err != nil {
		return fmt.Errorf("marshaling token: %w", err)
	}
	return atomicfile.Write(path, append(data, '\n'), atomicfile.OwnerOnly)
}
