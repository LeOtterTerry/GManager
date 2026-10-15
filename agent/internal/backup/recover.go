package backup

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

const (
	holdPrefix    = ".gmanager-hold-"
	restorePrefix = ".gmanager-restore-"
	migratePrefix = ".gmanager-migrate-"
	undoSuffix    = ".gmanager-undo"
)

// Recover puts interrupted restores back together. A hold is the previous
// copy of a file or directory. When the destination is missing, the
// lowest-numbered hold is renamed back. When the destination is present, the
// swap committed and leftover holds are removed. Staging directories and
// undo junk are removed once the destination is in place. This runs at
// process start and at the beginning of a restore, not during a rollback.
func Recover(paths ...string) error {
	seen := map[string]struct{}{}
	var err error
	for _, path := range paths {
		if path == "" {
			continue
		}
		abs, absErr := filepath.Abs(path)
		if absErr != nil {
			err = errors.Join(err, absErr)
			continue
		}
		dirs := []string{filepath.Dir(abs)}
		info, statErr := os.Lstat(abs)
		if statErr == nil && info.IsDir() {
			dirs = append([]string{abs}, dirs...)
		} else if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			err = errors.Join(err, statErr)
			continue
		}
		for _, dir := range dirs {
			if _, ok := seen[dir]; ok {
				continue
			}
			seen[dir] = struct{}{}
			if recErr := recoverDir(dir); recErr != nil {
				err = errors.Join(err, recErr)
			}
		}
	}
	return err
}

func recoverInstance(inst core.Instance, mod core.GameModule) error {
	paths := []string{inst.Dir, inst.UserDir}
	if mod != nil {
		if saves, err := mod.SavePaths(inst); err == nil {
			paths = append(paths, saves...)
		}
		for _, cf := range mod.ConfigFiles(inst) {
			paths = append(paths, filepath.Join(inst.Dir, filepath.FromSlash(cf.RelPath)))
		}
	}
	return Recover(paths...)
}

type holdCand struct {
	n    int
	path string
}

func recoverDir(dir string) error {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var staging []string
	holds := map[string][]holdCand{}
	undos := map[string]string{}
	for _, entry := range entries {
		name := entry.Name()
		full := filepath.Join(dir, name)
		if strings.HasPrefix(name, restorePrefix) || strings.HasPrefix(name, migratePrefix) {
			staging = append(staging, full)
			continue
		}
		if strings.HasSuffix(name, undoSuffix) {
			base := strings.TrimSuffix(name, undoSuffix)
			if base != "" {
				undos[base] = full
			}
			continue
		}
		if strings.HasPrefix(name, holdPrefix) {
			base, n := splitHold(strings.TrimPrefix(name, holdPrefix))
			if base == "" {
				continue
			}
			holds[base] = append(holds[base], holdCand{n: n, path: full})
		}
	}
	for _, path := range staging {
		if rmErr := os.RemoveAll(path); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			return rmErr
		}
	}
	bases := map[string]struct{}{}
	for base := range holds {
		bases[base] = struct{}{}
	}
	for base := range undos {
		bases[base] = struct{}{}
	}
	for base := range bases {
		if err := recoverOne(filepath.Join(dir, base), holds[base], undos[base]); err != nil {
			return err
		}
	}
	if len(staging) == 0 && len(bases) == 0 {
		return nil
	}
	return syncDir(dir)
}

func recoverOne(dest string, holds []holdCand, undo string) error {
	sort.Slice(holds, func(i, j int) bool { return holds[i].n < holds[j].n })
	_, err := os.Lstat(dest)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return err
	}
	if missing && len(holds) > 0 {
		if err := renameRetry(holds[0].path, dest); err != nil {
			return err
		}
		holds = holds[1:]
		missing = false
	}
	if missing && undo != "" {
		if err := renameRetry(undo, dest); err != nil {
			return err
		}
		undo = ""
		missing = false
	}
	if missing {
		return nil
	}
	for _, hold := range holds {
		if err := os.RemoveAll(hold.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if undo != "" {
		if err := os.RemoveAll(undo); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func splitHold(rest string) (string, int) {
	i := strings.LastIndex(rest, ".")
	if i <= 0 || i == len(rest)-1 {
		return rest, 0
	}
	num := rest[i+1:]
	for _, c := range num {
		if c < '0' || c > '9' {
			return rest, 0
		}
	}
	n, err := strconv.Atoi(num)
	if err != nil {
		return rest, 0
	}
	return rest[:i], n
}
