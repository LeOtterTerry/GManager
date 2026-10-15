package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// atomicWrite replaces path with data.
// When keepBak is set and path already exists, the previous file is kept as
// a timestamped .bak beside it. The write itself is temp file plus rename.
// Windows cannot replace an existing file with os.Rename, so the previous
// file is moved aside first on both platforms.
func atomicWrite(path string, data []byte, keepBak bool) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := true
	defer func() {
		if cleanup {
			if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return
			}
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			return fmt.Errorf("%w (close: %v)", err, closeErr)
		}
		return err
	}
	if err := tmp.Sync(); err != nil {
		if closeErr := tmp.Close(); closeErr != nil {
			return fmt.Errorf("%w (close: %v)", err, closeErr)
		}
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	_, statErr := os.Stat(path)
	switch {
	case statErr == nil && keepBak:
		bak, err := backupName(path)
		if err != nil {
			return err
		}
		if err := os.Rename(path, bak); err != nil {
			return err
		}
		if err := os.Rename(tmpName, path); err != nil {
			if restoreErr := os.Rename(bak, path); restoreErr != nil {
				return fmt.Errorf("replace %s: %w (restore backup: %v)", path, err, restoreErr)
			}
			return err
		}
		cleanup = false
		return nil
	case statErr == nil:
		aside := path + ".replacing"
		if err := os.Rename(path, aside); err != nil {
			return err
		}
		if err := os.Rename(tmpName, path); err != nil {
			if restoreErr := os.Rename(aside, path); restoreErr != nil {
				return fmt.Errorf("replace %s: %w (restore: %v)", path, err, restoreErr)
			}
			return err
		}
		if removeErr := os.Remove(aside); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return removeErr
		}
		cleanup = false
		return nil
	case errors.Is(statErr, os.ErrNotExist):
		if err := os.Rename(tmpName, path); err != nil {
			return err
		}
		cleanup = false
		return nil
	default:
		return statErr
	}
}

func backupName(path string) (string, error) {
	stamp := time.Now().UTC().Format("20060102T150405.000000000Z")
	for i := 0; i < 1000; i++ {
		name := fmt.Sprintf("%s.%s.%03d.bak", path, stamp, i)
		_, err := os.Stat(name)
		if errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a backup filename")
}
