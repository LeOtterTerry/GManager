//go:build !windows

package backup

import "os"

func replaceFile(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}
