package backup

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/LeOtterTerry/GManager/agent/internal/owner"
)

const (
	journalName      = ".gmanager-restore-journal"
	journalPending   = "pending"
	journalCommitted = "committed"
	journalVersion   = 1
	restorePrefix    = ".gmanager-restore-"
)

// ErrUnjournaledHold means a hold copy is on disk without a restore journal
// that names it. Recovery reports it and does not delete it.
var ErrUnjournaledHold = errors.New("unjournaled hold")

// ErrJournal means the restore journal is not safe to apply.
var ErrJournal = errors.New("restore journal")

// ErrJournalAuth means the journal HMAC does not match the integrity key.
// Callers report it and do not act on the file.
var ErrJournalAuth = errors.New("restore journal failed verification")

// ErrMissingHold means a journal names a hold that is not on disk.
var ErrMissingHold = errors.New("restore hold is missing")

// ErrInvalidHold means a path is not a hold beside a save or config file.
var ErrInvalidHold = errors.New("not a restore hold")

// ErrHoldInUse means a verified journal still names the hold.
var ErrHoldInUse = errors.New("hold is named by the restore journal")

// ErrRollbackRequired means the journal still describes a restore that has
// to be rolled back. Discarding it would drop the only record of that work.
var ErrRollbackRequired = errors.New("rollback required")

// ErrRestoreIDRequired means a journal whose id does not match was not
// acknowledged with the restore id from the preview.
var ErrRestoreIDRequired = errors.New("restore id acknowledgement is required")

// ErrRestoreIDAck means the restore id sent back does not match the journal.
var ErrRestoreIDAck = errors.New("restore id acknowledgement does not match")

// ErrHoldChanged means the hold's size or modification time does not match
// the preview the operator confirmed.
var ErrHoldChanged = errors.New("hold changed")

// ErrMidRestore means the destination still looks like a restore in progress.
var ErrMidRestore = errors.New("destination looks mid-restore")

// hookCommitCleanup, when set, runs after the committed journal is fsynced
// and before its holds are deleted. Tests use it to fail cleanup.
var hookCommitCleanup func() error

// hookCommitReread, when set, runs after the committed journal is written
// and before it is read back. Tests use it to fail revalidation.
var hookCommitReread func() error

// hookJournalRemove, when set, runs immediately before the journal file is
// removed. Tests use it to crash while the file is still on disk.
var hookJournalRemove func() error

// hookAfterJournalRemove, when set, runs after the journal file is removed
// and before the restore id is rotated. Tests use it to crash in that window.
var hookAfterJournalRemove func() error

// hookMarkSwapped, when set, runs after a target has been swapped and before
// the journal records that progress. Tests use it to fail that write.
var hookMarkSwapped func() error

type journalTarget struct {
	Dest    string `json:"dest"`
	Hold    string `json:"hold,omitempty"`
	Dir     bool   `json:"dir"`
	Swapped bool   `json:"swapped,omitempty"`
}

type journalFile struct {
	Version   int             `json:"version"`
	State     string          `json:"state"`
	Instance  string          `json:"instance"`
	RestoreID string          `json:"restoreID"`
	Targets   []journalTarget `json:"targets"`
	Staging   []string        `json:"staging,omitempty"`
	HMAC      string          `json:"hmac,omitempty"`
}

// BlocksRestore reports journal failures that must not stop the process.
// Startup logs them and refuses restore until the journal is resolved.
func BlocksRestore(err error) bool {
	return errors.Is(err, ErrJournalAuth) || errors.Is(err, ErrJournal) || errors.Is(err, ErrMissingHold)
}

func validInstanceID(id string) error {
	if id == "" || len(id) > 64 {
		return fmt.Errorf("%w: instance id", ErrJournal)
	}
	for i, c := range id {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || (i > 0 && (c == '_' || c == '-'))
		if !ok {
			return fmt.Errorf("%w: instance id", ErrJournal)
		}
	}
	return nil
}

func journalPath(dir, instanceID string) (string, error) {
	if err := validInstanceID(instanceID); err != nil {
		return "", err
	}
	return filepath.Join(dir, journalName+"-"+instanceID), nil
}

