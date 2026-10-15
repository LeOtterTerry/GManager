//go:build windows

package backup

import "golang.org/x/sys/windows"

func reparsePoint(path string) (bool, error) {
	full, err := extendedPath(path)
	if err != nil {
		return false, err
	}
	ptr, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return false, err
	}
	attrs, err := windows.GetFileAttributes(ptr)
	if err != nil {
		return false, err
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
