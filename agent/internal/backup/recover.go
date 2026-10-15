package backup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

const (
	holdPrefix = ".gmanager-hold-"
	undoSuffix = ".gmanager-undo"
)

// Recover finishes or rolls back a restore journal in the directories it is
// given. A pending journal puts every recorded target back from its hold. A
// committed journal deletes only the holds it names. Holds that no journal
// names are reported and left in place. Staging directories are removed only
// when the journal lists them. Parent directories are not scanned.
func Recover(paths ...string) error {
	dirs, err := recoverDirs(paths)
	known := map[string]struct{}{}
	scan := map[string]struct{}{}
	for _, dir := range dirs {
		scan[dir] = struct{}{}
	}
	var journals []string
	for _, dir := range dirs {
		j, rerr := readJournal(dir)
		if errors.Is(rerr, os.ErrNotExist) {
			continue
		}
		if rerr != nil {
			err = errors.Join(err, rerr)
			continue
		}
		noteJournal(j, known, scan)
		journals = append(journals, dir)
	}
	for _, dir := range journals {
		if rerr := rollbackJournal(dir); rerr != nil {
			err = errors.Join(err, rerr)
		}
	}
	for dir := range scan {
		found, scanErr := unjournaledHolds(dir, known)
		if scanErr != nil {
			err = errors.Join(err, scanErr)
			continue
		}
		for _, hold := range found {
			err = errors.Join(err, fmt.Errorf("%w: %s", ErrUnjournaledHold, hold))
		}
	}
	return err
}

func noteJournal(j journalFile, known, scan map[string]struct{}) {
	for _, target := range j.Targets {
		if target.Hold != "" {
			hold := filepath.Clean(target.Hold)
			known[hold] = struct{}{}
			scan[filepath.Dir(hold)] = struct{}{}
		}
		if target.Dest != "" {
			scan[filepath.Dir(filepath.Clean(target.Dest))] = struct{}{}
		}
	}
	for _, path := range j.Staging {
		if path == "" {
			continue
		}
		scan[filepath.Dir(filepath.Clean(path))] = struct{}{}
	}
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

func recoverDirs(paths []string) ([]string, error) {
	seen := map[string]struct{}{}
	var dirs []string
	var err error
	for _, path := range paths {
		dir, ok, convErr := recoverDir(path)
		if convErr != nil {
			err = errors.Join(err, convErr)
			continue
		}
		if !ok {
			continue
		}
		if _, dup := seen[dir]; dup {
			continue
		}
		seen[dir] = struct{}{}
		dirs = append(dirs, dir)
	}
	return dirs, err
}

func recoverDir(path string) (string, bool, error) {
	if path == "" {
		return "", false, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", false, err
	}
	info, err := os.Lstat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return abs, true, nil
	}
	return filepath.Dir(abs), true, nil
}
