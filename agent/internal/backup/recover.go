package backup

import (
	"errors"
	"fmt"
	"log"
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
// is reported and left untouched. A committed journal with a valid HMAC, a
// passing allowlist, a stale restore id, and no holds left is finished: it
// is logged and removed. Holds that no verified journal names are not
// deleted here.
func (s *Store) Recover(inst core.Instance, mod core.GameModule) error {
	allowed, err := allowedDests(inst, mod)
	if err != nil {
		if s.journalMissing(inst.ID) {
			return nil
		}
		return err
	}
	dir, err := s.journalDirectory()
	if err != nil {
		if s.journalMissing(inst.ID) {
			return nil
		}
		return err
	}
	key, err := s.readIntegrityKey()
	if err != nil {
		if s.journalMissing(inst.ID) {
			return nil
		}
		return err
	}
	if s.journalMissing(inst.ID) {
		return nil
	}
	j, err := readJournal(dir, inst.ID, key)
	if err != nil {
		return err
	}
	expected, expErr := s.expectedRestore(inst.ID)
	if expErr != nil && !errors.Is(expErr, ErrJournal) {
		return expErr
	}
	if expErr == nil && journalIDMatches(j, expected) {
		return rollbackJournal(dir, inst.ID, key, allowed, expected, func() error {
			return s.rotateRestoreID(inst.ID)
		})
	}
	if err := validateJournal(j, allowed); err != nil {
		return err
	}
	if journalFinished(j) {
		log.Printf("warning: restore journal for %s is a finished restore with a stale id; removing it", inst.ID)
		return removeJournal(dir, inst.ID)
	}
	if expErr != nil {
		return expErr
	}
	return fmt.Errorf("%w: restore id does not match", ErrJournal)
}

// JournalTargetView is one target in a journal an operator is inspecting.
type JournalTargetView struct {
	Dest        string `json:"dest"`
	Hold        string `json:"hold,omitempty"`
	Dir         bool   `json:"dir"`
	Swapped     bool   `json:"swapped"`
	HoldPresent bool   `json:"holdPresent"`
}

// JournalView is the journal an operator sees before rolling it back or
// discarding it. Targets are present only when the HMAC verifies.
type JournalView struct {
	Present     bool                `json:"present"`
	AuthOK      bool                `json:"authOK"`
	AllowlistOK bool                `json:"allowlistOK"`
	IDMatches   bool                `json:"idMatches"`
	Instance    string              `json:"instance,omitempty"`
	State       string              `json:"state,omitempty"`
	RestoreID   string              `json:"restoreID,omitempty"`
	ExpectedID  string              `json:"expectedRestoreID,omitempty"`
	Targets     []JournalTargetView `json:"targets,omitempty"`
	Staging     []string            `json:"staging,omitempty"`
}

// PreviewJournal reads the instance journal and does not change files.
// A missing journal is ErrNotFound. A file that fails HMAC or instance
// checks is present with AuthOK false and no targets.
func (s *Store) PreviewJournal(inst core.Instance, mod core.GameModule) (JournalView, error) {
	if s == nil {
		return JournalView{}, errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.journalDirectory()
	if err != nil {
		return JournalView{}, err
	}
	if s.journalMissing(inst.ID) {
		return JournalView{}, ErrNotFound
	}
	key, err := s.readIntegrityKey()
	if err != nil {
		return JournalView{}, err
	}
	j, err := readJournal(dir, inst.ID, key)
	if errors.Is(err, ErrJournalAuth) || errors.Is(err, ErrJournal) {
		return JournalView{Present: true, AuthOK: false}, nil
	}
	if err != nil {
		return JournalView{}, err
	}
	view := JournalView{
		Present:   true,
		AuthOK:    true,
		Instance:  j.Instance,
		State:     j.State,
		RestoreID: j.RestoreID,
		Staging:   append([]string(nil), j.Staging...),
	}
	if expected, expErr := s.expectedRestore(inst.ID); expErr == nil {
		view.ExpectedID = expected
		view.IDMatches = journalIDMatches(j, expected)
	}
	if allowed, aerr := allowedDests(inst, mod); aerr == nil && validateJournal(j, allowed) == nil {
		view.AllowlistOK = true
	}
	for _, target := range j.Targets {
		item := JournalTargetView{
			Dest:    target.Dest,
			Hold:    target.Hold,
			Dir:     target.Dir,
			Swapped: target.Swapped,
		}
		if target.Hold != "" {
			if _, lerr := os.Lstat(target.Hold); lerr == nil {
				item.HoldPresent = true
			}
		}
		view.Targets = append(view.Targets, item)
	}
	return view, nil
}

// RollbackJournal applies the instance journal when its HMAC and allowlist
// pass, including when the restore id is stale. The journal file is removed
// before the id is rotated.
func (s *Store) RollbackJournal(inst core.Instance, mod core.GameModule) error {
	if s == nil {
		return errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	allowed, err := allowedDests(inst, mod)
	if err != nil {
		return err
	}
	dir, err := s.journalDirectory()
	if err != nil {
		return err
	}
	key, err := s.readIntegrityKey()
	if err != nil {
		return err
	}
	j, err := readJournal(dir, inst.ID, key)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if err := validateJournal(j, allowed); err != nil {
		return err
	}
	return rollbackJournal(dir, inst.ID, key, allowed, j.RestoreID, func() error {
		return s.rotateRestoreID(inst.ID)
	})
}

// DiscardJournal removes the instance journal file and then rotates that
// instance's restore id. It does not rename saves, configs, or holds.
func (s *Store) DiscardJournal(inst core.Instance) error {
	if s == nil {
		return errors.New("backup directory is not configured")
	}
	if err := validInstanceID(inst.ID); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, err := s.journalDirectory()
	if err != nil {
		return err
	}
	if s.journalMissing(inst.ID) {
		return ErrNotFound
	}
	return finishJournalFile(dir, inst.ID, func() error {
		return s.rotateRestoreID(inst.ID)
	})
}

// StrayHolds lists hold copies beside a save or config path that the
// verified journal does not name. It does not delete them.
func (s *Store) StrayHolds(inst core.Instance, mod core.GameModule) ([]string, error) {
	allowed, err := allowedDests(inst, mod)
	if err != nil {
		return nil, err
	}
	known := map[string]struct{}{}
	if !s.journalMissing(inst.ID) {
		dir, err := s.journalDirectory()
		if err != nil {
			return nil, err
		}
		key, err := s.readIntegrityKey()
		if err != nil {
			return nil, err
		}
		j, err := readJournal(dir, inst.ID, key)
		if err != nil {
			return nil, err
		}
		expected, err := s.expectedRestore(inst.ID)
		if err != nil {
			return nil, err
		}
		if err := acceptJournal(j, inst.ID, expected); err != nil {
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
	if !s.journalMissing(inst.ID) {
		dir, err := s.journalDirectory()
		if err != nil {
			return err
		}
		key, err := s.readIntegrityKey()
		if err != nil {
			return err
		}
		j, err := readJournal(dir, inst.ID, key)
		if err != nil {
			return err
		}
		expected, err := s.expectedRestore(inst.ID)
		if err != nil {
			return err
		}
		if err := acceptJournal(j, inst.ID, expected); err != nil {
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
	if err := removeAllNoFollow(hold); err != nil && !errors.Is(err, os.ErrNotExist) {
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

func (s *Store) journalMissing(instanceID string) bool {
	dir, err := s.journalDirectory()
	if err != nil {
		return true
	}
	path, err := journalPath(dir, instanceID)
	if err != nil {
		return true
	}
	_, err = os.Lstat(path)
	return errors.Is(err, os.ErrNotExist)
}

func (s *Store) journalKey() ([]byte, error) {
	return s.readIntegrityKey()
}

func (s *Store) committedJournal(instanceID string) (bool, error) {
	if s.journalMissing(instanceID) {
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
	expected, err := s.expectedRestore(instanceID)
	if err != nil {
		return false, err
	}
	return journalIsCommitted(dir, instanceID, key, expected)
}

func (s *Store) holdsRemain(instanceID string, allowed []string) (bool, error) {
	if s.journalMissing(instanceID) {
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
	expected, err := s.expectedRestore(instanceID)
	if err != nil {
		return false, err
	}
	return journalHoldsExist(dir, instanceID, key, allowed, expected)
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
