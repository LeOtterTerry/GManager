//go:build windows

package backup

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func renameOnce(oldpath, newpath string) error {
	from, err := extendedPath(oldpath)
	if err != nil {
		return err
	}
	to, err := extendedPath(newpath)
	if err != nil {
		return err
	}
	src, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	dst, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(src, dst, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

func retryableRename(err error) bool {
	return errors.Is(err, windows.ERROR_SHARING_VIOLATION) || errors.Is(err, windows.ERROR_ACCESS_DENIED)
}

// extendedPath opts into the long-path form so a rename past MAX_PATH works
// without a manifest change. Both sides of MoveFileEx must use it.
func extendedPath(path string) (string, error) {
	if strings.HasPrefix(path, `\\?\`) {
		return path, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if strings.HasPrefix(abs, `\\?\`) {
		return abs, nil
	}
	if strings.HasPrefix(abs, `\\`) {
		return `\\?\UNC\` + strings.TrimPrefix(abs, `\\`), nil
	}
	return `\\?\` + abs, nil
}
