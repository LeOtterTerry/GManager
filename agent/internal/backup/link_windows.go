//go:build windows

package backup

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

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

func openNoFollow(path string) (*os.File, error) {
	full, err := extendedPath(path)
	if err != nil {
		return nil, err
	}
	ptr, err := windows.UTF16PtrFromString(full)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(
		ptr,
		windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		cerr := windows.CloseHandle(h)
		return nil, errors.Join(err, cerr)
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		cerr := windows.CloseHandle(h)
		return nil, errors.Join(fmt.Errorf("%w: %s is a reparse point", ErrJournal, path), cerr)
	}
	return os.NewFile(uintptr(h), path), nil
}
