package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxBackups is how many timestamped copies atomicWrite keeps beside a file.
const maxBackups = 20

// renameFile replaces newpath with oldpath in one operation.
// On Windows, moveFileReplace uses MoveFileEx with MOVEFILE_REPLACE_EXISTING
// and MOVEFILE_WRITE_THROUGH, so the replacement does not move the live file
// aside first and the directory fsync below is not required for durability.
var renameFile = os.Rename

// atomicWrite replaces path with data.
// When keepBak is set and path already exists, the current bytes are hard-linked
// (or copied) to a timestamped .bak first. The live path is then replaced by one
// rename of the temp file, so a crash cannot leave path missing. The directory
// is fsynced where the OS supports it. Leftover temp files are removed.
func atomicWrite(path string, data []byte, keepBak bool) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := cleanupTemps(dir, filepath.Base(path)); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
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
		if err := linkOrCopy(path, bak); err != nil {
			return err
		}
		if err := pruneBackups(path, maxBackups); err != nil {
			return err
		}
	case statErr == nil || errors.Is(statErr, os.ErrNotExist):
	default:
		return statErr
	}
	if err := renameFile(tmpName, path); err != nil {
		return err
	}
	cleanup = false
	if err := syncDir(dir); err != nil {
		return err
	}
	return nil
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

func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := in.Close(); closeErr != nil {
			return
		}
	}()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		if closeErr := out.Close(); closeErr != nil {
			return fmt.Errorf("%w (close: %v)", err, closeErr)
		}
		if removeErr := os.Remove(dst); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("%w (remove: %v)", err, removeErr)
		}
		return err
	}
	if err := out.Sync(); err != nil {
		if closeErr := out.Close(); closeErr != nil {
			return fmt.Errorf("%w (close: %v)", err, closeErr)
		}
		return err
	}
	return out.Close()
}

func pruneBackups(path string, keep int) error {
	if keep < 1 {
		return nil
	}
	names, err := listBackups(path)
	if err != nil {
		return err
	}
	if len(names) <= keep {
		return nil
	}
	for _, name := range names[:len(names)-keep] {
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func listBackups(path string) ([]string, error) {
	dir := filepath.Dir(path)
	prefix := filepath.Base(path) + "."
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, prefix) && strings.HasSuffix(name, ".bak") {
			names = append(names, filepath.Join(dir, name))
		}
	}
	sort.Strings(names)
	return names, nil
}

// cleanupTemps removes temp files left behind when a previous write crashed.
func cleanupTemps(dir, base string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	prefix := base + ".tmp-"
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasPrefix(entry.Name(), prefix) {
			continue
		}
		name := filepath.Join(dir, entry.Name())
		if err := os.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}
