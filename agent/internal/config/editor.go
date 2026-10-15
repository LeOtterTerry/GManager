// Package config implements the server.json editor.
//
// The agent stores the revision, sha256, mtime, and size of the last file it
// acknowledged. Before a read or a write it hashes the file again. If the file
// was changed outside the agent, the on-disk bytes become the merge base and
// nothing is written until conflicts are resolved. Unknown keys are kept.
// A stale If-Match revision is rejected. The file is never rewritten just
// because the server is starting; this package is not called from Start.
package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

// ErrStaleRevision is returned when If-Match does not match the current revision.
var ErrStaleRevision = errors.New("stale revision")

// ErrExternalDelete is returned when the config file disappeared outside the agent.
// The editor will not recreate it on a save.
var ErrExternalDelete = errors.New("config file was removed outside the agent")

// ErrInvalidResolution is returned when a conflict choice is not "disk" or "edit".
var ErrInvalidResolution = errors.New("invalid conflict resolution")

// EditError means the caller's JSON was rejected. The file was not written.
type EditError struct{ Err error }

func (e *EditError) Error() string { return e.Err.Error() }
func (e *EditError) Unwrap() error { return e.Err }

// DiskError means the on-disk file could not be merged. The file was not written.
type DiskError struct{ Err error }

func (e *DiskError) Error() string { return "on-disk config is not valid JSON: " + e.Err.Error() }
func (e *DiskError) Unwrap() error { return e.Err }

// Conflict is one field the user must choose between.
type Conflict struct {
	Field string          `json:"field"`
	Base  json.RawMessage `json:"base,omitempty"`
	Disk  json.RawMessage `json:"disk,omitempty"`
	Edit  json.RawMessage `json:"edit,omitempty"`
}

// ConflictError means the save was refused. The file was not written.
type ConflictError struct {
	Revision  int64
	Conflicts []Conflict
}

func (e *ConflictError) Error() string { return "conflicting fields" }

// StaleError is a stale revision that includes the revision the client should read.
type StaleError struct {
	Revision int64
}

func (e *StaleError) Error() string { return ErrStaleRevision.Error() }

func (e *StaleError) Unwrap() error { return ErrStaleRevision }

// ReadResult is the current view of the config file.
type ReadResult struct {
	Revision       int64
	SHA256         string
	Mtime          time.Time
	Size           int64
	ExternalChange bool
	// Content is the on-disk file. It is nil when the file does not exist.
	Content []byte
	// LastKnown is the last body the agent acknowledged, set when the file
	// has changed outside the agent.
	LastKnown []byte
}

// SaveResult is a successful save. Body is what the file holds afterward.
// Written is false when the merge result was already the on-disk bytes and
// the editor acknowledged them without rewriting the file.
type SaveResult struct {
	Revision int64
	SHA256   string
	Body     []byte
	Written  bool
}

// Editor guards one config file.
type Editor struct {
	mu         sync.Mutex
	configPath string
	statePath  string
}

// NewEditor tracks configPath and stores its revision metadata at statePath.
// statePath must not be the config file.
func NewEditor(configPath, statePath string) (*Editor, error) {
	if configPath == "" || statePath == "" {
		return nil, errors.New("config path and state path are required")
	}
	if configPath == statePath {
		return nil, errors.New("state path must not be the config file")
	}
	return &Editor{configPath: configPath, statePath: statePath}, nil
}

// ConfigPath returns the managed file.
func (e *Editor) ConfigPath() string { return e.configPath }

type snapshot struct {
	Revision      int64  `json:"revision"`
	SHA256        string `json:"sha256"`
	MtimeUnixNano int64  `json:"mtimeUnixNano"`
	Size          int64  `json:"size"`
	Body          []byte `json:"body"`
	Drift         bool   `json:"drift"`
	DriftSHA      string `json:"driftSha256"`
}

type diskFile struct {
	body   []byte
	sum    string
	mtime  time.Time
	size   int64
	absent bool
}

func (e *Editor) Read() (ReadResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	st, exists, err := e.load()
	if err != nil {
		return ReadResult{}, err
	}
	disk, err := e.readDisk()
	if err != nil {
		return ReadResult{}, err
	}
	if !exists {
		if disk.absent {
			return ReadResult{}, nil
		}
		st = acknowledge(disk, 1)
		if err := e.persist(st); err != nil {
			return ReadResult{}, err
		}
		return resultFrom(st, disk, false), nil
	}
	if err := e.observe(st, disk); err != nil {
		return ReadResult{}, err
	}
	return resultFrom(st, disk, st.Drift), nil
}

