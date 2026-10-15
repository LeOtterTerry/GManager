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
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
	if err := cleanupTemps(filepath.Dir(configPath), filepath.Base(configPath)); err != nil {
		return nil, err
	}
	if err := cleanupTemps(filepath.Dir(statePath), filepath.Base(statePath)); err != nil {
		return nil, err
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
	// Redacted holds sha256 hex of secret raw values removed from Body before
	// the snapshot is written. The in-memory body keeps those values.
	Redacted map[string]string `json:"redacted,omitempty"`
	Drift    bool              `json:"drift"`
	DriftSHA string            `json:"driftSha256"`
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
	if exists {
		rehydrate(st, disk)
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
	rehydrate(st, disk)
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

// Acknowledge records the current on-disk file as written by the agent.
// Restore calls this after it replaces server.json so the next read does not
// treat that restore as a manual edit. The config file is not rewritten.
// A missing file returns ErrExternalDelete.
func (e *Editor) Acknowledge() (ReadResult, error) {
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
	if disk.absent {
		return ReadResult{}, ErrExternalDelete
	}
	if !exists {
		st = acknowledge(disk, 1)
		if err := e.persist(st); err != nil {
			return ReadResult{}, err
		}
		return resultFrom(st, disk, false), nil
	}
	rehydrate(st, disk)
	if !drifted(st, disk) {
		if st.Drift {
			st.Drift = false
			st.DriftSHA = ""
			if err := e.persist(st); err != nil {
				return ReadResult{}, err
			}
		}
		return resultFrom(st, disk, false), nil
	}
	st.Revision++
	adopt(st, disk)
	if err := e.persist(st); err != nil {
		return ReadResult{}, err
	}
	return resultFrom(st, disk, false), nil
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
	st.Redacted = nil
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
	stored := *st
	if len(st.Redacted) > 0 {
		stored.Body = append([]byte(nil), st.Body...)
		stored.Redacted = cloneRedacted(st.Redacted)
	} else {
		body, redacted, err := redactPassword(st.Body)
		if err != nil {
			stored.Body = append([]byte(nil), st.Body...)
			stored.Redacted = nil
		} else {
			stored.Body = body
			stored.Redacted = redacted
		}
	}
	b, err := json.MarshalIndent(&stored, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return atomicWrite(e.statePath, b, false)
}

// redactPassword copies body with the password key removed. The returned map
// is the sha256 of that key's raw JSON, so a later save can restore it from
// the live file without keeping the secret in the metadata file.
func redactPassword(body []byte) ([]byte, map[string]string, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return append([]byte(nil), body...), nil, nil
	}
	doc, err := Parse(body)
	if err != nil {
		return nil, nil, err
	}
	raw, ok := doc.get("password")
	if !ok {
		return append([]byte(nil), body...), nil, nil
	}
	sum := sha256.Sum256(raw)
	var order []string
	for _, key := range doc.Order {
		if key == "password" {
			continue
		}
		order = append(order, key)
	}
	doc.Order = order
	delete(doc.Values, "password")
	return doc.Marshal(), map[string]string{"password": hex.EncodeToString(sum[:])}, nil
}

func rehydrate(st *snapshot, disk diskFile) {
	if st == nil || len(st.Redacted) == 0 || disk.absent {
		return
	}
	restored, ok := restoreRedacted(st.Body, st.Redacted, disk.body)
	if !ok {
		return
	}
	st.Body = restored
	st.Redacted = nil
}

// restoreRedacted puts the on-disk password back into base when its raw JSON
// hashes to the stored digest. A different on-disk password stays out of base
// so the merge treats it as an external change.
func restoreRedacted(base []byte, redacted map[string]string, diskBody []byte) ([]byte, bool) {
	want := redacted["password"]
	if want == "" {
		return base, false
	}
	diskDoc, err := Parse(diskBody)
	if err != nil {
		return base, false
	}
	raw, ok := diskDoc.get("password")
	if !ok {
		return base, false
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != want {
		return base, false
	}
	baseDoc, err := Parse(base)
	if err != nil {
		return base, false
	}
	if _, exists := baseDoc.Values["password"]; exists {
		return base, false
	}
	baseDoc.Order = append(baseDoc.Order, "password")
	baseDoc.Values["password"] = append(json.RawMessage(nil), raw...)
	return baseDoc.Marshal(), true
}

func cloneRedacted(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
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
