package googleauth

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

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
			_, insecure := checkSecretPerms(path)
			if insecure != tc.wantInsecure {
				t.Errorf("checkSecretPerms(mode=%o) insecure = %v, want %v", tc.mode, insecure, tc.wantInsecure)
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
