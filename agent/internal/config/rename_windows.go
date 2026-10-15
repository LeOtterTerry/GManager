//go:build windows

package config

import "golang.org/x/sys/windows"

func init() {
	renameFile = moveFileReplace
}

// moveFileReplace is the Windows replace. MOVEFILE_WRITE_THROUGH asks the
// filesystem to flush the file data and metadata before the call returns,
// which is the durability guarantee a directory fsync would provide elsewhere.
func moveFileReplace(oldpath, newpath string) error {
	from, err := windows.UTF16PtrFromString(oldpath)
	if err != nil {
		return err
	}
	to, err := windows.UTF16PtrFromString(newpath)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(from, to, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}
