package googleauth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// requireEnforcedPermissions skips a test that makes a file unreadable via
// os.Chmod, on platforms where that does not actually prevent reads:
//
//   - Windows: os.Chmod only toggles the read-only attribute. A 0o000 file is
//     still readable, so Client would succeed and the assertion would fail.
//   - root: permission bits are not enforced for uid 0.
func requireEnforcedPermissions(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows: os.Chmod cannot make a file unreadable")
	}
	if os.Geteuid() == 0 {
		t.Skip("running as root: permission bits are not enforced")
	}
}

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
	// Compare file IDENTITY, not mtime. atomicfile.Write replaces the file
	// through a rename, so any unwanted rewrite produces a new inode — whereas
	// mtime has 1-second granularity on some filesystems and would not change
	// across three calls microseconds apart, letting this assertion silently
	// never fail.
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
	if !os.SameFile(before, after) {
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

	// #nosec G101 -- fabricated malformed fixtures, none of them credentials.
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
	// #nosec G101 -- fabricated. The test asserts this string never reaches an
	// error message, which is exactly why it has to look like a token.
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

// fakeCredentials is a syntactically valid Google "installed app" client
// credentials document. Every value is fabricated: there is no real project, no
// real client, and no real secret, and nothing in this suite contacts Google.
//
// #nosec G101 -- fabricated fixture, not a credential. The literal
// "NOT-A-REAL-SECRET" is what makes that unambiguous to a human reader too.
const fakeCredentials = `{
  "installed": {
    "client_id": "000000000000-fakefakefakefakefakefakefake.apps.googleusercontent.com",
    "project_id": "calendar-bridge-test",
    "auth_uri": "https://accounts.google.com/o/oauth2/auth",
    "token_uri": "https://oauth2.googleapis.com/token",
    "client_secret": "NOT-A-REAL-SECRET",
    "redirect_uris": ["http://localhost"]
  }
}`

func writeFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
	return path
}

func writeToken(t *testing.T, dir, name string, tok *oauth2.Token) string {
	t.Helper()
	// #nosec G117 -- serialising a FABRICATED token into a temp directory is the
	// whole point of this helper; no real credential is involved.
	b, err := json.Marshal(tok)
	if err != nil {
		t.Fatalf("marshaling token: %v", err)
	}
	return writeFile(t, dir, name, string(b))
}

// A token file that EXISTS but cannot be opened — wrong ownership after a
// container UID change, a directory permission change, an I/O error — must not
// report as "not yet authorized". That would send the operator to re-run
// `auth`, which cannot fix any of those, and is the exact mis-signalling
// ErrTokenUnreadable was introduced to prevent.
func TestClient_UnopenableTokenFileIsNotReportedAsNeedsAuth(t *testing.T) {
	requireEnforcedPermissions(t)
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)
	tokenPath := writeToken(t, dir, "token.json", &oauth2.Token{AccessToken: "a", RefreshToken: "r"})

	// Unreadable, while very much still present.
	if err := os.Chmod(tokenPath, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(tokenPath, 0o600) })

	_, err := Client(context.Background(), creds, tokenPath, discardLogger())
	if err == nil {
		t.Fatal("want an error for an unreadable token file")
	}
	if errors.Is(err, ErrNeedsAuth) {
		t.Errorf("an unreadable token file reported as ErrNeedsAuth: %v", err)
	}
	if !errors.Is(err, ErrTokenInaccessible) {
		t.Errorf("error = %v, want ErrTokenInaccessible", err)
	}
}

