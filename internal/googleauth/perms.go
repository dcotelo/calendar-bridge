package googleauth

import (
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
)

// insecureSecretPermMask is the set of permission bits that must NOT be set on
// a credentials or token file: any group or other access (read/write/execute).
// These files are live credentials — only the owner should be able to touch
// them.
const insecureSecretPermMask fs.FileMode = 0o077

// checkSecretPerms reports whether the file at path has group- or
// other-accessible permission bits set. It returns the offending mode and true
// if the file is insecure, or (0, false) if the file is owner-only, missing
// (nothing to warn about — the caller handles absence), or on an OS where
// Unix permission bits aren't meaningful.
//
// It never reads the file contents and never returns an error: a permission
// check must not itself become a failure path for the sync loop.
func checkSecretPerms(path string) (fs.FileMode, bool) {
	// Windows permission bits don't map to the Unix owner/group/other model;
	// skip the check there rather than emit misleading warnings.
	if runtime.GOOS == "windows" {
		return 0, false
	}
	info, err := os.Stat(path)
	if err != nil {
		return 0, false
	}
	mode := info.Mode().Perm()
	if mode&insecureSecretPermMask != 0 {
		return mode, true
	}
	return 0, false
}

// warnIfInsecurePerms logs a warning if the secret file at path is group- or
// world-accessible. kind is a human label ("credentials" / "token") for the
// message. The path is included but the file contents are never read or
// logged.
func warnIfInsecurePerms(logger *slog.Logger, kind, path string) {
	if logger == nil {
		logger = slog.Default()
	}
	if mode, insecure := checkSecretPerms(path); insecure {
		// Log only the base name, not the full path: in shared/aggregated logs
		// the full path would disclose the exact on-disk location of the OAuth
		// credentials/token. The base name plus kind is enough to act on.
		logger.Warn("secret file has insecure permissions; restrict it to owner-only (chmod 600)",
			"kind", kind,
			"file", filepath.Base(path),
			"mode", mode.String(),
			"recommended", "-rw-------",
		)
	}
}
