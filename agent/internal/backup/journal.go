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

// hookCommitCleanup, when set, runs after the committed journal is fsynced
// and before its holds are deleted. Tests use it to fail cleanup.
var hookCommitCleanup func() error

type journalTarget struct {
	Dest string `json:"dest"`
	Hold string `json:"hold,omitempty"`
	Dir  bool   `json:"dir"`
}

type journalFile struct {
	Version int             `json:"version"`
	State   string          `json:"state"`
	Targets []journalTarget `json:"targets"`
	Staging []string        `json:"staging,omitempty"`
	HMAC    string          `json:"hmac,omitempty"`
}

func journalPath(dir string) string {
	return filepath.Join(dir, journalName)
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

func journalFrom(st *stagedSet, state string) journalFile {
	j := journalFile{Version: journalVersion, State: state}
	if st == nil {
		return j
	}
	for _, dir := range st.dirs {
		j.Targets = append(j.Targets, journalTarget{Dest: dir.dest, Hold: dir.hold, Dir: true})
		if dir.tmp != "" {
			j.Staging = append(j.Staging, dir.tmp)
		}
	}
	for _, file := range st.files {
		j.Targets = append(j.Targets, journalTarget{Dest: file.dest, Hold: file.hold, Dir: false})
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

func writeJournal(dir string, j journalFile, key []byte) error {
	if dir == "" {
		return errors.New("restore journal directory is not set")
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
	path := journalPath(dir)
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
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

func readJournal(dir string, key []byte) (j journalFile, err error) {
	path := journalPath(dir)
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
		return journalFile{}, err
	}
	if j.Version != journalVersion || (j.State != journalPending && j.State != journalCommitted) {
		return journalFile{}, fmt.Errorf("%w: journal in %s is not usable", ErrJournal, dir)
	}
	if err = checkJournalMAC(j, key); err != nil {
		return journalFile{}, err
	}
	return j, nil
}

func removeJournal(dir string) error {
	path := journalPath(dir)
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
func commitJournal(dir string, st *stagedSet, key []byte, allowed []string) error {
	if err := writeJournal(dir, journalFrom(st, journalCommitted), key); err != nil {
		return err
	}
	j, err := readJournal(dir, key)
	if err != nil {
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
	for _, target := range j.Targets {
		if target.Hold == "" {
			continue
		}
		if err := refuseSpecial(target.Hold); err != nil {
			return err
		}
		if err := os.RemoveAll(target.Hold); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return removeJournal(dir)
}

func journalHoldsExist(dir string, key []byte, allowed []string) (bool, error) {
	j, err := readJournal(dir, key)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
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

func rollbackJournal(dir string, key []byte, allowed []string) error {
	j, err := readJournal(dir, key)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := validateJournal(j, allowed); err != nil {
		return err
	}
	if j.State == journalCommitted {
		for _, target := range j.Targets {
			if target.Hold == "" {
				continue
			}
			if err := refuseSpecial(target.Hold); err != nil {
				return err
			}
			if err := os.RemoveAll(target.Hold); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return removeJournal(dir)
	}
	for i := len(j.Targets) - 1; i >= 0; i-- {
		if err := restoreHold(j.Targets[i]); err != nil {
			return err
		}
	}
	for _, path := range j.Staging {
		if err := refuseSpecial(path); err != nil {
			return err
		}
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return removeJournal(dir)
}

func journalIsCommitted(dir string, key []byte) (bool, error) {
	j, err := readJournal(dir, key)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
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
		if _, err := os.Lstat(target.Dest); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		return os.RemoveAll(target.Dest)
	}
	if err := refuseSpecial(target.Hold); err != nil {
		return err
	}
	if _, err := os.Lstat(target.Hold); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("%w: %s", ErrMissingHold, target.Hold)
	} else if err != nil {
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
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrJournal, path)
	}
	reparse, err := reparsePoint(path)
	if err != nil {
		return err
	}
	if reparse {
		return fmt.Errorf("%w: %s is a reparse point", ErrJournal, path)
	}
	return nil
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
