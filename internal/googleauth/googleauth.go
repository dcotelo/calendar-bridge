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
	"io/fs"
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

// ErrTokenInaccessible is returned when a token file exists but cannot be
// opened at all — wrong ownership after a container UID change, a directory
// permission change, an I/O error. Distinct from ErrTokenUnreadable (which
// means "opened, but is not a token") because the fixes differ: one is a
// filesystem problem, the other needs a fresh `auth` run.
var ErrTokenInaccessible = errors.New("token file exists but could not be opened")

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
		// None of these carry the token path. They travel to the daemon's
		// stderr, which under systemd is the journal and under Docker is
		// `docker logs` — shared sinks. The caller wraps with the ACCOUNT NAME
		// (see buildEngine), which is what the operator actually acts on:
		// `calendar-bridge auth -account <name>`. The path adds nothing they
		// don't already have in their own config, and discloses where the
		// secrets live.
		switch {
		case errors.Is(err, fs.ErrNotExist):
			// The only condition that actually means "never authorized".
			return nil, ErrNeedsAuth
		case errors.Is(err, ErrTokenUnreadable):
			return nil, ErrTokenUnreadable
		default:
			// Exists but could not be opened — wrong ownership after a
			// container UID change, a directory permission change, an I/O
			// error. Reporting any of these as "not yet authorized" would send
			// the operator to re-run `auth`, which cannot fix them, and is the
			// exact mis-signalling ErrTokenUnreadable was introduced to stop.
			return nil, fmt.Errorf("%w: %w", ErrTokenInaccessible, pathFreeErr(err))
		}
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
		return errors.New("no refresh token was returned; without one this account would stop working within the hour. " +
			"Revoke calendar-bridge's access at https://myaccount.google.com/permissions and run `auth` again")
	}

	if err := saveToken(tokenFile, tok); err != nil {
		return fmt.Errorf("saving token to %s: %w", filepath.Base(tokenFile), err)
	}
	// Base name only: `auth` is interactive, but its stdout can be captured by
	// a wrapper script, and nothing else in this package puts a credential path
	// into output any more. The operator supplied the path in their own config.
	fmt.Printf("Token saved to %s\n", filepath.Base(tokenFile))
	return nil
}

// loadOAuthConfig reads and parses an OAuth2 client credentials JSON file.
func loadOAuthConfig(credentialsFile string) (*oauth2.Config, error) {
	// #nosec G304 -- credentialsFile comes from the user's own config.yaml,
	// not untrusted external input.
	credBytes, err := os.ReadFile(credentialsFile)
	if err != nil {
		return nil, fmt.Errorf("reading credentials file %s: %w", filepath.Base(credentialsFile), pathFreeErr(err))
	}
	config, err := google.ConfigFromJSON(credBytes, Scopes...)
	if err != nil {
		return nil, fmt.Errorf("parsing credentials file %s: %w", filepath.Base(credentialsFile), err)
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
// When the input is a URL, its state parameter must be present and must match
// wantState. A bare code carries no state to check — the operator vouches for
// it by pasting it — but a URL is machine-generated, so a missing or mismatched
// state means it did not come from this run, which is the shape a
// swapped-authorization attack takes.
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
		// Require the state to be present AND to match. Google always echoes
		// the state we sent, so a redirect URL without one did not come from
		// this authorization request. Accepting a missing state would let an
		// attacker-supplied URL bypass the check simply by omitting the
		// parameter. PKCE already blocks the resulting code injection, so this
		// is defence in depth rather than the only barrier — which is exactly
		// why it should not be optional.
		if q.Get("state") != wantState {
			return "", errors.New("the redirect URL's state parameter is missing or does not match this authorization " +
				"request; it did not come from this `auth` run — start over rather than pasting it")
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
		// This runs in the daemon, so it lands in journald / Docker logs /
		// wherever stdout is shipped. Redact the directory: os.Rename and
		// os.OpenFile failures wrap *os.LinkError / *os.PathError, which embed
		// the FULL path regardless of how the message above is formatted, and
		// this package deliberately logs only base names elsewhere (see
		// warnIfInsecurePerms).
		s.logger.Warn("could not persist refreshed OAuth token; it will be refreshed again next start",
			"token_file", filepath.Base(s.path), "error", redactDir(err, filepath.Dir(s.path)))
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

// redactDir replaces every occurrence of dir in err's message with an ellipsis,
// so a wrapped *os.PathError or *os.LinkError cannot disclose the on-disk
// location of the secrets directory through a shared log sink. The base names
// survive, which is what makes the message actionable.
//
// Returns a string rather than an error: the only caller is a log field, and
// producing a new error type here would risk callers matching on the redacted
// value instead of the real cause.
func redactDir(err error, dir string) string {
	msg := err.Error()
	// A root or empty directory carries no secret worth redacting, and
	// replacing a one-character path like "/" would mangle every separator in
	// the message instead — turning "/a/b/c failed" into "…a…b…c failed".
	//
	// Both separators are listed rather than just this platform's: config files
	// are operator-written and may carry Unix-style paths on Windows, where
	// filepath.Separator is a backslash and "/" would otherwise fall through to
	// the ReplaceAll below. The mangling is silent when it happens.
	switch dir {
	case "", ".", "/", "\\":
		return msg
	}
	// A volume root such as "C:\\" — filepath.Dir of a root is itself.
	if filepath.Dir(dir) == dir {
		return msg
	}
	return strings.ReplaceAll(msg, dir, "…")
}

// pathFreeErr strips filesystem paths out of an OS error, keeping the cause.
// See the equivalent in internal/atomicfile for why this matters: *fs.PathError
// embeds the full path regardless of the wrapping format string, and these
// errors reach shared log sinks.
func pathFreeErr(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
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
		// Deliberately does not include the decode error: it can quote file
		// contents, i.e. the token itself.
		return nil, ErrTokenUnreadable
	}
	if tok.AccessToken == "" && tok.RefreshToken == "" {
		// Valid JSON that is not a token (e.g. "{}" left by an interrupted
		// write from an older, non-atomic version of this code).
		return nil, ErrTokenUnreadable
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
