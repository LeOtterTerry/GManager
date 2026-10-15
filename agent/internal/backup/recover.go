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

// Recover applies the restore journal in the agent data directory. A pending
// journal puts every checked target back from its hold. A committed journal
// deletes only the holds it names. A journal that fails HMAC or path checks
// is reported and left untouched. Holds that no verified journal names are
// not deleted here.
func (s *Store) Recover(inst core.Instance, mod core.GameModule) error {
	allowed, err := allowedDests(inst, mod)
	if err != nil {
		if s.journalMissing() {
			return nil
		}
		return err
	}
	dir, err := s.journalDirectory()
	if err != nil {
		if s.journalMissing() {
			return nil
		}
		return err
	}
	key, err := s.readIntegrityKey()
	if err != nil {
		if s.journalMissing() {
			return nil
		}
		return err
	}
	return rollbackJournal(dir, key, allowed)
}

// StrayHolds lists hold copies beside a save or config path that the
// verified journal does not name. It does not delete them.
func (s *Store) StrayHolds(inst core.Instance, mod core.GameModule) ([]string, error) {
	allowed, err := allowedDests(inst, mod)
	if err != nil {
		return nil, err
	}
	known := map[string]struct{}{}
	if !s.journalMissing() {
		dir, err := s.journalDirectory()
		if err != nil {
			return nil, err
		}
		key, err := s.readIntegrityKey()
		if err != nil {
			return nil, err
		}
		j, err := readJournal(dir, key)
		if err != nil {
			return nil, err
		}
		if err := validateJournal(j, allowed); err != nil {
			return nil, err
		}
		for _, target := range j.Targets {
			if target.Hold != "" {
				known[filepath.Clean(target.Hold)] = struct{}{}
			}
		}
	}
	seen := map[string]struct{}{}
	var out []string
	for _, dest := range allowed {
		parent := filepath.Dir(dest)
		if _, ok := seen[parent]; ok {
			continue
		}
		seen[parent] = struct{}{}
		found, err := unjournaledHolds(parent, known)
		if err != nil {
			return nil, err
		}
		for _, hold := range found {
			if !holdNameOK(dest, hold) && !holdMatches(hold, allowed) {
				continue
			}
			out = append(out, hold)
		}
	}
	return out, nil
}

// DiscardHold removes one unjournaled hold beside a save or config path.
func (s *Store) DiscardHold(inst core.Instance, mod core.GameModule, hold string) error {
	if s == nil {
		return errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed, err := allowedDests(inst, mod)
	if err != nil {
		return err
	}
	if !holdMatches(hold, allowed) {
		return ErrInvalidHold
	}
	if err := refuseSpecial(hold); err != nil {
		return err
	}
	info, err := os.Lstat(hold)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrJournal, hold)
	}
	if !s.journalMissing() {
		dir, err := s.journalDirectory()
		if err != nil {
			return err
		}
		key, err := s.readIntegrityKey()
		if err != nil {
			return err
		}
		j, err := readJournal(dir, key)
		if err != nil {
			return err
		}
		if err := validateJournal(j, allowed); err != nil {
			return err
		}
		for _, target := range j.Targets {
			if target.Hold != "" && SamePath(target.Hold, hold) {
				return ErrHoldInUse
			}
		}
	}
	if err := os.RemoveAll(hold); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) journalDirectory() (string, error) {
	if s == nil || s.JournalDir == "" {
		return "", errors.New("restore journal directory is not configured")
	}
	return s.JournalDir, nil
}

func (s *Store) journalMissing() bool {
	dir, err := s.journalDirectory()
	if err != nil {
		return true
	}
	_, err = os.Lstat(journalPath(dir))
	return errors.Is(err, os.ErrNotExist)
}

func (s *Store) journalKey() ([]byte, error) {
	return s.readIntegrityKey()
}

func (s *Store) committedJournal() (bool, error) {
	if s.journalMissing() {
		return false, nil
	}
	dir, err := s.journalDirectory()
	if err != nil {
		return false, err
	}
	key, err := s.journalKey()
	if err != nil {
		return false, err
	}
	return journalIsCommitted(dir, key)
}

func (s *Store) holdsRemain(allowed []string) (bool, error) {
	if s.journalMissing() {
		return false, nil
	}
	dir, err := s.journalDirectory()
	if err != nil {
		return false, err
	}
	key, err := s.journalKey()
	if err != nil {
		return false, err
	}
	return journalHoldsExist(dir, key, allowed)
}

func allowedDests(inst core.Instance, mod core.GameModule) ([]string, error) {
	if mod == nil {
		return nil, errors.New("module is required")
	}
	var out []string
	saves, err := mod.SavePaths(inst)
	if err != nil && !errors.Is(err, core.ErrSavePathUnknown) {
		return nil, err
	}
	out = append(out, saves...)
	for _, cf := range mod.ConfigFiles(inst) {
		out = append(out, filepath.Join(inst.Dir, filepath.FromSlash(cf.RelPath)))
	}
	if len(out) == 0 {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("no save or config paths")
	}
	return out, nil
}

func allowedFromPlan(plan restorePlan) []string {
	out := append([]string{}, plan.saves...)
	for _, cf := range plan.configs {
		out = append(out, cf.abs)
	}
	return out
}

func holdMatches(hold string, allowed []string) bool {
	for _, dest := range allowed {
		if holdNameOK(dest, hold) {
			return true
		}
	}
	return false
}