// Save merges editJSON onto the acknowledged base and the on-disk file.
// revision is the If-Match value. resolutions maps conflicting fields to
// "disk" or "edit". validate, when set, sees the merged document; a non-nil
// error prevents the write.
func (e *Editor) Save(revision int64, editJSON []byte, resolutions map[string]string, validate func([]byte) error) (SaveResult, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	st, _, err := e.load()
	if err != nil {
		return SaveResult{}, err
	}
	disk, err := e.readDisk()
	if err != nil {
		return SaveResult{}, err
	}
	if err := e.observe(st, disk); err != nil {
		return SaveResult{}, err
	}
	if revision != st.Revision {
		return SaveResult{}, &StaleError{Revision: st.Revision}
	}
	if disk.absent && len(st.Body) > 0 {
		return SaveResult{}, ErrExternalDelete
	}

	baseDoc, err := Parse(st.Body)
	if err != nil {
		return SaveResult{}, fmt.Errorf("acknowledged config is not valid JSON: %w", err)
	}
	var diskDoc *Doc
	if disk.absent {
		diskDoc = &Doc{Values: map[string]json.RawMessage{}}
	} else {
		diskDoc, err = Parse(disk.body)
		if err != nil {
			return SaveResult{}, &DiskError{Err: err}
		}
	}
	editDoc, err := Parse(editJSON)
	if err != nil {
		return SaveResult{}, &EditError{Err: err}
	}

	merged, conflicts, err := mergeDocs(baseDoc, diskDoc, editDoc, resolutions)
	if err != nil {
		return SaveResult{}, err
	}
	if len(conflicts) > 0 {
		return SaveResult{}, &ConflictError{Revision: st.Revision, Conflicts: conflicts}
	}

	// No drift and no on-disk file: write the edit. With a file, prefer the
	// disk's raw values whenever the merge did not change them.
	if !disk.absent && docsSameRaw(merged, diskDoc) {
		if validate != nil {
			if err := validate(disk.body); err != nil {
				return SaveResult{}, err
			}
		}
		adopt(st, disk)
		if err := e.persist(st); err != nil {
			return SaveResult{}, err
		}
		return SaveResult{Revision: st.Revision, SHA256: st.SHA256, Body: append([]byte(nil), disk.body...), Written: false}, nil
	}

	out := merged.Marshal()
	if validate != nil {
		if err := validate(out); err != nil {
			return SaveResult{}, err
		}
	}
	if err := atomicWrite(e.configPath, out, !disk.absent); err != nil {
		return SaveResult{}, err
	}
	written, err := e.readDisk()
	if err != nil {
		return SaveResult{}, err
	}
	if written.absent {
		return SaveResult{}, errors.New("config file missing after write")
	}
	st.Revision++
	adopt(st, written)
	if err := e.persist(st); err != nil {
		return SaveResult{}, err
	}
	return SaveResult{Revision: st.Revision, SHA256: st.SHA256, Body: append([]byte(nil), written.body...), Written: true}, nil
}

func resultFrom(st *snapshot, disk diskFile, external bool) ReadResult {
	res := ReadResult{
		Revision:       st.Revision,
		ExternalChange: external,
		LastKnown:      nil,
	}
	if external {
		res.LastKnown = append([]byte(nil), st.Body...)
		if disk.absent {
			return res
		}
		res.Content = append([]byte(nil), disk.body...)
		res.SHA256 = disk.sum
		res.Mtime = disk.mtime
		res.Size = disk.size
		return res
	}
	if disk.absent {
		return res
	}
	res.Content = append([]byte(nil), disk.body...)
	res.SHA256 = disk.sum
	res.Mtime = disk.mtime
	res.Size = disk.size
	return res
}

func acknowledge(disk diskFile, revision int64) *snapshot {
	st := &snapshot{Revision: revision}
	adopt(st, disk)
	return st
}

func adopt(st *snapshot, disk diskFile) {
	st.SHA256 = disk.sum
	st.MtimeUnixNano = disk.mtime.UnixNano()
	st.Size = disk.size
	st.Body = append([]byte(nil), disk.body...)
	st.Drift = false
	st.DriftSHA = ""
}

