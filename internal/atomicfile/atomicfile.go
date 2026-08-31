// Package atomicfile writes files atomically with owner-only permissions.
//
// calendar-bridge persists two kinds of sensitive local state: the OAuth token
// files under secrets/, and config.yaml (which may carry the web UI auth token
// and the webhook verification token). Both must survive a crash, a full disk,
// or a killed container without ever being left truncated, and neither may be
// readable beyond its owner.
package atomicfile

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// OwnerOnly is the permission mode for files holding credentials or secrets.
const OwnerOnly fs.FileMode = 0o600

// Write writes data to path atomically: it creates a temporary file in the same
// directory, chmods it to perm, writes and fsyncs it, renames it over path, and
// then fsyncs the parent directory so the rename itself is durable. A crash at
// any point leaves either the previous contents or the new ones, never a
// partial file.
//
// Errors are PATH-FREE: they name only the base of the destination, and the
// wrapped OS cause is stripped of its embedded paths (see pathFree). These
// writes carry credentials, and their failures reach the daemon's stderr, which
// under systemd is the journal and under Docker is `docker logs`. The bare
// cause is what makes a failure actionable; the path only discloses where the
// secrets live.
//
// The temp file is created in path's own directory so the rename is within one
// filesystem (os.Rename cannot atomically cross filesystems). It is removed on
// any failure before the rename.
func Write(path string, data []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", filepath.Base(dir), pathFree(err))
	}
	tmpName := tmp.Name()
	// Best-effort cleanup if we bail before the rename succeeds. Removing a
	// path that Rename already consumed is a harmless no-op.
	defer func() { _ = os.Remove(tmpName) }()

	// CreateTemp makes the file 0600 already, but be explicit: perm is the
	// caller's contract and a future umask or platform difference shouldn't
	// silently widen it.
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temp file to %o: %w", perm, pathFree(err))
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing temp file: %w", pathFree(err))
	}
	// fsync before the rename so the new contents are durably on disk first;
	// otherwise a crash right after rename could leave the directory entry
	// pointing at data that never reached the platter.
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing temp file: %w", pathFree(err))
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing temp file: %w", pathFree(err))
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("atomically replacing %s: %w", filepath.Base(path), pathFree(err))
	}
	// The rename is not durable until the PARENT DIRECTORY is synced: the file's
	// own fsync above covers its contents, not the directory entry pointing at
	// it. Reported but not fatal — the data is written and renamed, and failing
	// here would discard a completed write over a durability hint.
	if err := syncDir(dir); err != nil {
		return fmt.Errorf("syncing directory after replacing %s: %w", filepath.Base(path), pathFree(err))
	}
	return nil
}