// The three token-file conditions must stay distinguishable: each has a
// different fix, and the CLI prints a different next step for each.
func TestClient_TokenFileConditionsAreDistinguishable(t *testing.T) {
	dir := t.TempDir()
	creds := writeFile(t, dir, "credentials.json", fakeCredentials)

	t.Run("missing reports ErrNeedsAuth", func(t *testing.T) {
		_, err := Client(context.Background(), creds, filepath.Join(dir, "absent.json"), discardLogger())
		if !errors.Is(err, ErrNeedsAuth) {
			t.Errorf("err = %v, want ErrNeedsAuth", err)
		}
		if errors.Is(err, ErrTokenUnreadable) {
			t.Error("a missing file must not also report as unreadable")
		}
	})

	t.Run("corrupt reports ErrTokenUnreadable", func(t *testing.T) {
		p := writeFile(t, dir, "corrupt.json", `{"access_token":"trunc`)
		_, err := Client(context.Background(), creds, p, discardLogger())
		if !errors.Is(err, ErrTokenUnreadable) {
			t.Errorf("err = %v, want ErrTokenUnreadable", err)
		}
		if errors.Is(err, ErrNeedsAuth) {
			t.Error("a corrupt file must not also report as needing auth")
		}
	})
}

// The daemon's log is a shared sink. os.Rename and os.OpenFile failures wrap
// *os.LinkError / *os.PathError, which embed the FULL path no matter how the
// surrounding message is formatted — so the persistence warning must redact the
// directory, matching what warnIfInsecurePerms already does.
func TestPersistingTokenSource_WarningDoesNotDiscloseTheSecretsDirectory(t *testing.T) {
	// A path whose directory is distinctive enough to spot in the output, and
	// which cannot be written to (the parent does not exist).
	dir := filepath.Join(t.TempDir(), "very", "private", "secrets")
	path := filepath.Join(dir, "token.json")

	var logs bytes.Buffer
	src := &persistingTokenSource{
		inner:  &stubSource{tokens: []*oauth2.Token{{AccessToken: "a2", RefreshToken: "rt-2"}}},
		path:   path,
		last:   &oauth2.Token{AccessToken: "a1", RefreshToken: "rt-1"},
		logger: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})),
	}
	if _, err := src.Token(); err != nil {
		t.Fatalf("Token(): %v", err)
	}

	out := logs.String()
	if out == "" {
		t.Fatal("expected a warning about the failed persist")
	}
	if strings.Contains(out, dir) {
		t.Errorf("the warning discloses the secrets directory:\n%s", out)
	}
	// The base name must survive, or the message is not actionable.
	if !strings.Contains(out, "token.json") {
		t.Errorf("the warning does not name the token file:\n%s", out)
	}
}

func TestRedactDir(t *testing.T) {
	cases := []struct {
		name, dir, in, want string
	}{
		{"replaces the directory", "/home/u/secrets",
			"rename /home/u/secrets/.t.tmp /home/u/secrets/t: file exists",
			"rename …/.t.tmp …/t: file exists"},
		{"leaves an unrelated message alone", "/home/u/secrets", "permission denied", "permission denied"},
		{"empty dir is a no-op", "", "/a/b/c failed", "/a/b/c failed"},
		{"root is a no-op", "/", "/a/b/c failed", "/a/b/c failed"},
		{"dot is a no-op", ".", "./x failed", "./x failed"},
		// A backslash root must be a no-op too. On Windows this is the
		// platform separator; on Unix it is simply not a directory anyone
		// redacts. Either way, replacing it would mangle the message. This
		// case failed before the guard listed both separators.
		{"backslash root is a no-op", "\\", `C:\a\b failed`, `C:\a\b failed`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := redactDir(errors.New(tc.in), tc.dir); got != tc.want {
				t.Errorf("redactDir = %q, want %q", got, tc.want)
			}
		})
	}
}

