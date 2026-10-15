//go:build !windows

package backup

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func reparsePoint(string) (bool, error) { return false, nil }

func openNoFollow(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			info, lerr := os.Lstat(path)
			if lerr == nil && info.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("%w: %s is a symlink", ErrJournal, path)
			}
		}
		if errors.Is(err, unix.ELOOP) {
			return nil, fmt.Errorf("%w: %s is a symlink", ErrJournal, path)
		}
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

// removeAllNoFollow deletes path through a no-follow open of its parent.
// A symlink parent is refused. A symlink at path is unlinked and not followed.
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
	rmErr := removeAt(int(f.Fd()), name)
	cerr := f.Close()
	return errors.Join(rmErr, cerr)
}

func removeAt(dirfd int, name string) error {
	if !childNameOK(name) {
		return fmt.Errorf("%w: %s", ErrJournal, name)
	}
	var st unix.Stat_t
	err := unix.Fstatat(dirfd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
	if err != nil {
		if errors.Is(err, unix.ENOENT) {
			return nil
		}
		return err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR {
		if err := unix.Unlinkat(dirfd, name, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			return err
		}
		return nil
	}
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) || errors.Is(err, unix.ENOENT) {
			if uerr := unix.Unlinkat(dirfd, name, 0); uerr != nil && !errors.Is(uerr, unix.ENOENT) {
				return errors.Join(fmt.Errorf("%w: %s is a symlink", ErrJournal, name), uerr)
			}
			return nil
		}
		return err
	}
	file := os.NewFile(uintptr(fd), name)
	names, rdErr := file.Readdirnames(-1)
	var errs error
	if rdErr != nil {
		errs = rdErr
	}
	childfd := int(file.Fd())
	for _, child := range names {
		if child == "." || child == ".." {
			continue
		}
		if err := removeAt(childfd, child); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	if cerr := file.Close(); cerr != nil {
		errs = errors.Join(errs, cerr)
	}
	if errs != nil {
		return errs
	}
	if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}
