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
// files with the archived bytes. Save bytes are written to inst.UserDir,
// which must already be the directory the operator chose. Verification
// finishes before the first write. A hash mismatch leaves the live files alone.
func (s *Store) Restore(ctx context.Context, id string, inst core.Instance, mod core.GameModule) error {
	return s.RestoreInto(ctx, id, inst, mod, "", nil)
}

// RestoreInto is Restore with an automatic rollback. safetyID is a backup of
// the destination taken before this call and already verified by the caller.
// adapt, when set, may rewrite a verified config file before it is swapped
// into place. A returned error refuses the restore before any swap. Staging
// is cancellable. Once the swaps start, cancellation is ignored. A journal is
// fsynced before the first swap. If a swap fails, previous files are renamed
// back from their holds. safetyID is written back only when that rename fails
// and those holds are already gone.
func (s *Store) RestoreInto(ctx context.Context, id string, inst core.Instance, mod core.GameModule, safetyID string, adapt func(string, []byte) ([]byte, error)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if safetyID != "" && safetyID == id {
		return errors.New("safety backup must be a different archive")
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
	var safety *archive
	if safetyID != "" {
		safety, err = s.openVerified(safetyID)
		if err != nil {
			return err
		}
		defer func() {
			if closeErr := safety.Close(); closeErr != nil {
				return
			}
		}()
	}
	if err := s.Recover(inst, mod); err != nil {
		return err
	}
	if err := s.blockStray(inst, mod); err != nil {
		return err
	}
	plan, err := prepare(ctx, a, inst, mod, s.Dir, s.ensureSpace)
	if err != nil {
		return err
	}
	staged, err := stageAll(ctx, a, plan, adapt)
	if err != nil {
		if discardErr := staged.discard(); discardErr != nil {
			return errors.Join(err, discardErr)
		}
		return err
	}
	if err := ctx.Err(); err != nil {
		if discardErr := staged.discard(); discardErr != nil {
			return errors.Join(err, discardErr)
		}
		return err
	}
	// Swaps are the point of no return for cancellation. The journal is
	// durable before the first rename. A failure puts the previous names back
	// from their holds. The safety zip is used only when that rename-back
	// fails and the holds are already gone. Holds named by a pending journal
	// stay on disk so the next recovery can roll them back.
	allowed := allowedFromPlan(plan)
	if err := s.commit(staged, allowed, inst.ID); err != nil {
		return s.finishCommit(err, staged, safety, safetyID, plan, allowed, inst.ID)
	}
	return nil
}

func (s *Store) blockStray(inst core.Instance, mod core.GameModule) error {
	holds, err := s.StrayHolds(inst, mod)
	if err != nil {
		return err
	}
	if len(holds) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %s", ErrUnjournaledHold, holds[0])
}

func (s *Store) finishCommit(err error, staged *stagedSet, safety *archive, safetyID string, plan restorePlan, allowed []string, instanceID string) error {
	committed, cerr := s.committedJournal(instanceID)
	if cerr != nil {
		return errors.Join(err, cerr)
	}
	// The journal is removed before the restore id rotates. If that remove
	// already happened, the swaps are done and must not be undone.
	if committed || (staged != nil && staged.dirty && s.journalMissing(instanceID)) {
		return fmt.Errorf("restored; cleanup failed: %w", err)
	}
	undoErr := staged.undo()
	if discardErr := staged.discard(); discardErr != nil {
		undoErr = errors.Join(undoErr, discardErr)
	}
	if undoErr == nil {
		dir, dirErr := s.journalDirectory()
		if dirErr != nil {
			return errors.Join(err, dirErr)
		}
		if dropErr := finishJournalFile(dir, instanceID, func() error {
			return s.rotateRestoreID(instanceID)
		}); dropErr != nil {
			return errors.Join(err, dropErr)
		}
		return err
	}
	remain, herr := s.holdsRemain(instanceID, allowed)
	if herr != nil {
		return errors.Join(err, undoErr, herr)
	}
	if remain || safety == nil || !staged.dirty {
		return errors.Join(err, undoErr)
	}
	rbErr := s.apply(safety, plan, instanceID)
	if rbErr != nil {
		return fmt.Errorf("%w; undo failed: %v; rollback from safety backup %s failed: %v", err, undoErr, safetyID, rbErr)
	}
	return fmt.Errorf("%w; rolled back from safety backup %s", err, safetyID)
}

type restorePlan struct {
	saves   []string
	configs []configDest
}

type configDest struct {
	rel string
	abs string
}

func prepare(ctx context.Context, a *archive, inst core.Instance, mod core.GameModule, backupDir string, ensure func(string, uint64) error) (restorePlan, error) {
	if err := ctx.Err(); err != nil {
		return restorePlan{}, err
	}
	saves, err := mod.SavePaths(inst)
	if err != nil {
		return restorePlan{}, err
	}
	if len(saves) != len(a.man.Saves) {
		return restorePlan{}, fmt.Errorf("backup has %d save directories, the config now has %d", len(a.man.Saves), len(saves))
	}
	configs := mod.ConfigFiles(inst)
	if err := matchConfigs(a.man, configs); err != nil {
		return restorePlan{}, err
	}
	plan := restorePlan{saves: saves}
	for i, root := range saves {
		info, statErr := os.Lstat(root)
		if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
			return restorePlan{}, statErr
		}
		if statErr == nil && info.Mode()&os.ModeSymlink != 0 {
			return restorePlan{}, fmt.Errorf("save path %s is a symlink", root)
		}
		if statErr == nil && !info.IsDir() {
			return restorePlan{}, fmt.Errorf("save path %s is not a directory", root)
		}
		if err := guardSaveRoot(inst.Dir, backupDir, root); err != nil {
			return restorePlan{}, err
		}
		total, n := bytesFor(a.man, roleSave, i)
		need, err := spaceNeed(total, n)
		if err != nil {
			return restorePlan{}, err
		}
		check := root
		if errors.Is(statErr, os.ErrNotExist) {
			check = filepath.Dir(root)
		}
		if err := ensure(check, need); err != nil {
			return restorePlan{}, err
		}
	}
	for _, cf := range configs {
		rel := filepath.ToSlash(cf.RelPath)
		abs := filepath.Join(inst.Dir, filepath.FromSlash(rel))
		if err := within(inst.Dir, abs); err != nil {
			return restorePlan{}, err
		}
		total, n := configBytes(a.man, rel)
		need, err := spaceNeed(total, n)
		if err != nil {
			return restorePlan{}, err
		}
		if err := ensure(abs, need); err != nil {
			return restorePlan{}, err
		}
		plan.configs = append(plan.configs, configDest{rel: rel, abs: abs})
	}
	return plan, nil
}

