package backup

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

// Config reads one archived config file after the manifest hashes match.
// rel is the module path, such as configs/server.json. The bytes are the
// file that will be restored, so a caller can see which save directory that
// config names before it writes anything.
func (s *Store) Config(id, rel string) ([]byte, error) {
	rel = filepath.ToSlash(rel)
	if err := validRel(rel); err != nil {
		return nil, err
	}
	a, err := s.openVerified(id)
	if err != nil {
		return nil, err
	}
	defer func() {
		if closeErr := a.Close(); closeErr != nil {
			return
		}
	}()
	for _, f := range a.man.Files {
		if f.Role == roleConfig && f.Rel == rel {
			return readChecked(a, f)
		}
	}
	return nil, fmt.Errorf("backup is missing config %s", rel)
}

// Restore checks the manifest, then replaces save directories and config
// files with the archived bytes. Verification finishes before the first
// write. A hash mismatch leaves the live files alone.
// inst.UserDir selects where save bytes are written. That should be the
// directory named by the archived config, unless the operator overrode it.
func (s *Store) Restore(ctx context.Context, id string, inst core.Instance, mod core.GameModule) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if mod == nil {
		return errors.New("module is required")
	}
	a, err := s.openVerified(id)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := a.Close(); closeErr != nil {
			return
		}
	}()
	saves, err := mod.SavePaths(inst)
	if err != nil {
		return err
	}
	if len(saves) != len(a.man.Saves) {
		return fmt.Errorf("backup has %d save directories, the config now has %d", len(a.man.Saves), len(saves))
	}
	configs := mod.ConfigFiles(inst)
	if err := matchConfigs(a.man, configs); err != nil {
		return err
	}
	for i, root := range saves {
		info, statErr := os.Stat(root)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return statErr
		}
		if statErr == nil && !info.IsDir() {
			return fmt.Errorf("save path %s is not a directory", root)
		}
		if err := guardSaveRoot(inst.Dir, s.Dir, root); err != nil {
			return err
		}
		total, n := bytesFor(a.man, roleSave, i)
		need, err := spaceNeed(total, n)
		if err != nil {
			return err
		}
		check := root
		if errors.Is(statErr, os.ErrNotExist) {
			check = filepath.Dir(root)
		}
		if err := s.ensureSpace(check, need); err != nil {
			return err
		}
	}
	for _, cf := range configs {
		rel := filepath.ToSlash(cf.RelPath)
		abs := filepath.Join(inst.Dir, filepath.FromSlash(rel))
		if err := within(inst.Dir, abs); err != nil {
			return err
		}
		total, n := configBytes(a.man, rel)
		need, err := spaceNeed(total, n)
		if err != nil {
			return err
		}
		if err := s.ensureSpace(abs, need); err != nil {
			return err
		}
	}
	for i, root := range saves {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := restoreSave(a, i, root); err != nil {
			return err
		}
	}
	for _, cf := range configs {
		if err := ctx.Err(); err != nil {
			return err
		}
		rel := filepath.ToSlash(cf.RelPath)
		abs := filepath.Join(inst.Dir, filepath.FromSlash(rel))
		if err := restoreConfig(a, rel, abs); err != nil {
			return err
		}
	}
	return nil
}

func matchConfigs(man Manifest, files []core.ConfigFile) error {
	want := map[string]struct{}{}
	for _, f := range files {
		rel := filepath.ToSlash(f.RelPath)
		if err := validRel(rel); err != nil {
			return err
		}
		want[rel] = struct{}{}
	}
	got := map[string]struct{}{}
	for _, f := range man.Files {
		if f.Role != roleConfig {
			continue
		}
		got[f.Rel] = struct{}{}
	}
	if len(got) != len(want) {
		return errors.New("backup config files do not match the module")
	}
	for rel := range want {
		if _, ok := got[rel]; !ok {
			return fmt.Errorf("backup is missing config %s", rel)
		}
	}
	return nil
}

func bytesFor(man Manifest, role string, root int) (uint64, int) {
	var total uint64
	n := 0
	for _, f := range man.Files {
		if f.Role != role {
			continue
		}
		if role == roleSave && f.Root != root {
			continue
		}
		total += uint64(f.Size)
		n++
	}
	return total, n
}

