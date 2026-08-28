// Package googleauth handles the OAuth2 authorization flow and token
// persistence for a single Google account.
package googleauth

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"strings"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	calendar "google.golang.org/api/calendar/v3"
	"google.golang.org/api/option"
)

// Scopes requested for calendar-bridge. We need read access to detect
// events, and write access to create/update/delete the busy blocks we own.
var Scopes = []string{calendar.CalendarEventsScope}

// Client returns an authenticated Calendar API client for one account,
// using the OAuth2 client credentials at credentialsFile and the cached
// token at tokenFile.
//
// If tokenFile does not exist, Client returns ErrNeedsAuth so the caller can
// run the interactive authorization flow (see Authorize).
//
// logger, if non-nil, is used to warn when a credentials or token file has
// insecure (group/world-accessible) permissions. Pass nil to use the default
// logger.
func Client(ctx context.Context, credentialsFile, tokenFile string, logger *slog.Logger) (*calendar.Service, error) {
	// Warn loudly if either secret file is readable beyond its owner. These
	// are live credentials; the read path (run/sync-once) never rewrites them,
	// so this is the only place a loosened credentials/token file gets caught.
	warnIfInsecurePerms(logger, "credentials", credentialsFile)
	warnIfInsecurePerms(logger, "token", tokenFile)

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

	tok, err := tokenFromFile(tokenFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrNeedsAuth, tokenFile)
	}

	httpClient := config.Client(ctx, tok)
	svc, err := calendar.NewService(ctx, option.WithHTTPClient(httpClient))
	if err != nil {
		return nil, fmt.Errorf("creating calendar service: %w", err)
	}
	return svc, nil
}

// ErrNeedsAuth is returned when no valid token is cached for an account and
// the interactive authorization flow must be run first.
var ErrNeedsAuth = fmt.Errorf("account not yet authorized, run: calendar-bridge auth <account-name>")

// Authorize runs the interactive OAuth2 flow for one account and persists
// the resulting token to tokenFile. Intended to be invoked from a CLI
// subcommand, not from the sync loop.
func Authorize(ctx context.Context, credentialsFile, tokenFile string) error {
	warnIfInsecurePerms(nil, "credentials", credentialsFile)

	// #nosec G304 -- credentialsFile comes from the user's own config.yaml,
	// not untrusted external input.
	credBytes, err := os.ReadFile(credentialsFile)
	if err != nil {
		return fmt.Errorf("reading credentials file %s: %w", credentialsFile, err)
	}

	config, err := google.ConfigFromJSON(credBytes, Scopes...)
	if err != nil {
		return fmt.Errorf("parsing credentials file %s: %w", credentialsFile, err)
	}

	authURL := config.AuthCodeURL("state-token", oauth2.AccessTypeOffline)
	fmt.Printf("Open this URL in a browser and authorize access:\n\n%s\n\n", authURL)
	fmt.Print("Paste the authorization code, or the full redirect URL, here: ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return fmt.Errorf("reading authorization code: %w", err)
	}
	code, err := extractAuthCode(line)
	if err != nil {
		return fmt.Errorf("could not find an authorization code in the pasted input: %w", err)
	}

	tok, err := config.Exchange(ctx, code)
	if err != nil {
		return fmt.Errorf("exchanging authorization code: %w", err)
	}

	if err := saveToken(tokenFile, tok); err != nil {
		return fmt.Errorf("saving token to %s: %w", tokenFile, err)
	}
	fmt.Printf("Token saved to %s\n", tokenFile)
	return nil
}

// extractAuthCode accepts either a bare OAuth2 authorization code or the
// full redirect URL Google sends the browser to after consent
// (e.g. "http://localhost:1/?code=4/0A...&scope=..."), and returns just the
// code. Most users copy the whole URL rather than picking the code out of
// it by hand, so both forms must work.
func extractAuthCode(input string) (string, error) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", fmt.Errorf("empty input")
	}

	if strings.Contains(trimmed, "://") {
		u, err := url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("parsing as URL: %w", err)
		}
		code := u.Query().Get("code")
		if code == "" {
			return "", fmt.Errorf("URL has no ?code= parameter")
		}
		return code, nil
	}

	return trimmed, nil
}

func tokenFromFile(path string) (*oauth2.Token, error) {
	// #nosec G304 -- path comes from the user's own config.yaml (see
	// internal/config), not from untrusted external input. Reading an
	// arbitrary local file the user configured is the intended behavior.
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }() // read-only handle; nothing to flush, safe to ignore

	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, fmt.Errorf("decoding token file %s: %w", path, err)
	}
	return tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	// Token files contain live credentials: create with owner-only
	// permissions and never widen them afterward.
	// #nosec G304 -- path comes from the user's own config.yaml, not
	// untrusted external input; writing the token file here is the
	// intended purpose of this function.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// The 0o600 mode above only applies to newly-created files; if path
	// already existed (e.g. re-running `auth` to refresh a token) with
	// looser permissions from an older version of this tool or a manual
	// edit, OpenFile does NOT tighten them. Force it explicitly so a
	// stale, world- or group-readable token file can never survive a
	// save.
	if chmodErr := f.Chmod(0o600); chmodErr != nil {
		_ = f.Close()
		return fmt.Errorf("chmod token file %s to 0600: %w", path, chmodErr)
	}
	// #nosec G117 -- this file IS the on-disk token store; the whole point
	// of this function is to persist the OAuth2 token (including its
	// access token) locally at 0600, alongside the credentials file the
	// user already configured. That's expected, not a leak.
	encErr := json.NewEncoder(f).Encode(tok)
	if closeErr := f.Close(); closeErr != nil && encErr == nil {
		// A failed close on a write-mode file can mean buffered data never
		// made it to disk — surface it rather than silently dropping it.
		return fmt.Errorf("closing token file %s: %w", path, closeErr)
	}
	return encErr
}
