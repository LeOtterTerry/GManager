package backup

import (
	"crypto/hmac"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

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
// pass, including when the restore id is stale. A stale id has to be the
// restore id echoed from the preview. The journal file is removed before
// the id is rotated.
func (s *Store) RollbackJournal(inst core.Instance, mod core.GameModule, echoed string) error {
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
	expected, expErr := s.expectedRestore(inst.ID)
	matches := expErr == nil && journalIDMatches(j, expected)
	if !matches {
		if err := acknowledgeRestoreID(echoed, j.RestoreID); err != nil {
			return err
		}
	} else if echoed != "" && !hmac.Equal([]byte(echoed), []byte(j.RestoreID)) {
		return ErrRestoreIDAck
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
// A journal that still records a swapped target, or that has a hold on
// disk, is left in place with ErrRollbackRequired unless force is set and
// restoreID is the id from the preview. A present hold counts as swapped,
// because the destination was already renamed. An unusable journal, a
// finished journal, or an idle journal (no hold present, destinations
// still exist) may be removed without that override.
func (s *Store) DiscardJournal(inst core.Instance, mod core.GameModule, force bool, restoreID string) error {
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
	key, err := s.readIntegrityKey()
	if err != nil {
		return err
	}
	j, err := readJournal(dir, inst.ID, key)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		if !journalUnusable(err) {
			return err
		}
		return s.discardUnverified(dir, inst.ID, err)
	}
	allowed, aerr := allowedDests(inst, mod)
	if aerr != nil {
		return aerr
	}
	if verr := validateJournal(j, allowed); verr != nil {
		return s.discardAllowlist(dir, inst.ID, verr)
	}
	if journalFinished(j) || journalIdle(j) {
		return s.dropJournal(dir, inst.ID)
	}
	if force && restoreID != "" && hmac.Equal([]byte(restoreID), []byte(j.RestoreID)) {
		path, perr := journalPath(dir, inst.ID)
		if perr != nil {
			return perr
		}
		log.Printf("WARNING: force-discarding restore journal %s restore id %s state %s", path, j.RestoreID, j.State)
		return s.dropJournal(dir, inst.ID)
	}
	if force && restoreID != "" {
		return ErrRestoreIDAck
	}
	return ErrRollbackRequired
}

func (s *Store) discardUnverified(dir, instanceID string, reason error) error {
	path, err := journalPath(dir, instanceID)
	if err != nil {
		return err
	}
	log.Printf("WARNING: discarding unverified restore journal %s: %v", path, reason)
	return s.dropJournal(dir, instanceID)
}

func (s *Store) discardAllowlist(dir, instanceID string, reason error) error {
	path, err := journalPath(dir, instanceID)
	if err != nil {
		return err
	}
	log.Printf("WARNING: discarding restore journal %s that failed the allowlist: %v", path, reason)
	return s.dropJournal(dir, instanceID)
}

func (s *Store) dropJournal(dir, instanceID string) error {
	return finishJournalFile(dir, instanceID, func() error {
		return s.rotateRestoreID(instanceID)
	})
}

func acknowledgeRestoreID(echoed, actual string) error {
	if echoed == "" {
		return ErrRestoreIDRequired
	}
	if !hmac.Equal([]byte(echoed), []byte(actual)) {
		return ErrRestoreIDAck
	}
	return nil
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

// StagingView lists unjournaled restore staging directories. Paths are
// present only. Nothing is deleted.
type StagingView struct {
	Paths []string `json:"paths"`
}

// StrayStaging lists `.gmanager-restore-*` entries beside a save or config
// path that the verified journal does not name. It does not delete them.
func (s *Store) StrayStaging(inst core.Instance, mod core.GameModule) ([]string, error) {
	if s == nil {
		return nil, errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.strayStaging(inst, mod)
}

// PreviewStaging is the operator view of StrayStaging.
func (s *Store) PreviewStaging(inst core.Instance, mod core.GameModule) (StagingView, error) {
	paths, err := s.StrayStaging(inst, mod)
	if err != nil {
		return StagingView{}, err
	}
	if paths == nil {
		paths = []string{}
	}
	return StagingView{Paths: paths}, nil
}

// DiscardStaging removes unjournaled `.gmanager-restore-*` leftovers beside
// a save or config path. A directory named by the verified journal is left
// in place. Each delete goes through the no-follow parent handle.
func (s *Store) DiscardStaging(inst core.Instance, mod core.GameModule) ([]string, error) {
	if s == nil {
		return nil, errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	paths, err := s.strayStaging(inst, mod)
	if err != nil {
		return nil, err
	}
	var errs error
	var removed []string
	for _, path := range paths {
		if err := removeAllNoFollow(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = errors.Join(errs, err)
			continue
		}
		removed = append(removed, path)
	}
	if removed == nil {
		removed = []string{}
	}
	return removed, errs
}

func (s *Store) strayStaging(inst core.Instance, mod core.GameModule) ([]string, error) {
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
		for _, path := range j.Staging {
			known[filepath.Clean(path)] = struct{}{}
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
		found, err := unjournaledStaging(parent, known)
		if err != nil {
			return nil, err
		}
		out = append(out, found...)
	}
	return out, nil
}

func unjournaledStaging(dir string, known map[string]struct{}) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, entry := range entries {
		name := entry.Name()
		if !stagingNameOK(name) {
			continue
		}
		full := filepath.Clean(filepath.Join(dir, name))
		if holdKnown(full, known) {
			continue
		}
		out = append(out, full)
	}
	return out, nil
}

func stagingNameOK(name string) bool {
	return strings.HasPrefix(name, restorePrefix) && len(name) > len(restorePrefix)
}

// HoldView is the size and modification time an operator must send back
// before a hold can be deleted.
type HoldView struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	ModTime string `json:"modTime"`
}

// PreviewHold reads one hold beside a save or config path and does not
// delete it. ModTime is UTC RFC3339Nano.
func (s *Store) PreviewHold(inst core.Instance, mod core.GameModule, hold string) (HoldView, error) {
	if s == nil {
		return HoldView{}, errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, _, err := s.inspectHold(inst, mod, hold)
	if err != nil {
		return HoldView{}, err
	}
	return HoldView{
		Path:    hold,
		Size:    info.Size(),
		ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
	}, nil
}

// DiscardHold removes one unjournaled hold beside a save or config path.
// size and modTime must match a fresh stat of that hold. modTime is compared
// with Time.UnixNano, so the preview's UTC RFC3339Nano value has to round-trip
// every nanosecond; a value truncated to seconds does not match. A destination
// that is missing, parked under the undo suffix, or sitting next to a
// staging directory is refused.
func (s *Store) DiscardHold(inst core.Instance, mod core.GameModule, hold string, size int64, modTime time.Time) error {
	if s == nil {
		return errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	info, allowed, err := s.inspectHold(inst, mod, hold)
	if err != nil {
		return err
	}
	if err := s.holdNamedByJournal(inst, allowed, hold); err != nil {
		return err
	}
	if destMidRestore(hold, allowed) {
		return ErrMidRestore
	}
	// UnixNano, not seconds. The preview formats UTC RFC3339Nano so the
	// fraction the operator sends back is the same instant.
	if info.Size() != size || info.ModTime().UnixNano() != modTime.UnixNano() {
		return ErrHoldChanged
	}
	if err := removeAllNoFollow(hold); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (s *Store) inspectHold(inst core.Instance, mod core.GameModule, hold string) (os.FileInfo, []string, error) {
	allowed, err := allowedDests(inst, mod)
	if err != nil {
		return nil, nil, err
	}
	if !holdMatches(hold, allowed) {
		return nil, nil, ErrInvalidHold
	}
	if err := refuseSpecial(hold); err != nil {
		return nil, nil, err
	}
	info, err := os.Lstat(hold)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("%w: %s is a symlink", ErrJournal, hold)
	}
	return info, allowed, nil
}

func (s *Store) holdNamedByJournal(inst core.Instance, allowed []string, hold string) error {
	if s.journalMissing(inst.ID) {
		return nil
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
	return nil
}

// destMidRestore reports a hold whose destination is not a settled save or
// config path. A missing destination means this hold may be the only copy.
func destMidRestore(hold string, allowed []string) bool {
	dest := ""
	for _, candidate := range allowed {
		if holdNameOK(candidate, hold) {
			dest = candidate
			break
		}
	}
	if dest == "" {
		return true
	}
	if _, err := os.Lstat(dest); err != nil {
		return true
	}
	if _, err := os.Lstat(dest + undoSuffix); err == nil {
		return true
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		return true
	}
	for _, ent := range entries {
		name := ent.Name()
		if stagingNameOK(name) {
			return true
		}
	}
	return false
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