type stagedSet struct {
	dirs  []stagedDir
	files []stagedFile
	dirty bool
}

type stagedDir struct {
	dest    string
	tmp     string
	hold    string
	swapped bool
}

type stagedFile struct {
	dest    string
	tmp     string
	hold    string
	swapped bool
}

func stageAll(ctx context.Context, a *archive, plan restorePlan, adapt func(string, []byte) ([]byte, error)) (*stagedSet, error) {
	st := &stagedSet{}
	for i, root := range plan.saves {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		dir, err := stageSave(a, i, root)
		if err != nil {
			return st, err
		}
		st.dirs = append(st.dirs, dir)
	}
	for _, cf := range plan.configs {
		if err := ctx.Err(); err != nil {
			return st, err
		}
		file, err := stageConfig(a, cf, adapt)
		if err != nil {
			return st, err
		}
		st.files = append(st.files, file)
	}
	return st, nil
}

func stageSave(a *archive, index int, dest string) (stagedDir, error) {
	parent := filepath.Dir(dest)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return stagedDir{}, err
	}
	tmp, err := os.MkdirTemp(parent, ".gmanager-restore-*")
	if err != nil {
		return stagedDir{}, err
	}
	for _, f := range a.man.Files {
		if f.Role != roleSave || f.Root != index {
			continue
		}
		target := filepath.Join(tmp, filepath.FromSlash(f.Rel))
		if err := within(tmp, target); err != nil {
			if rmErr := os.RemoveAll(tmp); rmErr != nil {
				return stagedDir{}, errors.Join(err, rmErr)
			}
			return stagedDir{}, err
		}
		if err := extractChecked(a, f, target); err != nil {
			if rmErr := os.RemoveAll(tmp); rmErr != nil {
				return stagedDir{}, errors.Join(err, rmErr)
			}
			return stagedDir{}, err
		}
	}
	return stagedDir{dest: dest, tmp: tmp}, nil
}

