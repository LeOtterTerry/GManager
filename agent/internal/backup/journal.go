package backup

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

const (
	journalName      = ".gmanager-restore-journal"
	journalPending   = "pending"
	journalCommitted = "committed"
	journalVersion   = 1
)

// ErrUnjournaledHold means a hold copy is on disk without a restore journal
// that names it. Recovery reports it and does not delete it.
var ErrUnjournaledHold = errors.New("unjournaled hold")

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
	_, err := os.Lstat(dest)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
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

func writeJournal(dir string, j journalFile) error {
	if dir == "" {
		return errors.New("restore journal directory is not set")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(j, "", "  ")
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
	return syncDir(dir)
}

func readJournal(dir string) (j journalFile, err error) {
	f, err := os.Open(journalPath(dir))
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
		return journalFile{}, fmt.Errorf("restore journal in %s is not usable", dir)
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
func commitJournal(dir string, st *stagedSet) error {
	if err := writeJournal(dir, journalFrom(st, journalCommitted)); err != nil {
		return err
	}
	j, err := readJournal(dir)
	if err != nil {
		return err
	}
	if j.State != journalCommitted {
		return errors.New("restore journal was not committed")
	}
	for _, target := range j.Targets {
		if target.Hold == "" {
			continue
		}
		if err := os.RemoveAll(target.Hold); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return removeJournal(dir)
}

func journalHoldsExist(dir string) bool {
	j, err := readJournal(dir)
	if err != nil {
		return false
	}
	for _, target := range j.Targets {
		if target.Hold == "" {
			continue
		}
		if _, err := os.Lstat(target.Hold); err == nil {
			return true
		}
	}
	return false
}

func rollbackJournal(dir string) error {
	j, err := readJournal(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if j.State == journalCommitted {
		for _, target := range j.Targets {
			if target.Hold == "" {
				continue
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
		if err := os.RemoveAll(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return removeJournal(dir)
}

func journalIsCommitted(dir string) bool {
	j, err := readJournal(dir)
	return err == nil && j.State == journalCommitted
}

func restoreHold(target journalTarget) error {
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
	if _, err := os.Lstat(target.Hold); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return undoSwapped(target.Dest, target.Hold, true)
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
		if _, ok := known[full]; ok {
			continue
		}
		out = append(out, full)
	}
	return out, nil
}

func stringsHasHoldPrefix(name string) bool {
	return len(name) > len(holdPrefix) && name[:len(holdPrefix)] == holdPrefix
}
