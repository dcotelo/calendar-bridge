package googleauth

import (
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// stubSource returns a fixed sequence of tokens, one per Token() call, then
// repeats the last one.
type stubSource struct {
	tokens []*oauth2.Token
	err    error
	calls  int
}

func (s *stubSource) Token() (*oauth2.Token, error) {
	if s.err != nil {
		return nil, s.err
	}
	i := s.calls
	s.calls++
	if i >= len(s.tokens) {
		i = len(s.tokens) - 1
	}
	return s.tokens[i], nil
}

func readToken(t *testing.T, path string) *oauth2.Token {
	t.Helper()
	tok, err := tokenFromFile(path)
	if err != nil {
		t.Fatalf("tokenFromFile(%s): %v", path, err)
	}
	return tok
}

func TestPersistingTokenSource_WritesRefreshedAccessToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	original := &oauth2.Token{AccessToken: "old-access", RefreshToken: "rt-1", Expiry: time.Now().Add(-time.Minute)}
	if err := saveToken(path, original); err != nil {
		t.Fatalf("seed: %v", err)
	}

	refreshed := &oauth2.Token{AccessToken: "new-access", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour)}
	src := &persistingTokenSource{
		inner:  &stubSource{tokens: []*oauth2.Token{refreshed}},
		path:   path,
		last:   original,
		logger: discardLogger(),
	}

	got, err := src.Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if got.AccessToken != "new-access" {
		t.Errorf("returned AccessToken = %q, want %q", got.AccessToken, "new-access")
	}
	if onDisk := readToken(t, path).AccessToken; onDisk != "new-access" {
		t.Errorf("on-disk AccessToken = %q, want %q — a refreshed token must be persisted", onDisk, "new-access")
	}
}

func TestPersistingTokenSource_WritesRotatedRefreshToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	original := &oauth2.Token{AccessToken: "a1", RefreshToken: "rt-1"}
	if err := saveToken(path, original); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rotated := &oauth2.Token{AccessToken: "a2", RefreshToken: "rt-2"}
	src := &persistingTokenSource{
		inner:  &stubSource{tokens: []*oauth2.Token{rotated}},
		path:   path,
		last:   original,
		logger: discardLogger(),
	}
	if _, err := src.Token(); err != nil {
		t.Fatalf("Token(): %v", err)
	}

	if onDisk := readToken(t, path).RefreshToken; onDisk != "rt-2" {
		t.Errorf("on-disk RefreshToken = %q, want %q — a rotated refresh token is the case that most needs persisting", onDisk, "rt-2")
	}
}

// The refresh response omits refresh_token when the server does not rotate it.
// Writing that through would destroy the only copy of a credential that cannot
// be re-obtained without another consent screen.
func TestPersistingTokenSource_NeverErasesAnExistingRefreshToken(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	original := &oauth2.Token{AccessToken: "a1", RefreshToken: "rt-1"}
	if err := saveToken(path, original); err != nil {
		t.Fatalf("seed: %v", err)
	}

	withoutRefresh := &oauth2.Token{AccessToken: "a2"} // no RefreshToken
	src := &persistingTokenSource{
		inner:  &stubSource{tokens: []*oauth2.Token{withoutRefresh}},
		path:   path,
		last:   original,
		logger: discardLogger(),
	}
	if _, err := src.Token(); err != nil {
		t.Fatalf("Token(): %v", err)
	}

	onDisk := readToken(t, path)
	if onDisk.RefreshToken != "rt-1" {
		t.Errorf("on-disk RefreshToken = %q, want %q — an omitted refresh token must not erase the stored one", onDisk.RefreshToken, "rt-1")
	}
	if onDisk.AccessToken != "a2" {
		t.Errorf("on-disk AccessToken = %q, want %q", onDisk.AccessToken, "a2")
	}
}

func TestPersistingTokenSource_UnchangedTokenDoesNotRewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	tok := &oauth2.Token{AccessToken: "a1", RefreshToken: "rt-1", Expiry: time.Now().Add(time.Hour).Round(time.Second)}
	if err := saveToken(path, tok); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	src := &persistingTokenSource{
		inner:  &stubSource{tokens: []*oauth2.Token{tok}},
		path:   path,
		last:   tok,
		logger: discardLogger(),
	}
	for range 3 {
		if _, err := src.Token(); err != nil {
			t.Fatalf("Token(): %v", err)
		}
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("an unchanged token was rewritten; the poll loop would rewrite the token file on every API call")
	}
}

