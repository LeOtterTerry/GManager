// Package disk reports free space on the volume that holds a path.
// Backup writes and update rollbacks both refuse to continue when the
// volume cannot hold the bytes they are about to add.
package disk

import (
	"errors"
	"os"
	"path/filepath"
)

// Probe is the platform free-space lookup. Tests replace it.
// The path passed to Probe exists and is a directory.
var Probe = platformFree

// Free reports bytes available to this process on the volume holding path.
// A missing leaf walks up to an existing directory so a new file's parent
// volume is the one that is checked.
func Free(path string) (uint64, error) {
	if Probe == nil {
		return 0, errors.New("free space is unavailable")
	}
	return Probe(existingDir(path))
}

func existingDir(path string) string {
	for {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}
