//go:build windows

package backup

import "golang.org/x/sys/windows"

// replaceFile replaces newpath with oldpath. MOVEFILE_REPLACE_EXISTING is
// required because os.Rename on Windows fails when the destination exists.
func replaceFile(oldpath, newpath string) error {
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
