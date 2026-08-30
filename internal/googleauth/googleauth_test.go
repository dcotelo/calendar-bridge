package googleauth

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/oauth2"
)

func TestExtractAuthCode(t *testing.T) {
	const wantState = "test-state-value"

	cases := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{
			name:  "bare code",
			input: "4/0AVGzR1abcXYZ123\n",
			want:  "4/0AVGzR1abcXYZ123",
		},
		{
			name:  "bare code no trailing newline",
			input: "4/0AVGzR1abcXYZ123",
			want:  "4/0AVGzR1abcXYZ123",
		},
		{
			name:  "full redirect URL",
			input: "http://localhost:1/?code=4%2F0AVGzR1abcXYZ123&scope=https://www.googleapis.com/auth/calendar.events\n",
			want:  "4/0AVGzR1abcXYZ123",
		},
		{
			name:  "https redirect URL with matching state",
			input: "https://localhost/?state=test-state-value&code=abc123&scope=foo\n",
			want:  "abc123",
		},
		{
			// A URL with no state at all is still accepted: some clients omit
			// it on the redirect, and the bare-code path has no state either.
			name:  "redirect URL with no state parameter",
			input: "https://localhost/?code=abc123&scope=foo\n",
			want:  "abc123",
		},
		{
			// The swapped-authorization shape: a code from someone else's
			// consent, which would bind calendar-bridge to their calendar.
			name:    "redirect URL with mismatched state is refused",
			input:   "https://localhost/?state=attacker-state&code=abc123&scope=foo\n",
			wantErr: true,
		},
		{
			name:    "redirect URL carrying an error parameter",
			input:   "https://localhost/?error=access_denied&state=test-state-value\n",
			wantErr: true,
		},
		{
			name:  "surrounding whitespace",
			input: "   4/0AVGzR1abcXYZ123   \n",
			want:  "4/0AVGzR1abcXYZ123",
		},
		{
			name:    "empty input",
			input:   "\n",
			wantErr: true,
		},
		{
			name:    "URL missing code param",
			input:   "http://localhost:1/?scope=foo\n",
			wantErr: true,
		},
		{
			name:    "malformed URL",
			input:   "http://[::1\n",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extractAuthCode(tc.input, wantState)
			if tc.wantErr {
				if err == nil {
					t.Errorf("extractAuthCode(%q) error = nil, want error", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("extractAuthCode(%q) unexpected error: %v", tc.input, err)
			}
			if got != tc.want {
				t.Errorf("extractAuthCode(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestSaveToken_TightensExistingFilePermissions is a regression test: a
// pre-existing token file with looser-than-0600 permissions (e.g. left
// over from an older version of this tool, or a manual `chmod`) must have
// its permissions tightened on every save, not just at creation time.
// os.OpenFile's mode argument only applies when it actually creates the
// file — it silently leaves an existing file's permissions untouched
// otherwise.
func TestSaveToken_TightensExistingFilePermissions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token.json")

	// Pre-create the file with permissive mode, as if it predated the
	// 0600 requirement or was manually loosened.
	// #nosec G306 -- intentionally permissive: this is the exact stale
	// state the test exists to exercise and fix, not the code under test.
	if err := os.WriteFile(path, []byte(`{}`), 0o644); err != nil {
		t.Fatalf("seeding pre-existing token file: %v", err)
	}

	tok := &oauth2.Token{AccessToken: "test-access-token"}
	if err := saveToken(path, tok); err != nil {
		t.Fatalf("saveToken() error = %v, want nil", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat token file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("token file mode after saveToken = %o, want 0600 (existing file's looser permissions must be tightened, not just skipped)", got)
	}

	// Confirm the token itself actually round-trips, not just the mode.
	got, err := tokenFromFile(path)
	if err != nil {
		t.Fatalf("tokenFromFile() error = %v, want nil", err)
	}
	if got.AccessToken != tok.AccessToken {
		t.Errorf("tokenFromFile().AccessToken = %q, want %q", got.AccessToken, tok.AccessToken)
	}
}
