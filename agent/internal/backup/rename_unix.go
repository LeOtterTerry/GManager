//go:build !windows

package backup

import "os"

func renameOnce(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}

func isSharingViolation(error) bool { return false }

func isAccessDenied(error) bool { return false }

func isLockViolation(error) bool { return false }
