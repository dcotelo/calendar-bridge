package atomicfile

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWrite_CreatesFileWithContentsAndPerms(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")

	if err := Write(path, []byte("hello"), OwnerOnly); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// #nosec G304 -- path is built from t.TempDir() inside this test.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("contents = %q, want %q", got, "hello")
	}

	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat: %v", err)
		}
		if perm := info.Mode().Perm(); perm != OwnerOnly {
			t.Errorf("mode = %v, want %v", perm, OwnerOnly)
		}
	}
}

func TestWrite_TightensPermissionsOnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission bits are not meaningful on Windows")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")

	// A file left world-readable by an older version or a manual edit must not
	// survive a rewrite with its loose permissions intact.
	// #nosec G306 -- deliberately permissive: this IS the stale state the test
	// exists to exercise, not the behaviour of the code under test.
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Write(path, []byte("new"), OwnerOnly); err != nil {
		t.Fatalf("Write: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != OwnerOnly {
		t.Errorf("mode = %v, want %v (rename must replace the inode, not reuse it)", perm, OwnerOnly)
	}
}

func TestWrite_LeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")

	if err := Write(path, []byte("hello"), OwnerOnly); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != "secret.json" {
		names := make([]string, len(entries))
		for i, e := range entries {
			names[i] = e.Name()
		}
		t.Errorf("directory contains %v, want only [secret.json]", names)
	}
}

func TestWrite_FailsAndCleansUpWhenDirectoryMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-such-dir", "secret.json")
	if err := Write(path, []byte("hello"), OwnerOnly); err == nil {
		t.Fatal("Write into a missing directory should fail")
	}
}

func TestWrite_ReplacesContentsCompletely(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secret.json")

	// A shorter second write must not leave a tail of the longer first one —
	// which is exactly what an in-place O_TRUNC write can do if it is
	// interrupted, and what the rename makes impossible.
	if err := Write(path, []byte("a much longer first value"), OwnerOnly); err != nil {
		t.Fatalf("first Write: %v", err)
	}
	if err := Write(path, []byte("short"), OwnerOnly); err != nil {
		t.Fatalf("second Write: %v", err)
	}

	// #nosec G304 -- path is built from t.TempDir() inside this test.
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != "short" {
		t.Errorf("contents = %q, want %q", got, "short")
	}
}