// A disk problem must not become an API outage: the token in hand is valid.
func TestPersistingTokenSource_WriteFailureStillReturnsTheToken(t *testing.T) {
	// A path inside a nonexistent directory can never be written.
	path := filepath.Join(t.TempDir(), "missing-dir", "token.json")
	refreshed := &oauth2.Token{AccessToken: "a2", RefreshToken: "rt-2"}
	src := &persistingTokenSource{
		inner:  &stubSource{tokens: []*oauth2.Token{refreshed}},
		path:   path,
		last:   &oauth2.Token{AccessToken: "a1", RefreshToken: "rt-1"},
		logger: discardLogger(),
	}

	got, err := src.Token()
	if err != nil {
		t.Fatalf("Token() error = %v, want nil (a failed cache write must not fail the call)", err)
	}
	if got.AccessToken != "a2" {
		t.Errorf("AccessToken = %q, want %q", got.AccessToken, "a2")
	}
}

func TestPersistingTokenSource_PropagatesInnerError(t *testing.T) {
	wantErr := errors.New("refresh rejected")
	src := &persistingTokenSource{
		inner:  &stubSource{err: wantErr},
		path:   filepath.Join(t.TempDir(), "token.json"),
		logger: discardLogger(),
	}
	if _, err := src.Token(); !errors.Is(err, wantErr) {
		t.Errorf("Token() error = %v, want %v", err, wantErr)
	}
}

func TestTokenFromFile_DistinguishesMissingFromCorrupt(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing file is not ErrTokenUnreadable", func(t *testing.T) {
		_, err := tokenFromFile(filepath.Join(dir, "absent.json"))
		if err == nil {
			t.Fatal("want an error for a missing token file")
		}
		if errors.Is(err, ErrTokenUnreadable) {
			t.Error("a missing file must report as needing auth, not as corrupt")
		}
	})

	cases := map[string]string{
		"truncated json":    `{"access_token":"abc`,
		"not json at all":   `this is not json`,
		"empty file":        ``,
		"valid but empty":   `{}`,
		"json but no token": `{"scope":"calendar"}`,
	}
	for name, contents := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "tok-"+name+".json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("seed: %v", err)
			}
			_, err := tokenFromFile(path)
			if !errors.Is(err, ErrTokenUnreadable) {
				t.Errorf("tokenFromFile(%q) error = %v, want ErrTokenUnreadable", contents, err)
			}
		})
	}
}

// A corrupt token file must not have its contents quoted into the error, which
// would put an access token into logs.
func TestTokenFromFile_ErrorDoesNotQuoteFileContents(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token.json")
	const secret = "ya29.SUPER-SECRET-ACCESS-TOKEN"
	if err := os.WriteFile(path, []byte(`{"access_token":"`+secret), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	_, err := tokenFromFile(path)
	if err == nil {
		t.Fatal("want an error")
	}
	if got := err.Error(); strings.Contains(got, secret) {
		t.Errorf("error message %q leaks the token contents", got)
	}
}

// The auth URL must carry every parameter the flow depends on. Getting any of
// these wrong is silent — the flow still completes, and the account breaks an
// hour later.
func TestAuthCodeURL_CarriesOfflineForcedConsentAndPKCE(t *testing.T) {
	cfg := &oauth2.Config{
		ClientID:    "test-client-id",
		RedirectURL: "http://localhost",
		Endpoint:    oauth2.Endpoint{AuthURL: "https://accounts.google.com/o/oauth2/auth"},
		Scopes:      Scopes,
	}
	verifier := oauth2.GenerateVerifier()
	raw := cfg.AuthCodeURL("the-state",
		oauth2.AccessTypeOffline,
		oauth2.ApprovalForce,
		oauth2.S256ChallengeOption(verifier),
	)

	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parsing auth URL: %v", err)
	}
	q := u.Query()

	want := map[string]string{
		"access_type":           "offline",
		"prompt":                "consent",
		"state":                 "the-state",
		"code_challenge_method": "S256",
	}
	for k, v := range want {
		if got := q.Get(k); got != v {
			t.Errorf("auth URL %s = %q, want %q", k, got, v)
		}
	}
	if q.Get("code_challenge") == "" {
		t.Error("auth URL is missing code_challenge (PKCE)")
	}
	if got := q.Get("scope"); got != Scopes[0] {
		t.Errorf("auth URL scope = %q, want exactly %q", got, Scopes[0])
	}
}

// Guard the scope set itself: widening it is a security-relevant change that
// should never happen as a side effect of some other edit.
func TestScopes_AreMinimal(t *testing.T) {
	if len(Scopes) != 1 {
		t.Fatalf("Scopes = %v, want exactly one scope", Scopes)
	}
	const want = "https://www.googleapis.com/auth/calendar.events"
	if Scopes[0] != want {
		t.Errorf("Scopes[0] = %q, want %q — widening the OAuth scope needs an explicit, reviewed decision", Scopes[0], want)
	}
}
