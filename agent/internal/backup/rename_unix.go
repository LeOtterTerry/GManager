//go:build !windows

package backup

import "os"

func renameOnce(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func retryableRename(error) bool { return false }
