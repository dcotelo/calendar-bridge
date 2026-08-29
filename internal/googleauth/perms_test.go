package googleauth

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// countingHandler counts the log records it receives, for asserting how many
// times a warning fired without depending on message text.
type countingHandler struct {
	count int
}

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *countingHandler) WithGroup(string) slog.Handler            { return h }
func (h *countingHandler) Handle(_ context.Context, _ slog.Record) error {
	h.count++
	return nil
}

func TestCheckSecretPerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits not meaningful on Windows")
	}

	dir := t.TempDir()

	cases := []struct {
		name         string
		mode         os.FileMode
		wantInsecure bool
	}{
		{"owner only 600", 0o600, false},
		{"owner read only 400", 0o400, false},
		{"group readable 640", 0o640, true},
		{"world readable 604", 0o604, true},
		{"group+world 666", 0o666, true},
		{"world executable 601", 0o601, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name)
			if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
				t.Fatalf("write: %v", err)
			}
			// Set the mode explicitly (WriteFile is subject to umask).
			if err := os.Chmod(path, tc.mode); err != nil {
				t.Fatalf("chmod: %v", err)
			}
			mode, insecure := checkSecretPerms(path)
			if insecure != tc.wantInsecure {
				t.Errorf("checkSecretPerms(mode=%o) insecure = %v, want %v", tc.mode, insecure, tc.wantInsecure)
			}
			wantMode := os.FileMode(0)
			if tc.wantInsecure {
				wantMode = tc.mode
			}
			if mode != wantMode {
				t.Errorf("checkSecretPerms(mode=%o) mode = %o, want %o", tc.mode, mode, wantMode)
			}
		})
	}
}

func TestCheckSecretPerms_MissingFileIsNotFlagged(t *testing.T) {
	_, insecure := checkSecretPerms(filepath.Join(t.TempDir(), "does-not-exist"))
	if insecure {
		t.Error("missing file reported as insecure, want not-insecure (absence handled by caller)")
	}
}

func TestWarnIfInsecurePerms(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits not meaningful on Windows")
	}

	dir := t.TempDir()

	insecurePath := filepath.Join(dir, "insecure")
	if err := os.WriteFile(insecurePath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.Chmod(insecurePath, 0o644); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	securePath := filepath.Join(dir, "secure")
	if err := os.WriteFile(securePath, []byte(`{}`), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Run("insecure file logs exactly one warning", func(t *testing.T) {
		h := &countingHandler{}
		warnIfInsecurePerms(slog.New(h), "token", insecurePath)
		if h.count != 1 {
			t.Errorf("warnings logged = %d, want 1", h.count)
		}
	})

	t.Run("secure file logs no warning", func(t *testing.T) {
		h := &countingHandler{}
		warnIfInsecurePerms(slog.New(h), "token", securePath)
		if h.count != 0 {
			t.Errorf("warnings logged = %d, want 0", h.count)
		}
	})

	t.Run("nil logger falls back to default without panicking", func(t *testing.T) {
		// Exercises the nil-logger branch; a secure file keeps this quiet on
		// the real default logger regardless of test output capture.
		warnIfInsecurePerms(nil, "token", securePath)
	})
}