// No error returned by this package may contain the on-disk path it was given.
//
// These errors reach the daemon's stderr, which under systemd is the journal
// and under Docker is `docker logs` — shared sinks. The caller wraps with the
// ACCOUNT NAME, which is what an operator acts on; the path only discloses
// where the secrets live. This test covers every token-file condition plus the
// credentials file.
func TestClient_ErrorsNeverContainTheSuppliedPath(t *testing.T) {
	// A directory name distinctive enough that any leak is unambiguous.
	root := filepath.Join(t.TempDir(), "vErYdIsTiNcTiVeSeCrEtDiR")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}
	creds := writeFile(t, root, "credentials.json", fakeCredentials)

	assertNoPath := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("want an error")
		}
		if strings.Contains(err.Error(), root) {
			t.Errorf("error discloses the secrets directory: %v", err)
		}
		if strings.Contains(err.Error(), "vErYdIsTiNcTiVeSeCrEtDiR") {
			t.Errorf("error discloses a path component: %v", err)
		}
	}

	t.Run("missing token file", func(t *testing.T) {
		_, err := Client(context.Background(), creds, filepath.Join(root, "absent.json"), discardLogger())
		assertNoPath(t, err)
	})

	t.Run("corrupt token file", func(t *testing.T) {
		p := writeFile(t, root, "corrupt.json", `{"access_token":"trunc`)
		_, err := Client(context.Background(), creds, p, discardLogger())
		assertNoPath(t, err)
	})

	t.Run("unopenable token file", func(t *testing.T) {
		requireEnforcedPermissions(t)
		p := writeToken(t, root, "locked.json", &oauth2.Token{AccessToken: "a", RefreshToken: "r"})
		if err := os.Chmod(p, 0o000); err != nil {
			t.Fatalf("chmod: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(p, 0o600) })
		_, err := Client(context.Background(), creds, p, discardLogger())
		assertNoPath(t, err)
	})

	t.Run("missing credentials file", func(t *testing.T) {
		_, err := Client(context.Background(), filepath.Join(root, "absent-creds.json"),
			filepath.Join(root, "t.json"), discardLogger())
		assertNoPath(t, err)
	})

	t.Run("token write failure (rename onto a directory)", func(t *testing.T) {
		// saveToken -> atomicfile.Write, whose rename fails against a directory.
		blocked := filepath.Join(root, "blocked")
		if err := os.Mkdir(blocked, 0o700); err != nil {
			t.Fatalf("seed: %v", err)
		}
		assertNoPath(t, saveToken(blocked, &oauth2.Token{AccessToken: "a", RefreshToken: "r"}))
	})
}

// The two error paths the dedicated redaction test did not reach: a malformed
// (rather than missing) credentials file, and the interactive Authorize flow.
func TestAuthorize_ErrorsNeverContainTheSuppliedPath(t *testing.T) {
	root := filepath.Join(t.TempDir(), "aNoThErDiStInCtIvEdIr")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("seed: %v", err)
	}

	cases := []struct {
		name string
		call func(t *testing.T) error
	}{
		{
			name: "Authorize with a missing credentials file",
			call: func(t *testing.T) error {
				return Authorize(context.Background(),
					filepath.Join(root, "absent.json"), filepath.Join(root, "t.json"))
			},
		},
		{
			name: "Authorize with a malformed credentials file",
			call: func(t *testing.T) error {
				bad := writeFile(t, root, "malformed.json", `{"installed": "not an object"}`)
				return Authorize(context.Background(), bad, filepath.Join(root, "t.json"))
			},
		},
		{
			name: "Client with a malformed credentials file",
			call: func(t *testing.T) error {
				bad := writeFile(t, root, "malformed2.json", `{"installed": "not an object"}`)
				_, err := Client(context.Background(), bad, filepath.Join(root, "t.json"), discardLogger())
				return err
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(t)
			if err == nil {
				t.Fatal("want an error")
			}
			// Both the full path and the leaf name: a bare base name is still
			// a disclosure when the directory itself is the secret.
			if strings.Contains(err.Error(), root) || strings.Contains(err.Error(), "aNoThErDiStInCtIvEdIr") {
				t.Errorf("error discloses the secrets directory: %v", err)
			}
		})
	}
}