func stageConfig(a *archive, dest configDest, adapt func(string, []byte) ([]byte, error)) (stagedFile, error) {
	var file ManifestFile
	found := false
	for _, f := range a.man.Files {
		if f.Role == roleConfig && f.Rel == dest.rel {
			file = f
			found = true
			break
		}
	}
	if !found {
		return stagedFile{}, fmt.Errorf("backup is missing config %s", dest.rel)
	}
	parent := filepath.Dir(dest.abs)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return stagedFile{}, err
	}
	tmp, err := os.CreateTemp(parent, ".gmanager-restore-*.tmp")
	if err != nil {
		return stagedFile{}, err
	}
	tmpName := tmp.Name()
	if err := tmp.Close(); err != nil {
		if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return stagedFile{}, errors.Join(err, removeErr)
		}
		return stagedFile{}, err
	}
	if err := os.Remove(tmpName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return stagedFile{}, err
	}
	if err := extractChecked(a, file, tmpName); err != nil {
		return stagedFile{}, err
	}
	if adapt != nil {
		body, err := os.ReadFile(tmpName)
		if err != nil {
			if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return stagedFile{}, errors.Join(err, removeErr)
			}
			return stagedFile{}, err
		}
		updated, err := adapt(dest.rel, body)
		if err != nil {
			if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				return stagedFile{}, errors.Join(err, removeErr)
			}
			return stagedFile{}, err
		}
		if !bytes.Equal(body, updated) {
			if err := rewriteFile(tmpName, updated); err != nil {
				if removeErr := os.Remove(tmpName); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return stagedFile{}, errors.Join(err, removeErr)
				}
				return stagedFile{}, err
			}
		}
	}
	return stagedFile{dest: dest.abs, tmp: tmpName}, nil
}

func rewriteFile(path string, body []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		return err
	}
	_, werr := f.Write(body)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	if serr != nil {
		return serr
	}
	return cerr
}

func (s *Store) commit(st *stagedSet, allowed []string, instanceID string) error {
	dir, err := s.journalDirectory()
	if err != nil {
		return err
	}
	key, err := s.integrityKey()
	if err != nil {
		return err
	}
	restoreID, err := newRestoreID()
	if err != nil {
		return err
	}
	if err := s.armRestore(instanceID, restoreID); err != nil {
		return err
	}
	if err := assignHolds(st); err != nil {
		return err
	}
	pending := journalFrom(st, journalPending, instanceID, restoreID)
	if err := validateJournal(pending, allowed); err != nil {
		return err
	}
	if err := writeJournal(dir, instanceID, pending, key); err != nil {
		return err
	}
	if err := s.swapAll(st, dir, instanceID, key); err != nil {
		return err
	}
	return commitJournal(dir, instanceID, st, key, allowed, restoreID, func() error {
		return s.rotateRestoreID(instanceID)
	})
}

