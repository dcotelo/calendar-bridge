package atomicfile

import (
	"errors"
	"io/fs"
	"os"
)

// pathFree strips the filesystem paths out of an OS error, keeping the cause.
//
// os.OpenFile, os.Rename and friends return *fs.PathError / *os.LinkError,
// whose Error() embeds the FULL path regardless of how the caller formats the
// wrapping message. These errors travel to the daemon's stderr, which under
// systemd is the journal and under Docker is `docker logs` — shared sinks. The
// bare cause ("permission denied", "file exists") is what makes the message
// actionable; the path is what discloses where the secrets live.
//
// Errors that are not path-carrying are returned unchanged, and the result
// still unwraps to the original sentinel so errors.Is keeps working
// (fs.ErrNotExist, fs.ErrPermission and so on live on the inner error).
func pathFree(err error) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}
