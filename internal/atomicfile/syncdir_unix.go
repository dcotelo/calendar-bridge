//go:build !windows

package atomicfile

import "os"

// syncDir fsyncs a directory so a rename into it is durably committed.
//
// tmp.Sync() flushes the new file's CONTENTS, but os.Rename changes the parent
// directory's entry, and that metadata change is not covered by the file's own
// fsync. Without this, a power loss immediately after Write returns can leave
// the rename uncommitted and the directory still pointing at the old inode —
// which would defeat the entire purpose of writing atomically.
func syncDir(dir string) error {
	// #nosec G304 -- dir is filepath.Dir of the caller-supplied destination,
	// which the caller has already written to; opening it read-only to fsync
	// adds no reachability that the write itself did not already have.
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	// Sync first, then close, so a Sync failure is the error we report.
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