func (s *Store) swapAll(st *stagedSet, dir, instanceID string, key []byte) error {
	for i := range st.dirs {
		if err := s.beforeSwap(); err != nil {
			return err
		}
		hold, err := swapPath(st.dirs[i].dest, st.dirs[i].tmp, st.dirs[i].hold, true)
		if hold != "" {
			st.dirs[i].hold = hold
		}
		if err != nil {
			if hold != "" {
				st.dirty = true
			}
			return err
		}
		st.dirs[i].swapped = true
		st.dirty = true
		if err := markSwapped(dir, instanceID, key, st.dirs[i].dest); err != nil {
			return err
		}
	}
	for i := range st.files {
		if err := s.beforeSwap(); err != nil {
			return err
		}
		hold, err := swapPath(st.files[i].dest, st.files[i].tmp, st.files[i].hold, false)
		if hold != "" {
			st.files[i].hold = hold
		}
		if err != nil {
			if hold != "" {
				st.dirty = true
			}
			return err
		}
		st.files[i].swapped = true
		st.dirty = true
		if err := markSwapped(dir, instanceID, key, st.files[i].dest); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) beforeSwap() error {
	if s.swapFail == nil {
		return nil
	}
	return s.swapFail()
}

func (s *Store) apply(a *archive, plan restorePlan, instanceID string) error {
	prev := s.swapFail
	s.swapFail = nil
	defer func() { s.swapFail = prev }()
	staged, err := stageAll(context.Background(), a, plan, nil)
	if err != nil {
		if discardErr := staged.discard(); discardErr != nil {
			return errors.Join(err, discardErr)
		}
		return err
	}
	if err := s.commit(staged, allowedFromPlan(plan), instanceID); err != nil {
		return s.finishCommit(err, staged, nil, "", plan, allowedFromPlan(plan), instanceID)
	}
	return nil
}

func (st *stagedSet) discard() error {
	if st == nil {
		return nil
	}
	var err error
	for i := range st.dirs {
		if st.dirs[i].swapped || st.dirs[i].tmp == "" {
			continue
		}
		if rmErr := os.RemoveAll(st.dirs[i].tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}
	for i := range st.files {
		if st.files[i].swapped || st.files[i].tmp == "" {
			continue
		}
		if rmErr := os.Remove(st.files[i].tmp); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
			err = errors.Join(err, rmErr)
		}
	}
	return err
}

func (st *stagedSet) undo() error {
	if st == nil {
		return nil
	}
	var err error
	for i := len(st.files) - 1; i >= 0; i-- {
		if undoErr := undoSwapped(st.files[i].dest, st.files[i].hold, st.files[i].swapped); undoErr != nil {
			err = errors.Join(err, undoErr)
		}
	}
	for i := len(st.dirs) - 1; i >= 0; i-- {
		if undoErr := undoSwapped(st.dirs[i].dest, st.dirs[i].hold, st.dirs[i].swapped); undoErr != nil {
			err = errors.Join(err, undoErr)
		}
	}
	return err
}

func undoSwapped(dest, hold string, swapped bool) error {
	if hold != "" {
		_, err := os.Lstat(hold)
		if err == nil {
			return putBack(dest, hold)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if swapped {
			return fmt.Errorf("previous copy of %s is missing", dest)
		}
		return nil
	}
	if !swapped {
		return nil
	}
	if err := os.RemoveAll(dest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func putBack(dest, hold string) error {
	junk := dest + undoSuffix
	if err := os.RemoveAll(junk); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		if err := renameRetry(dest, junk); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := renameRetry(hold, dest); err != nil {
		return err
	}
	if err := os.RemoveAll(junk); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(filepath.Dir(dest))
}

func swapPath(dest, src, hold string, dir bool) (string, error) {
	info, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		if mkErr := os.MkdirAll(filepath.Dir(dest), 0o755); mkErr != nil {
			return "", mkErr
		}
		if err := renameRetry(src, dest); err != nil {
			return "", err
		}
		if err := syncDir(filepath.Dir(dest)); err != nil {
			return "", err
		}
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if dir && !info.IsDir() {
		return "", fmt.Errorf("save path %s is not a directory", dest)
	}
	if !dir && info.IsDir() {
		return "", fmt.Errorf("config path %s is a directory", dest)
	}
	if hold == "" {
		hold, err = allocateHold(dest)
		if err != nil {
			return "", err
		}
	}
	if err := renameRetry(dest, hold); err != nil {
		return "", err
	}
	if err := renameRetry(src, dest); err != nil {
		if backErr := renameRetry(hold, dest); backErr != nil {
			return hold, fmt.Errorf("move restored data into place: %v; putting the previous data back failed: %w", err, backErr)
		}
		return "", err
	}
	if err := syncDir(filepath.Dir(dest)); err != nil {
		return hold, err
	}
	return hold, nil
}

func holdPath(dest string) string {
	return filepath.Join(filepath.Dir(dest), holdPrefix+filepath.Base(dest))
}

// allocateHold picks a hold name that is not already on disk. An existing
// hold is a previous copy and is not deleted here.
func allocateHold(dest string) (string, error) {
	base := holdPath(dest)
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s.%d", base, i+1)
		}
		_, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			return name, nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", errors.New("could not allocate a hold name")
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
