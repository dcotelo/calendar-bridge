package googleauth

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestClient_MissingTokenFileReportsNeedsAuth(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)

	_, err := Client(context.Background(), creds, filepath.Join(dir, "absent-token.json"), discardLogger())
	if !errors.Is(err, ErrNeedsAuth) {
		t.Fatalf("Client with no token file = %v, want ErrNeedsAuth", err)
	}
}

func TestClient_CorruptTokenFileReportsUnreadable(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)
	tokenPath := writeFile(t, dir, "token.json", `{"access_token": "trunc`)

	_, err := Client(context.Background(), creds, tokenPath, discardLogger())
	if !errors.Is(err, ErrTokenUnreadable) {
		t.Fatalf("Client with a corrupt token file = %v, want ErrTokenUnreadable", err)
	}
	if errors.Is(err, ErrNeedsAuth) {
		t.Error("a corrupt token must not also report as needing auth; the fixes differ")
	}
}

func TestClient_MissingCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	_, err := Client(context.Background(), filepath.Join(dir, "absent.json"), filepath.Join(dir, "t.json"), discardLogger())
	if err == nil {
		t.Fatal("want an error for a missing credentials file")
	}
	if !strings.Contains(err.Error(), "reading credentials file") {
		t.Errorf("error = %v, want it to name the credentials file as the problem", err)
	}
}

func TestClient_MalformedCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", `{"installed": "not an object"}`)
	_, err := Client(context.Background(), creds, filepath.Join(dir, "t.json"), discardLogger())
	if err == nil {
		t.Fatal("want an error for a malformed credentials file")
	}
	if !strings.Contains(err.Error(), "credentials file") {
		t.Errorf("error = %v, want it to name the credentials file", err)
	}
}

func TestClient_SucceedsWithAValidTokenFile(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)
	tokenPath := writeToken(t, dir, "token.json", &oauth2.Token{
		AccessToken:  "fake-access-token",
		RefreshToken: "fake-refresh-token",
		TokenType:    "Bearer",
		Expiry:       time.Now().Add(time.Hour),
	})

	svc, err := Client(context.Background(), creds, tokenPath, discardLogger())
	if err != nil {
		t.Fatalf("Client: %v", err)
	}
	if svc == nil {
		t.Fatal("Client returned a nil service and a nil error")
	}
}

// A token with no refresh token still builds a client — it works until the
// access token expires — but must warn loudly, because it is a time bomb.
func TestClient_WarnsWhenTheTokenHasNoRefreshToken(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)
	tokenPath := writeToken(t, dir, "token.json", &oauth2.Token{
		AccessToken: "fake-access-token",
		TokenType:   "Bearer",
		Expiry:      time.Now().Add(time.Hour),
	})

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))

	if _, err := Client(context.Background(), creds, tokenPath, logger); err != nil {
		t.Fatalf("Client: %v", err)
	}
	if !strings.Contains(logs.String(), "no refresh token") {
		t.Errorf("expected a warning about the missing refresh token, got:\n%s", logs.String())
	}
}

// Log output must never contain token material or the full on-disk path of a
// secret — only the base name, which is enough to act on.
func TestClient_LogsNeverCarryTokenMaterialOrFullPaths(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)
	// #nosec G101 -- fabricated, and used precisely so the test can assert it
	// never appears in a log line.
	const accessToken = "ya29.FAKE-ACCESS-TOKEN-VALUE"
	tokenPath := writeToken(t, dir, "token.json", &oauth2.Token{AccessToken: accessToken})
	// Loosen permissions so the insecure-perms warning fires too.
	// #nosec G302 -- deliberately insecure: this is the condition under test.
	if err := os.Chmod(tokenPath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if _, err := Client(context.Background(), creds, tokenPath, logger); err != nil {
		t.Fatalf("Client: %v", err)
	}

	out := logs.String()
	if out == "" {
		t.Fatal("expected at least the insecure-permissions warning")
	}
	if strings.Contains(out, accessToken) {
		t.Error("logs contain the access token")
	}
	if strings.Contains(out, "NOT-A-REAL-SECRET") {
		t.Error("logs contain the OAuth client secret")
	}
	if strings.Contains(out, dir) {
		t.Errorf("logs contain the full secrets directory path:\n%s", out)
	}
}

func TestClient_NilLoggerIsAccepted(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)
	tokenPath := writeToken(t, dir, "token.json", &oauth2.Token{AccessToken: "a", RefreshToken: "r"})

	if _, err := Client(context.Background(), creds, tokenPath, nil); err != nil {
		t.Fatalf("Client with a nil logger: %v", err)
	}
}

func TestAuthorize_ReportsAMissingCredentialsFile(t *testing.T) {
	dir := t.TempDir()
	err := Authorize(context.Background(), filepath.Join(dir, "absent.json"), filepath.Join(dir, "t.json"))
	if err == nil {
		t.Fatal("want an error for a missing credentials file")
	}
	if !strings.Contains(err.Error(), "reading credentials file") {
		t.Errorf("error = %v", err)
	}
}

func TestLoadOAuthConfig_UsesTheMinimalScopeAndTheCredentialFilesEndpoints(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)

	cfg, err := loadOAuthConfig(creds)
	if err != nil {
		t.Fatalf("loadOAuthConfig: %v", err)
	}
	if len(cfg.Scopes) != 1 || cfg.Scopes[0] != Scopes[0] {
		t.Errorf("Scopes = %v, want %v", cfg.Scopes, Scopes)
	}
	if cfg.ClientID == "" {
		t.Error("ClientID was not read from the credentials file")
	}
	if cfg.Endpoint.TokenURL != "https://oauth2.googleapis.com/token" {
		t.Errorf("TokenURL = %q, want the value from the credentials file", cfg.Endpoint.TokenURL)
	}
}

func TestRandomURLSafe_ProducesDistinctURLSafeValues(t *testing.T) {
	seen := make(map[string]bool, 64)
	for range 64 {
		s, err := randomURLSafe(24)
		if err != nil {
			t.Fatalf("randomURLSafe: %v", err)
		}
		if s == "" {
			t.Fatal("randomURLSafe returned an empty string")
		}
		if strings.ContainsAny(s, "+/= ") {
			t.Errorf("randomURLSafe returned %q, which is not URL-safe", s)
		}
		if seen[s] {
			t.Fatalf("randomURLSafe repeated a value (%q); it is used as an OAuth state", s)
		}
		seen[s] = true
	}
}