func assignHolds(st *stagedSet) error {
	if st == nil {
		return nil
	}
	for i := range st.dirs {
		hold, err := holdFor(st.dirs[i].dest)
		if err != nil {
			return err
		}
		st.dirs[i].hold = hold
	}
	for i := range st.files {
		hold, err := holdFor(st.files[i].dest)
		if err != nil {
			return err
		}
		st.files[i].hold = hold
	}
	return nil
}

func holdFor(dest string) (string, error) {
	info, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%w: %s is a symlink", ErrJournal, dest)
	}
	if reparse, rerr := reparsePoint(dest); rerr != nil {
		return "", rerr
	} else if reparse {
		return "", fmt.Errorf("%w: %s is a reparse point", ErrJournal, dest)
	}
	return allocateHold(dest)
}

func journalFrom(st *stagedSet, state, instance, restoreID string) journalFile {
	j := journalFile{Version: journalVersion, State: state, Instance: instance, RestoreID: restoreID}
	if st == nil {
		return j
	}
	for _, dir := range st.dirs {
		j.Targets = append(j.Targets, journalTarget{Dest: dir.dest, Hold: dir.hold, Dir: true, Swapped: dir.swapped})
		if dir.tmp != "" {
			j.Staging = append(j.Staging, dir.tmp)
		}
	}
	for _, file := range st.files {
		j.Targets = append(j.Targets, journalTarget{Dest: file.dest, Hold: file.hold, Dir: false, Swapped: file.swapped})
		if file.tmp != "" {
			j.Staging = append(j.Staging, file.tmp)
		}
	}
	return j
}

func journalMAC(j journalFile, key []byte) (string, error) {
	if len(key) < 32 {
		return "", errors.New("backup integrity key is not configured")
	}
	j.HMAC = ""
	raw, err := json.Marshal(j)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	if _, err := m.Write(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(m.Sum(nil)), nil
}

func sealJournal(j journalFile, key []byte) (journalFile, error) {
	sum, err := journalMAC(j, key)
	if err != nil {
		return journalFile{}, err
	}
	j.HMAC = sum
	return j, nil
}

func checkJournalMAC(j journalFile, key []byte) error {
	if j.HMAC == "" {
		return ErrJournalAuth
	}
	sum, err := journalMAC(j, key)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(sum), []byte(j.HMAC)) {
		return ErrJournalAuth
	}
	return nil
}