func configBytes(man Manifest, rel string) (uint64, int) {
	var total uint64
	n := 0
	for _, f := range man.Files {
		if f.Role == roleConfig && f.Rel == rel {
			total += uint64(f.Size)
			n++
		}
	}
	return total, n
}

func restoreSave(a *archive, index int, dest string) error {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".gmanager-restore-*")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if removeErr := os.RemoveAll(tmp); removeErr != nil {
			return
		}
	}()
	for _, f := range a.man.Files {
		if f.Role != roleSave || f.Root != index {
			continue
		}
		target := filepath.Join(tmp, filepath.FromSlash(f.Rel))
		if err := within(tmp, target); err != nil {
			return err
		}
		if err := extractChecked(a, f, target); err != nil {
			return err
		}
	}
	if err := replaceDir(dest, tmp); err != nil {
		return err
	}
	committed = true
	return nil
}

func restoreConfig(a *archive, rel, dest string) error {
	var file ManifestFile
	found := false
	for _, f := range a.man.Files {
		if f.Role == roleConfig && f.Rel == rel {
			file = f
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("backup is missing config %s", rel)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dest), ".gmanager-restore-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(err, removeErr)
		}
		return err
	}
	if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := extractChecked(a, file, tmpName); err != nil {
		return err
	}
	if err := replaceFile(tmpName, dest); err != nil {
		if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(err, removeErr)
		}
		return err
	}
	return nil
}

func readChecked(a *archive, f ManifestFile) ([]byte, error) {
	zf, ok := a.files[f.Path]
	if !ok {
		return nil, fmt.Errorf("%w: missing %s", ErrIntegrity, f.Path)
	}
	rc, err := zf.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: %s: %v", ErrIntegrity, f.Path, err)
	}
	defer func() {
		if closeErr := rc.Close(); closeErr != nil {
			return
		}
	}()
	var buf bytes.Buffer
	sum, n, err := hashReader(io.TeeReader(rc, &buf))
	if err != nil {
		return nil, err
	}
	if sum != f.SHA256 || n != f.Size {
		return nil, fmt.Errorf("%w: %s", ErrIntegrity, f.Path)
	}
	return buf.Bytes(), nil
}

func extractChecked(a *archive, f ManifestFile, dest string) error {
	zf, ok := a.files[f.Path]
	if !ok {
		return fmt.Errorf("%w: missing %s", ErrIntegrity, f.Path)
	}
	rc, err := zf.Open()
	if err != nil {
		return fmt.Errorf("%w: %s: %v", ErrIntegrity, f.Path, err)
	}
	defer func() {
		if closeErr := rc.Close(); closeErr != nil {
			return
		}
	}()
	sum, n, err := copyToFile(dest, rc)
	if err != nil {
		return err
	}
	if sum != f.SHA256 || n != f.Size {
		if removeErr := os.Remove(dest); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(fmt.Errorf("%w: %s", ErrIntegrity, f.Path), removeErr)
		}
		return fmt.Errorf("%w: %s", ErrIntegrity, f.Path)
	}
	return nil
}

func replaceDir(dest, src string) error {
	if _, err := os.Stat(dest); errors.Is(err, os.ErrNotExist) {
		return os.Rename(src, dest)
	} else if err != nil {
		return err
	}
	hold := filepath.Join(filepath.Dir(dest), ".gmanager-hold-"+filepath.Base(dest))
	if err := os.RemoveAll(hold); err != nil {
		return err
	}
	if err := os.Rename(dest, hold); err != nil {
		return err
	}
	if err := os.Rename(src, dest); err != nil {
		if backErr := os.Rename(hold, dest); backErr != nil {
			return fmt.Errorf("move restored save into place: %v; putting the previous save back failed: %w", err, backErr)
		}
		return err
	}
	if err := os.RemoveAll(hold); err != nil {
		return fmt.Errorf("restored save is in place, removing the previous copy failed: %w", err)
	}
	return nil
}
