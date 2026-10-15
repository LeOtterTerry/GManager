//go:build windows

package backup

import (
	"errors"
	"fmt"
	"os"
	"unsafe"

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
		windows.FILE_LIST_DIRECTORY|windows.FILE_TRAVERSE|fileDeleteChild|windows.DELETE|windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
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

const fileDeleteChild = 0x00000040

// fileIDBothDirInfo matches the layout Go uses for FileIdBothDirectoryInfo.
type fileIDBothDirInfo struct {
	NextEntryOffset uint32
	FileIndex       uint32
	CreationTime    windows.Filetime
	LastAccessTime  windows.Filetime
	LastWriteTime   windows.Filetime
	ChangeTime      windows.Filetime
	EndOfFile       uint64
	AllocationSize  uint64
	FileAttributes  uint32
	FileNameLength  uint32
	EaSize          uint32
	ShortNameLength uint32
	ShortName       [12]uint16
	FileID          uint64
	FileName        [1]uint16
}

type fileDispositionInfo struct {
	DeleteFile byte
}

// removeAllNoFollow deletes path through a no-follow open of its parent.
// A reparse-point parent is refused. A reparse point at path is deleted
// without following it.
func removeAllNoFollow(path string) error {
	parent, name, err := splitFinal(path)
	if err != nil {
		return err
	}
	f, err := openNoFollow(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	rmErr := removeRelative(windows.Handle(f.Fd()), name)
	cerr := f.Close()
	return errors.Join(rmErr, cerr)
}

func removeRelative(root windows.Handle, name string) error {
	if !childNameOK(name) {
		return fmt.Errorf("%w: %s", ErrJournal, name)
	}
	h, err := openRelative(root, name)
	if ntMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rmErr := removeOpened(h)
	cerr := windows.CloseHandle(h)
	return errors.Join(rmErr, cerr)
}

func removeOpened(h windows.Handle) error {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return err
	}
	if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0 && info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0 {
		names, err := listDirNames(h)
		if err != nil {
			return err
		}
		var errs error
		for _, child := range names {
			if err := removeRelative(h, child); err != nil {
				errs = errors.Join(errs, err)
			}
		}
		if errs != nil {
			return errs
		}
	}
	disp := fileDispositionInfo{DeleteFile: 1}
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfo, (*byte)(unsafe.Pointer(&disp)), uint32(unsafe.Sizeof(disp)))
}

func openRelative(root windows.Handle, name string) (windows.Handle, error) {
	objectName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return 0, err
	}
	oa := &windows.OBJECT_ATTRIBUTES{
		RootDirectory: root,
		ObjectName:    objectName,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	oa.Length = uint32(unsafe.Sizeof(*oa))
	var iosb windows.IO_STATUS_BLOCK
	var h windows.Handle
	err = windows.NtCreateFile(
		&h,
		windows.FILE_GENERIC_READ|windows.DELETE|windows.SYNCHRONIZE,
		oa,
		&iosb,
		nil,
		0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_SYNCHRONOUS_IO_NONALERT,
		0,
		0,
	)
	if err != nil {
		return 0, err
	}
	return h, nil
}

func listDirNames(h windows.Handle) ([]string, error) {
	buf := make([]byte, 64*1024)
	class := uint32(windows.FileIdBothDirectoryRestartInfo)
	var probe fileIDBothDirInfo
	minEntry := int(unsafe.Offsetof(probe.FileName))
	var names []string
	for {
		err := windows.GetFileInformationByHandleEx(h, class, &buf[0], uint32(len(buf)))
		if err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
				return names, nil
			}
			return nil, err
		}
		if class == windows.FileIdBothDirectoryRestartInfo {
			class = windows.FileIdBothDirectoryInfo
		}
		offset := 0
		for {
			if offset < 0 || offset+minEntry > len(buf) {
				return nil, fmt.Errorf("%w: directory listing", ErrJournal)
			}
			info := (*fileIDBothDirInfo)(unsafe.Pointer(&buf[offset]))
			next := info.NextEntryOffset
			nlen := int(info.FileNameLength)
			if nlen < 0 || nlen%2 != 0 || offset+minEntry+nlen > len(buf) {
				return nil, fmt.Errorf("%w: directory listing", ErrJournal)
			}
			name := windows.UTF16ToString(unsafe.Slice(&info.FileName[0], nlen/2))
			if name != "." && name != ".." && childNameOK(name) {
				names = append(names, name)
			}
			if next == 0 {
				break
			}
			if next < uint32(minEntry) || offset+int(next) <= offset {
				return nil, fmt.Errorf("%w: directory listing", ErrJournal)
			}
			offset += int(next)
		}
		if len(names) > 100000 {
			return nil, fmt.Errorf("%w: directory listing", ErrJournal)
		}
	}
}

func ntMissing(err error) bool {
	return err != nil && (errors.Is(err, os.ErrNotExist) ||
		errors.Is(err, windows.ERROR_FILE_NOT_FOUND) ||
		errors.Is(err, windows.ERROR_PATH_NOT_FOUND) ||
		errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) ||
		errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND))
}