// observe records an external change. It never writes the config file.
func (e *Editor) observe(st *snapshot, disk diskFile) error {
	if !drifted(st, disk) {
		if st.Drift {
			st.Drift = false
			st.DriftSHA = ""
			return e.persist(st)
		}
		return nil
	}
	sum := disk.sum
	if disk.absent {
		sum = "missing"
	}
	if st.Drift && st.DriftSHA == sum {
		return nil
	}
	st.Revision++
	st.Drift = true
	st.DriftSHA = sum
	return e.persist(st)
}

func drifted(st *snapshot, disk diskFile) bool {
	if disk.absent {
		return st.SHA256 != "" || len(st.Body) > 0
	}
	if st.SHA256 != disk.sum || st.Size != disk.size || st.MtimeUnixNano != disk.mtime.UnixNano() {
		return true
	}
	return false
}

func (e *Editor) readDisk() (diskFile, error) {
	info, err := os.Stat(e.configPath)
	if errors.Is(err, os.ErrNotExist) {
		return diskFile{absent: true}, nil
	}
	if err != nil {
		return diskFile{}, err
	}
	body, err := os.ReadFile(e.configPath)
	if errors.Is(err, os.ErrNotExist) {
		return diskFile{absent: true}, nil
	}
	if err != nil {
		return diskFile{}, err
	}
	sum := sha256.Sum256(body)
	return diskFile{
		body:  body,
		sum:   hex.EncodeToString(sum[:]),
		mtime: info.ModTime(),
		size:  info.Size(),
	}, nil
}

func (e *Editor) load() (*snapshot, bool, error) {
	b, err := os.ReadFile(e.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return &snapshot{}, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var st snapshot
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, false, fmt.Errorf("read config state: %w", err)
	}
	return &st, true, nil
}

func (e *Editor) persist(st *snapshot) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return atomicWrite(e.statePath, b, false)
}

func mergeDocs(base, disk, edit *Doc, resolutions map[string]string) (*Doc, []Conflict, error) {
	inEdit := map[string]struct{}{}
	if edit != nil {
		for _, key := range edit.Order {
			inEdit[key] = struct{}{}
		}
	}
	seen := map[string]struct{}{}
	var order []string
	add := func(key string) {
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		order = append(order, key)
	}
	for _, doc := range []*Doc{disk, base, edit} {
		if doc == nil {
			continue
		}
		for _, key := range doc.Order {
			add(key)
		}
	}

	out := &Doc{Values: map[string]json.RawMessage{}}
	var conflicts []Conflict
	for _, key := range order {
		baseRaw, baseOK := base.get(key)
		diskRaw, diskOK := disk.get(key)
		_, editPresent := inEdit[key]
		editRaw, editOK := edit.get(key)

		diskChanged := !rawEqual(baseRaw, baseOK, diskRaw, diskOK)
		editChanged := false
		if editPresent {
			editChanged = !rawEqual(baseRaw, baseOK, editRaw, editOK)
		}

		switch {
		case diskChanged && editChanged && !rawEqual(diskRaw, diskOK, editRaw, editOK):
			choice := resolutions[key]
			switch choice {
			case "":
				conflicts = append(conflicts, Conflict{Field: key, Base: baseRaw, Disk: diskRaw, Edit: editRaw})
			case "disk":
				if diskOK {
					out.Order = append(out.Order, key)
					out.Values[key] = diskRaw
				}
			case "edit":
				if editOK {
					out.Order = append(out.Order, key)
					out.Values[key] = editRaw
				}
			default:
				return nil, nil, fmt.Errorf("%w: %s", ErrInvalidResolution, key)
			}
		case diskChanged:
			if diskOK {
				out.Order = append(out.Order, key)
				out.Values[key] = diskRaw
			}
		case editChanged:
			out.Order = append(out.Order, key)
			out.Values[key] = editRaw
		default:
			switch {
			case diskOK:
				out.Order = append(out.Order, key)
				out.Values[key] = diskRaw
			case baseOK:
				out.Order = append(out.Order, key)
				out.Values[key] = baseRaw
			case editOK:
				out.Order = append(out.Order, key)
				out.Values[key] = editRaw
			}
		}
	}
	return out, conflicts, nil
}
