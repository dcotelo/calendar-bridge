//go:build windows

package atomicfile

// syncDir is a no-op on Windows.
//
// Windows has no directory handle that can be fsynced the way POSIX allows:
// opening a directory with os.Open and calling Sync returns an error rather
// than flushing metadata. Windows also commits a rename's metadata differently,
// so the POSIX durability gap this closes elsewhere does not apply in the same
// form here.
func syncDir(string) error { return nil }
