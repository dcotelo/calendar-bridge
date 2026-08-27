// Package googleauth handles the OAuth2 authorization flow and token
// persistence for a single Google account.
package googleauth

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

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
func Client(ctx context.Context, credentialsFile, tokenFile string) (*calendar.Service, error) {
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
	fmt.Print("Paste the authorization code here: ")

	var code string
	if _, err := fmt.Scan(&code); err != nil {
		return fmt.Errorf("reading authorization code: %w", err)
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

func tokenFromFile(path string) (*oauth2.Token, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tok := &oauth2.Token{}
	if err := json.NewDecoder(f).Decode(tok); err != nil {
		return nil, fmt.Errorf("decoding token file %s: %w", path, err)
	}
	return tok, nil
}

func saveToken(path string, tok *oauth2.Token) error {
	// Token files contain live credentials: create with owner-only
	// permissions and never widen them afterward.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	return json.NewEncoder(f).Encode(tok)
}
