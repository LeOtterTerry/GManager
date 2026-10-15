package supervisor

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

func preservedRel(mod core.GameModule, inst core.Instance) map[string]struct{} {
	out := map[string]struct{}{}
	if mod == nil {
		return out
	}
	for _, file := range mod.ConfigFiles(inst) {
		if file.RelPath == "" {
			continue
		}
		out[filepath.ToSlash(file.RelPath)] = struct{}{}
	}
	return out
}

func isPreserved(rel string, preserve map[string]struct{}) bool {
	_, ok := preserve[filepath.ToSlash(rel)]
	return ok
}

func isParentOfPreserved(rel string, preserve map[string]struct{}) bool {
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return true
	}
	prefix := rel + "/"
	for path := range preserve {
		if strings.HasPrefix(path, prefix) || path == rel {
			return true
		}
	}
	return false
}

func snapshotTree(src string, preserve map[string]struct{}) (string, error) {
	dest, err := os.MkdirTemp("", "gmanager-snap-")
	if err != nil {
		return "", err
	}
	if err := copyTree(src, dest, preserve, true); err != nil {
		if removeErr := os.RemoveAll(dest); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", errors.Join(err, removeErr)
		}
		return "", err
	}
	return dest, nil
}

func restoreTree(dest, snap string, preserve map[string]struct{}) error {
	if err := deleteExcept(dest, preserve); err != nil {
		return err
	}
	return copyTree(snap, dest, preserve, true)
}

func copyTree(src, dest string, preserve map[string]struct{}, skipPreserve bool) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("snapshot source is not a directory")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if skipPreserve && isPreserved(rel, preserve) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(path, target, info.Mode())
	})
}

func deleteExcept(root string, preserve map[string]struct{}) error {
	var doomed []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if isPreserved(rel, preserve) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			if isParentOfPreserved(rel, preserve) {
				return nil
			}
			doomed = append(doomed, path)
			return filepath.SkipDir
		}
		doomed = append(doomed, path)
		return nil
	})
	if err != nil {
		return err
	}
	for _, path := range doomed {
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := in.Close(); closeErr != nil {
			return
		}
	}()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		if closeErr := out.Close(); closeErr != nil {
			return errors.Join(err, closeErr)
		}
		return err
	}
	return out.Close()
}