func writeJournal(dir, instanceID string, j journalFile, key []byte) error {
	if dir == "" {
		return errors.New("restore journal directory is not set")
	}
	if err := validInstanceID(instanceID); err != nil {
		return err
	}
	if j.Instance == "" {
		j.Instance = instanceID
	}
	if j.Instance != instanceID {
		return fmt.Errorf("%w: journal instance does not match", ErrJournal)
	}
	if j.RestoreID == "" {
		return fmt.Errorf("%w: restore id is missing", ErrJournal)
	}
	sealed, err := sealJournal(j, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(sealed, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	path, err := journalPath(dir, instanceID)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if rerr := owner.Restrict(tmp); rerr != nil {
		cerr := f.Close()
		removeErr := os.Remove(tmp)
		return errors.Join(rerr, cerr, removeErr)
	}
	_, werr := f.Write(raw)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil {
		if removeErr := os.Remove(tmp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(werr, removeErr)
		}
		return werr
	}
	if serr != nil {
		return serr
	}
	if cerr != nil {
		return cerr
	}
	if err := renameRetry(tmp, path); err != nil {
		return err
	}
	if err := owner.Restrict(path); err != nil {
		return err
	}
	return syncDir(dir)
}

func readJournal(dir, instanceID string, key []byte) (j journalFile, err error) {
	path, err := journalPath(dir, instanceID)
	if err != nil {
		return journalFile{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return journalFile{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return journalFile{}, fmt.Errorf("%w: journal is a symlink", ErrJournal)
	}
	if reparse, rerr := reparsePoint(path); rerr != nil {
		return journalFile{}, rerr
	} else if reparse {
		return journalFile{}, fmt.Errorf("%w: journal is a reparse point", ErrJournal)
	}
	f, err := os.Open(path)
	if err != nil {
		return journalFile{}, err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err = dec.Decode(&j); err != nil {
		return journalFile{}, fmt.Errorf("%w: %v", ErrJournal, err)
	}
	if j.Version != journalVersion || (j.State != journalPending && j.State != journalCommitted) {
		return journalFile{}, fmt.Errorf("%w: journal in %s is not usable", ErrJournal, dir)
	}
	if err = checkJournalMAC(j, key); err != nil {
		return journalFile{}, err
	}
	if j.Instance != instanceID {
		return journalFile{}, fmt.Errorf("%w: journal instance does not match", ErrJournal)
	}
	if j.RestoreID == "" {
		return journalFile{}, fmt.Errorf("%w: restore id is missing", ErrJournal)
	}
	return j, nil
}

func acceptJournal(j journalFile, instance, expectedRestore string) error {
	if j.Instance != instance {
		return fmt.Errorf("%w: journal instance does not match", ErrJournal)
	}
	if !journalIDMatches(j, expectedRestore) {
		return fmt.Errorf("%w: restore id does not match", ErrJournal)
	}
	return nil
}

func journalIDMatches(j journalFile, expectedRestore string) bool {
	return j.RestoreID != "" && expectedRestore != "" && hmac.Equal([]byte(j.RestoreID), []byte(expectedRestore))
}

func removeJournal(dir, instanceID string) error {
	path, err := journalPath(dir, instanceID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	tmp := path + ".tmp"
	if err := os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDir(dir)
}

// commitJournal fsyncs the committed record before any hold is deleted.
// The journal file is removed before afterRemove runs. afterRemove rotates
// the restore id, so a crash before the remove leaves an id that still
// matches and a crash after it leaves no journal.
func commitJournal(dir, instanceID string, st *stagedSet, key []byte, allowed []string, restoreID string, afterRemove func() error) error {
	if err := writeJournal(dir, instanceID, journalFrom(st, journalCommitted, instanceID, restoreID), key); err != nil {
		return err
	}
	if hookCommitReread != nil {
		if err := hookCommitReread(); err != nil {
			return err
		}
	}
	j, err := readJournal(dir, instanceID, key)
	if err != nil {
		return err
	}
	if err := acceptJournal(j, instanceID, restoreID); err != nil {
		return err
	}
	if j.State != journalCommitted {
		return errors.New("restore journal was not committed")
	}
	if err := validateJournal(j, allowed); err != nil {
		return err
	}
	if hookCommitCleanup != nil {
		if err := hookCommitCleanup(); err != nil {
			return err
		}
	}
	if err := deleteHolds(j); err != nil {
		return err
	}
	return finishJournalFile(dir, instanceID, afterRemove)
}

func markSwapped(dir, instanceID string, key []byte, dest string) error {
	if hookMarkSwapped != nil {
		if err := hookMarkSwapped(); err != nil {
			return err
		}
	}
	j, err := readJournal(dir, instanceID, key)
	if err != nil {
		return err
	}
	found := false
	for i := range j.Targets {
		if SamePath(j.Targets[i].Dest, dest) {
			j.Targets[i].Swapped = true
			found = true
		}
	}
	if !found {
		return fmt.Errorf("%w: swapped dest %s is not in the journal", ErrJournal, dest)
	}
	return writeJournal(dir, instanceID, j, key)
}

func journalHoldsExist(dir, instanceID string, key []byte, allowed []string, expectedRestore string) (bool, error) {
	j, err := readJournal(dir, instanceID, key)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := acceptJournal(j, instanceID, expectedRestore); err != nil {
		return false, err
	}
	if err := validateJournal(j, allowed); err != nil {
		return false, err
	}
	for _, target := range j.Targets {
		if target.Hold == "" {
			continue
		}
		if _, err := os.Lstat(target.Hold); err == nil {
			return true, nil
		}
	}
	return false, nil
}

func rollbackJournal(dir, instanceID string, key []byte, allowed []string, expectedRestore string, afterRemove func() error) error {
	j, err := readJournal(dir, instanceID, key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := acceptJournal(j, instanceID, expectedRestore); err != nil {
		return err
	}
	if err := validateJournal(j, allowed); err != nil {
		return err
	}
	if j.State == journalCommitted {
		if err := deleteHolds(j); err != nil {
			return err
		}
		return finishJournalFile(dir, instanceID, afterRemove)
	}
	var errs error
	for i := len(j.Targets) - 1; i >= 0; i-- {
		if err := restoreHold(j.Targets[i]); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	for _, path := range j.Staging {
		if err := refuseSpecial(path); err != nil {
			errs = errors.Join(errs, err)
			continue
		}
		if err := removeAllNoFollow(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = errors.Join(errs, err)
		}
	}
	if errs != nil {
		return errs
	}
	return finishJournalFile(dir, instanceID, afterRemove)
}

// finishJournalFile removes the journal and then runs afterRemove.
// afterRemove rotates the restore id. Removing first means a crash in
// between leaves no journal rather than a journal whose id can never match.
func finishJournalFile(dir, instanceID string, afterRemove func() error) error {
	if hookJournalRemove != nil {
		if err := hookJournalRemove(); err != nil {
			return err
		}
	}
	if err := removeJournal(dir, instanceID); err != nil {
		return err
	}
	if hookAfterJournalRemove != nil {
		if err := hookAfterJournalRemove(); err != nil {
			return err
		}
	}
	if afterRemove != nil {
		return afterRemove()
	}
	return nil
}

func deleteHolds(j journalFile) error {
	var errs error
	for _, target := range j.Targets {
		if target.Hold == "" {
			continue
		}
		if err := removeHold(target.Hold); err != nil {
			errs = errors.Join(errs, err)
		}
	}
	return errs
}

func removeHold(path string) error {
	if err := refuseSpecial(path); err != nil {
		return err
	}
	err := removeAllNoFollow(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// journalIdle reports a journal whose live destinations were not renamed.
// A hold that is present counts as swapped: the destination was already
// renamed into that hold, even when the journal has not recorded it. Idle
// means no hold is present and every destination still exists.
func journalIdle(j journalFile) bool {
	for _, target := range j.Targets {
		if target.Swapped {
			return false
		}
		if target.Hold != "" {
			_, err := os.Lstat(target.Hold)
			if err == nil || !errors.Is(err, os.ErrNotExist) {
				return false
			}
		}
		if target.Dest == "" {
			return false
		}
		if _, err := os.Lstat(target.Dest); err != nil {
			return false
		}
	}
	return true
}

// journalUnusable reports a journal file that cannot be trusted: bad HMAC,
// wrong instance, bad JSON, or a path check that failed before the body
// was accepted. Callers may delete that file and must not follow paths
// inside it.
func journalUnusable(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ErrJournalAuth) || errors.Is(err, ErrJournal)
}

func journalFinished(j journalFile) bool {
	if j.State != journalCommitted {
		return false
	}
	for _, target := range j.Targets {
		if target.Hold == "" {
			continue
		}
		_, err := os.Lstat(target.Hold)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		return false
	}
	return true
}

func childNameOK(name string) bool {
	if name == "" || name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, `/\`)
}

func splitFinal(path string) (string, string, error) {
	if path == "" {
		return "", "", fmt.Errorf("%w: empty path", ErrJournal)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	abs = filepath.Clean(abs)
	name := filepath.Base(abs)
	parent := filepath.Dir(abs)
	if parent == abs || !childNameOK(name) {
		return "", "", fmt.Errorf("%w: %s", ErrJournal, path)
	}
	return parent, name, nil
}

func journalIsCommitted(dir, instanceID string, key []byte, expectedRestore string) (bool, error) {
	j, err := readJournal(dir, instanceID, key)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := acceptJournal(j, instanceID, expectedRestore); err != nil {
		return false, err
	}
	return j.State == journalCommitted, nil
}

func restoreHold(target journalTarget) error {
	if err := refuseSpecial(target.Dest); err != nil {
		return err
	}
	if target.Hold == "" {
		if target.Dest == "" {
			return nil
		}
		_, err := os.Lstat(target.Dest)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		return removeAllNoFollow(target.Dest)
	}
	if err := refuseSpecial(target.Hold); err != nil {
		return err
	}
	_, err := os.Lstat(target.Hold)
	if errors.Is(err, os.ErrNotExist) {
		_, derr := os.Lstat(target.Dest)
		if errors.Is(derr, os.ErrNotExist) {
			return fmt.Errorf("%w: %s", ErrMissingHold, target.Hold)
		}
		if derr != nil {
			return derr
		}
		if target.Swapped {
			return fmt.Errorf("%w: %s", ErrMissingHold, target.Hold)
		}
		return nil
	}
	if err != nil {
		return err
	}
	return undoSwapped(target.Dest, target.Hold, true)
}

func validateJournal(j journalFile, allowed []string) error {
	if len(allowed) == 0 {
		return fmt.Errorf("%w: no save or config paths to check", ErrJournal)
	}
	for _, target := range j.Targets {
		if !destAllowed(target.Dest, allowed) {
			return fmt.Errorf("%w: dest %s is not a save or config path", ErrJournal, target.Dest)
		}
		if err := refuseSpecial(target.Dest); err != nil {
			return err
		}
		if target.Hold != "" {
			if !holdNameOK(target.Dest, target.Hold) {
				return fmt.Errorf("%w: hold %s", ErrJournal, target.Hold)
			}
			if err := refuseSpecial(target.Hold); err != nil {
				return err
			}
		}
	}
	for _, path := range j.Staging {
		if !stagingOK(path, allowed) {
			return fmt.Errorf("%w: staging %s", ErrJournal, path)
		}
		if err := refuseSpecial(path); err != nil {
			return err
		}
	}
	return nil
}

func destAllowed(dest string, allowed []string) bool {
	for _, path := range allowed {
		if SamePath(dest, path) {
			return true
		}
	}
	return false
}

func holdNameOK(dest, hold string) bool {
	if dest == "" || hold == "" {
		return false
	}
	if !SamePath(filepath.Dir(hold), filepath.Dir(dest)) {
		return false
	}
	prefix := holdPrefix + filepath.Base(dest)
	name := filepath.Base(hold)
	if name == prefix {
		return true
	}
	if !strings.HasPrefix(name, prefix+".") {
		return false
	}
	rest := name[len(prefix)+1:]
	if rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func stagingOK(path string, allowed []string) bool {
	name := filepath.Base(path)
	if !strings.HasPrefix(name, restorePrefix) || len(name) == len(restorePrefix) {
		return false
	}
	for _, dest := range allowed {
		if SamePath(filepath.Dir(path), filepath.Dir(dest)) {
			return true
		}
	}
	return false
}

func refuseSpecial(path string) error {
	if path == "" {
		return nil
	}
	return componentsSafe(path)
}

// componentsSafe refuses a symlink or reparse point on any component of path.
// A missing leaf is allowed once its existing parents have been checked. The
// parent is opened no-follow so a symlink that replaces it is not used.
func componentsSafe(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	var comps []string
	cur := abs
	for {
		comps = append(comps, cur)
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	for i := len(comps) - 1; i >= 0; i-- {
		info, lerr := os.Lstat(comps[i])
		if errors.Is(lerr, os.ErrNotExist) {
			if i+1 < len(comps) {
				return openParentNoFollow(comps[i+1])
			}
			return nil
		}
		if lerr != nil {
			return lerr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s is a symlink", ErrJournal, comps[i])
		}
		reparse, rerr := reparsePoint(comps[i])
		if rerr != nil {
			return rerr
		}
		if reparse {
			return fmt.Errorf("%w: %s is a reparse point", ErrJournal, comps[i])
		}
	}
	parent := filepath.Dir(abs)
	if parent == abs {
		return nil
	}
	return openParentNoFollow(parent)
}

func openParentNoFollow(parent string) error {
	f, err := openNoFollow(parent)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return f.Close()
}

func unjournaledHolds(dir string, known map[string]struct{}) ([]string, error) {
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
		if !stringsHasHoldPrefix(name) {
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

func holdKnown(path string, known map[string]struct{}) bool {
	if _, ok := known[path]; ok {
		return true
	}
	for hold := range known {
		if SamePath(hold, path) {
			return true
		}
	}
	return false
}

func stringsHasHoldPrefix(name string) bool {
	return len(name) > len(holdPrefix) && name[:len(holdPrefix)] == holdPrefix
}
