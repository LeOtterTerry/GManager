// Package backup writes timestamped zip archives of a module's save
// directories and config files. Each zip carries a sha256 manifest signed
// with an HMAC key that stays outside the zip.
// The caller must already have stopped the game process. This package does
// not invent a save path: GameModule.SavePaths decides that, and an unknown
// path fails the backup.
package backup

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/internal/disk"
	"github.com/LeOtterTerry/GManager/agent/internal/owner"
)

const (
	// KindManual is a backup the operator asked for.
	KindManual = "manual"
	// KindUpdate is the backup taken by the update flow before SteamCMD runs.
	KindUpdate = "update"
	// KindSafety is the backup taken immediately before a restore.
	KindSafety = "safety"

	// DefaultKeep is how many backups retention leaves when Keep is unset.
	DefaultKeep = 10

	// SuggestedDir is the folder an installer may offer when drive P: exists.
	// The agent does not select it. The default is <data-dir>/backups unless
	// --backup-dir is set.
	SuggestedDir = `P:\GManager\Backups`
)

const volumeP = `P:\`

var (
	// ErrNoSpace means the destination volume cannot hold the write.
	ErrNoSpace = errors.New("not enough free space")
	// ErrIntegrity means the zip does not match its manifest.
	ErrIntegrity = errors.New("backup failed integrity check")
	// ErrNotFound means no zip exists for that id.
	ErrNotFound = errors.New("unknown backup")
	// ErrInvalidID means the id is not a backup id this package writes.
	ErrInvalidID = errors.New("invalid backup id")
	// ErrInvalidCursor means a list cursor is not a backup id.
	ErrInvalidCursor = errors.New("invalid cursor")
	// ErrServerRunning means a backup was asked for while the process was up.
	ErrServerRunning = errors.New("server is running")
	// ErrUnsigned means the manifest has no HMAC. Hashes can still be checked.
	// Listing reports the zip as unsigned. Restore refuses it until Sign.
	ErrUnsigned = errors.New("backup manifest is unsigned")
	// ErrKeyMissing means backups are already on disk and every copy of the
	// HMAC key is gone. A new key is not created, because it would fail every
	// existing archive.
	ErrKeyMissing = errors.New("backup integrity key is missing and backups already exist")
	// ErrNotQuarantined means Discard or Release was asked to act on a backup
	// that is still a normal zip.
	ErrNotQuarantined = errors.New("backup is not quarantined")
	// ErrLiveBackup means Release was asked to restore a quarantine name
	// while that id still has a live zip.
	ErrLiveBackup = errors.New("a live backup with that id already exists")
	// ErrAmbiguousQuarantine means more than one quarantine file matches the id.
	ErrAmbiguousQuarantine = errors.New("more than one quarantined copy")
	// ErrTooNew means an unsigned zip is newer than the integrity key file.
	ErrTooNew = errors.New("backup is newer than the integrity key")
	// ErrManifestAck means sign was not given the manifest digest that was shown.
	ErrManifestAck = errors.New("manifest acknowledgement does not match")
)

// Entry is one backup as the list API returns it.
// Cursor is the backup id. Paths and file contents are omitted.
type Entry struct {
	Cursor     string    `json:"cursor"`
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"createdAt"`
	Bytes      int64     `json:"bytes"`
	FileCount  int       `json:"fileCount"`
	Kind       string    `json:"kind"`
	InstanceID string    `json:"instanceId"`
	// Corrupt is true when the zip could not be read. One bad zip does not
	// fail the list.
	Corrupt bool `json:"corrupt,omitempty"`
	// Unsigned is true when the manifest hashes match and the HMAC is empty.
	// Restore refuses the zip until an operator signs it.
	Unsigned bool `json:"unsigned,omitempty"`
	// Quarantined is true when the zip was set aside because it could not be
	// classified. Retention does not delete it. Discard does, and only then.
	Quarantined bool `json:"quarantined,omitempty"`
}

// Request describes one backup. Protect ids are kept even when they are
// older than the retention window, so a restore does not delete the zip it
// is about to read.
type Request struct {
	Instance core.Instance
	Module   core.GameModule
	Kind     string
	Protect  []string
}

// Store writes zips into Dir and deletes the oldest beyond Keep.
type Store struct {
	Dir  string
	Keep int
	// Now is the clock used for ids. Nil means time.Now.
	Now func() time.Time
	// Free reports free bytes. Nil means disk.Free. Tests replace it.
	Free func(string) (uint64, error)
	// Key is the HMAC key. Tests set it. Production leaves it empty and
	// uses KeyPath, which is never stored inside a zip.
	Key []byte
	// KeyPath is an owner-only file of at least 32 bytes outside the backup
	// directory. Copies are kept at KeyPath+".bak" and beside KeyPath as
	// integrity.key. A missing key is created only when no backup is on disk
	// yet. If a zip or a quarantined zip is already there and every copy is
	// gone, calls fail with ErrKeyMissing instead of minting a key that would
	// reject them.
	KeyPath string
	// JournalDir is the agent data directory. The restore journal is an
	// owner-only HMAC file there, not in the server directory.
	JournalDir string

	keyMu     sync.Mutex
	loadedKey []byte
	// mu guards quarantine, discard, release, sign, and prune.
	mu sync.Mutex
	// swapFail, when set, runs before each live swap during restore.
	// Tests use it to force a partial swap. Production leaves it nil.
	swapFail func() error
	// hookQuarantine, when set, runs at the start of quarantine while mu is
	// held. Tests use it to prove callers share that mutex.
	hookQuarantine func()

	cacheMu sync.Mutex
	cache   map[string]zipCacheEntry
}

// SuggestDir returns SuggestedDir when volume P: exists.
// exists nil uses VolumeExists. The boolean is false when P: is absent;
// the caller then chooses another directory.
func SuggestDir(exists func(string) bool) (string, bool) {
	if exists == nil {
		exists = VolumeExists
	}
	if exists(volumeP) {
		return SuggestedDir, true
	}
	return "", false
}

// VolumeExists reports whether path is a directory.
func VolumeExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// DefaultDir is <dataDir>/backups. SuggestDir is not applied here.
func DefaultDir(dataDir string) string {
	return filepath.Join(dataDir, "backups")
}

// Create zips the module save directories and config files.
// The server process must already be stopped. Create checks free space
// before it writes, then prunes older zips beyond Keep.
func (s *Store) Create(ctx context.Context, req Request) (Entry, error) {
	if err := ctx.Err(); err != nil {
		return Entry{}, err
	}
	if s == nil || s.Dir == "" {
		return Entry{}, errors.New("backup directory is not configured")
	}
	if err := ValidateDir(s.Dir, req.Instance.Dir); err != nil {
		return Entry{}, err
	}
	if req.Module == nil {
		return Entry{}, errors.New("module is required")
	}
	if !validKind(req.Kind) {
		return Entry{}, errors.New("invalid backup kind")
	}
	if _, err := s.integrityKey(); err != nil {
		return Entry{}, err
	}
	files, saves, total, err := s.collect(req)
	if err != nil {
		return Entry{}, err
	}
	need, err := spaceNeed(total, len(files)+1)
	if err != nil {
		return Entry{}, err
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return Entry{}, err
	}
	if err := s.sweepPartial(); err != nil {
		return Entry{}, err
	}
	if err := s.ensureSpace(s.Dir, need); err != nil {
		return Entry{}, err
	}
	now := s.now()
	id, err := s.allocateID(now)
	if err != nil {
		return Entry{}, err
	}
	man := Manifest{
		Version:    manifestVer,
		ID:         id,
		CreatedAt:  now.UTC(),
		InstanceID: req.Instance.ID,
		GameID:     req.Module.ID(),
		Kind:       req.Kind,
		Saves:      saves,
	}
	partial := filepath.Join(s.Dir, id+".zip.partial")
	final := filepath.Join(s.Dir, id+".zip")
	key, err := s.integrityKey()
	if err != nil {
		return Entry{}, err
	}
	written, err := writeZip(partial, man, files, key)
	if err != nil {
		if removeErr := os.Remove(partial); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return Entry{}, errors.Join(err, removeErr)
		}
		return Entry{}, err
	}
	if err := renameRetry(partial, final); err != nil {
		if removeErr := os.Remove(partial); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return Entry{}, errors.Join(err, removeErr)
		}
		return Entry{}, err
	}
	if err := syncDir(s.Dir); err != nil {
		return Entry{}, err
	}
	info, err := os.Stat(final)
	if err != nil {
		return Entry{}, err
	}
	protect := append(append([]string{}, req.Protect...), id)
	if err := s.prune(protect); err != nil {
		return Entry{}, fmt.Errorf("backup %s was written, retention failed: %w", id, err)
	}
	return entryFrom(id, info, written), nil
}

// List returns backups whose id sorts after cursor, oldest first, up to limit.
// next is the last returned id, or cursor when the page is empty.
func (s *Store) List(cursor string, limit int) ([]Entry, string, error) {
	if s == nil || s.Dir == "" {
		return nil, "", errors.New("backup directory is not configured")
	}
	if cursor != "" && !validID(cursor) {
		return nil, "", ErrInvalidCursor
	}
	if limit < 1 {
		return nil, "", errors.New("limit must be positive")
	}
	all, err := s.entries()
	if err != nil {
		return nil, "", err
	}
	next := cursor
	var page []Entry
	for _, entry := range all {
		if cursor != "" && entry.ID <= cursor {
			continue
		}
		page = append(page, entry)
		next = entry.ID
		if len(page) == limit {
			break
		}
	}
	if page == nil {
		page = []Entry{}
	}
	return page, next, nil
}

// Exists reports whether a zip with that id is present.
func (s *Store) Exists(id string) error {
	path, err := s.zipPath(id)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return ErrNotFound
	}
	return nil
}

// Verify checks the manifest and every sha256. It does not write.
func (s *Store) Verify(id string) error {
	a, err := s.openVerified(id)
	if err != nil {
		return err
	}
	return a.Close()
}

func (s *Store) openVerified(id string) (*archive, error) {
	path, err := s.zipPath(id)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	key, err := s.integrityKey()
	if err != nil {
		return nil, err
	}
	a, err := openArchive(path, openOpts{hash: true, mac: true, key: key})
	if err != nil {
		return nil, err
	}
	if a.unsigned {
		if closeErr := a.Close(); closeErr != nil {
			return nil, errors.Join(ErrUnsigned, closeErr)
		}
		return nil, ErrUnsigned
	}
	if a.man.ID != id {
		if closeErr := a.Close(); closeErr != nil {
			return nil, errors.Join(fmt.Errorf("%w: manifest id does not match the file name", ErrIntegrity), closeErr)
		}
		return nil, fmt.Errorf("%w: manifest id does not match the file name", ErrIntegrity)
	}
	return a, nil
}

func (s *Store) zipPath(id string) (string, error) {
	if !validID(id) {
		return "", ErrInvalidID
	}
	if s == nil || s.Dir == "" {
		return "", errors.New("backup directory is not configured")
	}
	return filepath.Join(s.Dir, id+".zip"), nil
}

func (s *Store) collect(req Request) ([]sourceFile, []SaveRoot, uint64, error) {
	saves, err := req.Module.SavePaths(req.Instance)
	if err != nil {
		return nil, nil, 0, err
	}
	if len(saves) == 0 {
		return nil, nil, 0, core.ErrSavePathUnknown
	}
	var files []sourceFile
	var roots []SaveRoot
	var total uint64
	seenZip := map[string]struct{}{}
	seenFold := map[string]string{}
	for i, root := range saves {
		info, err := os.Stat(root)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("save path: %w", err)
		}
		if !info.IsDir() {
			return nil, nil, 0, fmt.Errorf("save path %s is not a directory", root)
		}
		if err := guardSaveRoot(req.Instance.Dir, s.Dir, root); err != nil {
			return nil, nil, 0, err
		}
		roots = append(roots, SaveRoot{Index: i})
		err = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("refusing to back up symlink %s", path)
			}
			if d.IsDir() {
				return nil
			}
			if !d.Type().IsRegular() {
				return fmt.Errorf("refusing to back up %s", path)
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			zipName, err := zipPathFor(roleSave, rel, i)
			if err != nil {
				return err
			}
			if _, ok := seenZip[zipName]; ok {
				return fmt.Errorf("duplicate backup path %s", zipName)
			}
			if prev, ok := seenFold[strings.ToLower(zipName)]; ok {
				return fmt.Errorf("case-colliding paths %s and %s", prev, zipName)
			}
			seenZip[zipName] = struct{}{}
			seenFold[strings.ToLower(zipName)] = zipName
			fi, err := d.Info()
			if err != nil {
				return err
			}
			if fi.Size() < 0 {
				return fmt.Errorf("negative size for %s", zipName)
			}
			if uint64(fi.Size()) > maxZipFileBytes {
				return fmt.Errorf("%s is too large to back up", zipName)
			}
			next, err := addSize(total, uint64(fi.Size()))
			if err != nil {
				return err
			}
			total = next
			files = append(files, sourceFile{
				abs: path,
				entry: ManifestFile{
					Path: zipName,
					Role: roleSave,
					Root: i,
					Rel:  rel,
				},
			})
			return nil
		})
		if err != nil {
			return nil, nil, 0, err
		}
	}
	configs := req.Module.ConfigFiles(req.Instance)
	if len(configs) == 0 {
		return nil, nil, 0, errors.New("module has no config files")
	}
	for _, cf := range configs {
		rel := filepath.ToSlash(cf.RelPath)
		abs := filepath.Join(req.Instance.Dir, filepath.FromSlash(rel))
		if err := within(req.Instance.Dir, abs); err != nil {
			return nil, nil, 0, err
		}
		info, err := os.Stat(abs)
		if err != nil {
			return nil, nil, 0, fmt.Errorf("config file: %w", err)
		}
		if !info.Mode().IsRegular() {
			return nil, nil, 0, fmt.Errorf("config file %s is not a regular file", rel)
		}
		if uint64(info.Size()) > maxZipFileBytes {
			return nil, nil, 0, fmt.Errorf("%s is too large to back up", rel)
		}
		zipName, err := zipPathFor(roleConfig, rel, 0)
		if err != nil {
			return nil, nil, 0, err
		}
		if _, ok := seenZip[zipName]; ok {
			return nil, nil, 0, fmt.Errorf("duplicate backup path %s", zipName)
		}
		if prev, ok := seenFold[strings.ToLower(zipName)]; ok {
			return nil, nil, 0, fmt.Errorf("case-colliding paths %s and %s", prev, zipName)
		}
		seenZip[zipName] = struct{}{}
		seenFold[strings.ToLower(zipName)] = zipName
		next, err := addSize(total, uint64(info.Size()))
		if err != nil {
			return nil, nil, 0, err
		}
		total = next
		files = append(files, sourceFile{
			abs: abs,
			entry: ManifestFile{
				Path: zipName,
				Role: roleConfig,
				Rel:  rel,
			},
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].entry.Path < files[j].entry.Path })
	if len(files)+1 > maxZipEntries {
		return nil, nil, 0, errors.New("too many files to back up")
	}
	if total > maxZipBytes {
		return nil, nil, 0, errors.New("backup is too large")
	}
	return files, roots, total, nil
}

func (s *Store) ensureSpace(path string, need uint64) error {
	fn := s.Free
	if fn == nil {
		fn = disk.Free
	}
	free, err := fn(path)
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("%w: need %d bytes, %d free", ErrNoSpace, need, free)
	}
	return nil
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) keepN() int {
	if s.Keep < 1 {
		return DefaultKeep
	}
	return s.Keep
}

func (s *Store) allocateID(now time.Time) (string, error) {
	base := now.UTC().Format("20060102T150405.000000000Z")
	for i := 0; i < 1000; i++ {
		id := base
		if i > 0 {
			id = fmt.Sprintf("%s-%03d", base, i)
		}
		_, zipErr := os.Stat(filepath.Join(s.Dir, id+".zip"))
		_, partErr := os.Stat(filepath.Join(s.Dir, id+".zip.partial"))
		_, quarErr := os.Stat(filepath.Join(s.Dir, id+".zip.quarantine"))
		if errors.Is(zipErr, os.ErrNotExist) && errors.Is(partErr, os.ErrNotExist) && errors.Is(quarErr, os.ErrNotExist) {
			return id, nil
		}
		if zipErr != nil && !errors.Is(zipErr, os.ErrNotExist) {
			return "", zipErr
		}
		if partErr != nil && !errors.Is(partErr, os.ErrNotExist) {
			return "", partErr
		}
		if quarErr != nil && !errors.Is(quarErr, os.ErrNotExist) {
			return "", quarErr
		}
	}
	return "", errors.New("could not allocate a backup id")
}

type zipMeta struct {
	id           string
	kind         string
	instance     string
	created      time.Time
	files        int
	verified     bool
	unsigned     bool
	unclassified bool
}

// classifyOpenHook, when set, runs before each archive open on the classify
// path. Tests use it to force one transient failure. Production leaves it nil.
var classifyOpenHook func() error

type zipCacheEntry struct {
	size int64
	mod  int64
	meta zipMeta
}

func (s *Store) prune(protect []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	inv, err := s.inventory()
	if err != nil {
		return err
	}
	protected := map[string]struct{}{}
	for _, id := range protect {
		protected[id] = struct{}{}
	}
	keepN := s.keepN()
	keep := map[string]struct{}{}
	byKind := map[string][]zipMeta{}
	for _, z := range inv {
		if z.unclassified || z.unsigned || z.kind == "" {
			continue
		}
		byKind[z.kind] = append(byKind[z.kind], z)
	}
	for _, group := range byKind {
		start := 0
		if len(group) > keepN {
			start = len(group) - keepN
		}
		for _, z := range group[start:] {
			keep[z.id] = struct{}{}
		}
	}
	for id := range protected {
		keep[id] = struct{}{}
	}
	verifiedKept := false
	for _, z := range inv {
		if _, ok := keep[z.id]; ok && z.verified {
			verifiedKept = true
			break
		}
	}
	if !verifiedKept {
		for i := len(inv) - 1; i >= 0; i-- {
			if inv[i].verified {
				keep[inv[i].id] = struct{}{}
				break
			}
		}
	}
	for _, z := range inv {
		if _, ok := keep[z.id]; ok {
			continue
		}
		if _, ok := protected[z.id]; ok {
			continue
		}
		// An unsigned archive stays a zip until an operator signs or replaces
		// it. An archive we cannot classify is never deleted. It is renamed
		// aside so a later explicit Discard is the only way to remove it.
		if z.unsigned {
			continue
		}
		if z.unclassified || !z.verified || z.kind == "" {
			if err := s.quarantine(z.id); err != nil {
				return err
			}
			s.dropCache(z.id)
			continue
		}
		path, err := s.zipPath(z.id)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		s.dropCache(z.id)
	}
	return syncDir(s.Dir)
}

func (s *Store) inventory() ([]zipMeta, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}
	key, err := s.integrityKey()
	if err != nil {
		return nil, err
	}
	out := make([]zipMeta, 0, len(ids))
	for _, id := range ids {
		meta, err := s.classifyCached(id, key)
		if err != nil {
			return nil, err
		}
		out = append(out, meta)
	}
	return out, nil
}

func (s *Store) classifyCached(id string, key []byte) (zipMeta, error) {
	path, err := s.zipPath(id)
	if err != nil {
		return zipMeta{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return zipMeta{}, err
	}
	if meta, ok := s.cached(id, info); ok {
		return meta, nil
	}
	meta := s.classify(path, id, key)
	info, err = os.Stat(path)
	if err != nil {
		return meta, nil
	}
	s.remember(id, info, meta)
	return meta, nil
}

func (s *Store) classify(path, id string, key []byte) zipMeta {
	unknown := zipMeta{id: id, unclassified: true}
	a, err := s.openForClassify(path, key)
	if err != nil {
		a, err = s.openForClassify(path, key)
		if err != nil {
			return unknown
		}
	}
	meta, ok := metaFromArchive(a, id)
	if closeErr := a.Close(); closeErr != nil || !ok {
		return unknown
	}
	return meta
}

func (s *Store) openForClassify(path string, key []byte) (*archive, error) {
	if classifyOpenHook != nil {
		if err := classifyOpenHook(); err != nil {
			return nil, err
		}
	}
	return openArchive(path, openOpts{hash: true, mac: true, key: key})
}

func metaFromArchive(a *archive, id string) (zipMeta, bool) {
	if a == nil || a.man.ID != id || !validKind(a.man.Kind) {
		return zipMeta{}, false
	}
	meta := zipMeta{
		id:       id,
		kind:     a.man.Kind,
		instance: a.man.InstanceID,
		created:  a.man.CreatedAt.UTC(),
		files:    len(a.man.Files),
		unsigned: a.unsigned,
	}
	if !a.unsigned {
		meta.verified = true
	}
	return meta, true
}

func (s *Store) cached(id string, info os.FileInfo) (zipMeta, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == nil {
		return zipMeta{}, false
	}
	e, ok := s.cache[id]
	if !ok || e.size != info.Size() || e.mod != info.ModTime().UnixNano() {
		return zipMeta{}, false
	}
	return e.meta, true
}

func (s *Store) remember(id string, info os.FileInfo, meta zipMeta) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == nil {
		s.cache = map[string]zipCacheEntry{}
	}
	s.cache[id] = zipCacheEntry{size: info.Size(), mod: info.ModTime().UnixNano(), meta: meta}
}

func (s *Store) dropCache(id string) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	delete(s.cache, id)
}

func (s *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if filepath.Ext(name) != ".zip" {
			continue
		}
		id := name[:len(name)-len(".zip")]
		if !validID(id) {
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) entries() ([]Entry, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	quar, err := s.quarantined()
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 && len(quar) == 0 {
		return nil, nil
	}
	key, err := s.readIntegrityKey()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, id := range ids {
		path, err := s.zipPath(id)
		if err != nil {
			return nil, err
		}
		meta, err := s.classifyCached(id, key)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		if meta.unsigned {
			out = append(out, Entry{
				Cursor:     id,
				ID:         id,
				CreatedAt:  meta.created,
				Bytes:      info.Size(),
				FileCount:  meta.files,
				Kind:       meta.kind,
				InstanceID: meta.instance,
				Unsigned:   true,
			})
			continue
		}
		if meta.unclassified || !meta.verified {
			out = append(out, corruptEntry(id, info))
			continue
		}
		out = append(out, Entry{
			Cursor:     id,
			ID:         id,
			CreatedAt:  meta.created,
			Bytes:      info.Size(),
			FileCount:  meta.files,
			Kind:       meta.kind,
			InstanceID: meta.instance,
		})
	}
	for _, q := range quar {
		info, err := os.Stat(q.path)
		if err != nil {
			return nil, err
		}
		item := corruptEntry(q.id, info)
		item.Quarantined = true
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID == out[j].ID {
			return !out[i].Quarantined && out[j].Quarantined
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// Discard removes a quarantined archive. A live zip is left in place.
func (s *Store) Discard(id string) error {
	if s == nil || s.Dir == "" {
		return errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validID(id) {
		return ErrInvalidID
	}
	matches, err := s.quarantinePaths(id)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		if err := s.Exists(id); err == nil {
			return ErrNotQuarantined
		} else if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		return ErrNotFound
	}
	for _, path := range matches {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	s.dropCache(id)
	return syncDir(s.Dir)
}

func corruptEntry(id string, info os.FileInfo) Entry {
	return Entry{
		Cursor:  id,
		ID:      id,
		Bytes:   info.Size(),
		Corrupt: true,
	}
}

func (s *Store) sweepPartial() error {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".zip.partial") {
			continue
		}
		if err := os.Remove(filepath.Join(s.Dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) integrityKey() ([]byte, error) {
	if len(s.Key) >= 32 {
		return append([]byte(nil), s.Key...), nil
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if len(s.loadedKey) >= 32 {
		return append([]byte(nil), s.loadedKey...), nil
	}
	if s.KeyPath == "" {
		return nil, errors.New("backup integrity key is not configured")
	}
	copies := s.keyPaths()
	var found []byte
	for _, path := range copies {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(b) < 32 {
			continue
		}
		if found != nil && !bytes.Equal(found, b) {
			return nil, errors.New("backup integrity key copies disagree")
		}
		if found == nil {
			found = append([]byte(nil), b...)
		}
	}
	if found == nil {
		present, err := s.hasArchives()
		if err != nil {
			return nil, err
		}
		if present {
			return nil, ErrKeyMissing
		}
		found = make([]byte, 32)
		if _, err := rand.Read(found); err != nil {
			return nil, err
		}
	}
	for _, path := range copies {
		if err := s.ensureKeyCopy(path, found); err != nil {
			return nil, err
		}
	}
	s.loadedKey = append([]byte(nil), found...)
	return append([]byte(nil), found...), nil
}

func (s *Store) keyPaths() []string {
	if s.KeyPath == "" {
		return nil
	}
	paths := []string{s.KeyPath, s.KeyPath + ".bak"}
	side := filepath.Join(filepath.Dir(s.KeyPath), "integrity.key")
	if side != s.KeyPath && side != s.KeyPath+".bak" {
		paths = append(paths, side)
	}
	return paths
}

// readIntegrityKey returns a key already in memory or on disk. It does not
// create a key, rewrite a copy, or change file permissions.
func (s *Store) readIntegrityKey() ([]byte, error) {
	if len(s.Key) >= 32 {
		return append([]byte(nil), s.Key...), nil
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if len(s.loadedKey) >= 32 {
		return append([]byte(nil), s.loadedKey...), nil
	}
	if s.KeyPath == "" {
		return nil, errors.New("backup integrity key is not configured")
	}
	var found []byte
	for _, path := range s.keyPaths() {
		b, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if len(b) < 32 {
			continue
		}
		if found != nil && !bytes.Equal(found, b) {
			return nil, errors.New("backup integrity key copies disagree")
		}
		if found == nil {
			found = append([]byte(nil), b...)
		}
	}
	if found == nil {
		present, err := s.hasArchives()
		if err != nil {
			return nil, err
		}
		if present {
			return nil, ErrKeyMissing
		}
		return nil, errors.New("backup integrity key is not configured")
	}
	return append([]byte(nil), found...), nil
}

func (s *Store) ensureKeyCopy(path string, key []byte) error {
	b, err := os.ReadFile(path)
	if err == nil && bytes.Equal(b, key) {
		return owner.Restrict(path)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && !bytes.Equal(b, key) {
		return errors.New("backup integrity key copies disagree")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if err := writeKeyFile(path, key); err != nil {
		if !errors.Is(err, os.ErrExist) {
			return err
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if !bytes.Equal(b, key) {
			return errors.New("backup integrity key copies disagree")
		}
	}
	return owner.Restrict(path)
}

func (s *Store) hasArchives() (bool, error) {
	if s == nil || s.Dir == "" {
		return false, nil
	}
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".zip") && !strings.HasSuffix(name, ".partial") {
			id := strings.TrimSuffix(name, ".zip")
			if validID(id) {
				return true, nil
			}
		}
		if id, ok := quarantineNameID(name); ok && validID(id) {
			return true, nil
		}
	}
	return false, nil
}

func writeKeyFile(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, werr := f.Write(b)
	serr := f.Sync()
	cerr := f.Close()
	if werr != nil {
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(werr, removeErr)
		}
		return werr
	}
	if serr != nil {
		return serr
	}
	return cerr
}

type quarFile struct {
	id   string
	path string
}

func (s *Store) quarantined() ([]quarFile, error) {
	entries, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []quarFile
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		id, ok := quarantineNameID(entry.Name())
		if !ok || !validID(id) {
			continue
		}
		out = append(out, quarFile{id: id, path: filepath.Join(s.Dir, entry.Name())})
	}
	return out, nil
}

func (s *Store) quarantinePaths(id string) ([]string, error) {
	all, err := s.quarantined()
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, q := range all {
		if q.id == id {
			paths = append(paths, q.path)
		}
	}
	return paths, nil
}

func quarantineNameID(name string) (string, bool) {
	const mark = ".zip.quarantine"
	if strings.HasSuffix(name, mark) {
		return strings.TrimSuffix(name, mark), true
	}
	idx := strings.Index(name, mark+".")
	if idx <= 0 {
		return "", false
	}
	rest := name[idx+len(mark)+1:]
	if rest == "" {
		return "", false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return "", false
		}
	}
	return name[:idx], true
}

func (s *Store) quarantine(id string) error {
	if s.hookQuarantine != nil {
		s.hookQuarantine()
	}
	src, err := s.zipPath(id)
	if err != nil {
		return err
	}
	if _, err := os.Stat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	base := src + ".quarantine"
	for i := 0; i < 1000; i++ {
		name := base
		if i > 0 {
			name = fmt.Sprintf("%s.%d", base, i)
		}
		_, err := os.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			if err := renameRetry(src, name); err != nil {
				return err
			}
			return syncDir(s.Dir)
		}
		if err != nil {
			return err
		}
	}
	return errors.New("could not quarantine backup")
}

// SignView is the manifest an operator has to see before Sign will run.
type SignView struct {
	ID             string     `json:"id"`
	Manifest       Manifest   `json:"manifest"`
	Files          []SignFile `json:"files"`
	ManifestSHA256 string     `json:"manifestSHA256"`
	Unsigned       bool       `json:"unsigned"`
	ZipModified    time.Time  `json:"zipModified"`
	KeyCreated     time.Time  `json:"keyCreated,omitempty"`
}

// SignFile is one archived path and its sha256, shown before signing.
type SignFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// PreviewSign reads an archive and returns its manifest and file hashes.
// It does not rewrite the zip.
func (s *Store) PreviewSign(id string) (SignView, error) {
	if s == nil || s.Dir == "" {
		return SignView{}, errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.previewSignLocked(id)
}

// Sign rewrites a hash-valid archive that has an empty HMAC. manifestSHA256
// is the digest PreviewSign showed. A zip newer than the key file is refused.
// A wrong HMAC is not signed. Listing and restore do not call this.
func (s *Store) Sign(id, manifestSHA256 string) error {
	if s == nil || s.Dir == "" {
		return errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	view, err := s.previewSignLocked(id)
	if err != nil {
		return err
	}
	if manifestSHA256 == "" || !strings.EqualFold(manifestSHA256, view.ManifestSHA256) {
		return ErrManifestAck
	}
	if !view.Unsigned {
		return nil
	}
	if err := s.refuseNewerThanKey(id); err != nil {
		return err
	}
	return s.signLocked(id)
}

func (s *Store) previewSignLocked(id string) (SignView, error) {
	if !validID(id) {
		return SignView{}, ErrInvalidID
	}
	path, err := s.zipPath(id)
	if err != nil {
		return SignView{}, err
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return SignView{}, ErrNotFound
	}
	if err != nil {
		return SignView{}, err
	}
	key, err := s.integrityKey()
	if err != nil {
		return SignView{}, err
	}
	a, err := openArchive(path, openOpts{hash: true, mac: true, key: key})
	if err != nil {
		return SignView{}, err
	}
	man := a.man
	unsigned := a.unsigned
	if err := a.Close(); err != nil {
		return SignView{}, err
	}
	if man.ID != id {
		return SignView{}, fmt.Errorf("%w: manifest id does not match the file name", ErrIntegrity)
	}
	digest, err := manifestDigest(man)
	if err != nil {
		return SignView{}, err
	}
	view := SignView{
		ID:             id,
		Manifest:       man,
		ManifestSHA256: digest,
		Unsigned:       unsigned,
		ZipModified:    info.ModTime().UTC(),
	}
	for _, f := range man.Files {
		view.Files = append(view.Files, SignFile{Path: f.Path, SHA256: f.SHA256, Size: f.Size})
	}
	if view.Files == nil {
		view.Files = []SignFile{}
	}
	if unsigned {
		created, err := s.keyCreated()
		if err != nil {
			return SignView{}, err
		}
		view.KeyCreated = created.UTC()
	}
	return view, nil
}

func (s *Store) refuseNewerThanKey(id string) error {
	path, err := s.zipPath(id)
	if err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	created, err := s.keyCreated()
	if err != nil {
		return err
	}
	if info.ModTime().After(created) {
		return ErrTooNew
	}
	return nil
}

func (s *Store) keyCreated() (time.Time, error) {
	var earliest time.Time
	found := false
	for _, path := range s.keyPaths() {
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, err
		}
		if !found || info.ModTime().Before(earliest) {
			earliest = info.ModTime()
			found = true
		}
	}
	if !found {
		return time.Time{}, errors.New("backup integrity key file is required to sign")
	}
	return earliest, nil
}

// Release renames one quarantined archive back to a live zip.
func (s *Store) Release(id string) error {
	if s == nil || s.Dir == "" {
		return errors.New("backup directory is not configured")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validID(id) {
		return ErrInvalidID
	}
	matches, err := s.quarantinePaths(id)
	if err != nil {
		return err
	}
	if len(matches) == 0 {
		if err := s.Exists(id); err == nil {
			return ErrNotQuarantined
		} else if errors.Is(err, ErrNotFound) {
			return ErrNotFound
		} else if err != nil {
			return err
		}
		return ErrNotFound
	}
	if len(matches) != 1 {
		return ErrAmbiguousQuarantine
	}
	dest, err := s.zipPath(id)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return ErrLiveBackup
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := renameRetry(matches[0], dest); err != nil {
		return err
	}
	s.dropCache(id)
	return syncDir(s.Dir)
}

func (s *Store) signLocked(id string) error {
	path, err := s.zipPath(id)
	if err != nil {
		return err
	}
	key, err := s.integrityKey()
	if err != nil {
		return err
	}
	a, err := openArchive(path, openOpts{hash: true, mac: true, key: key})
	if err != nil {
		return err
	}
	if !a.unsigned {
		return a.Close()
	}
	if a.man.ID != id {
		err := fmt.Errorf("%w: manifest id does not match the file name", ErrIntegrity)
		if closeErr := a.Close(); closeErr != nil {
			return errors.Join(err, closeErr)
		}
		return err
	}
	tmp, err := os.MkdirTemp(s.Dir, ".gmanager-migrate-*")
	if err != nil {
		if closeErr := a.Close(); closeErr != nil {
			return errors.Join(err, closeErr)
		}
		return err
	}
	defer func() {
		if rmErr := os.RemoveAll(tmp); rmErr != nil {
			return
		}
	}()
	var files []sourceFile
	for _, f := range a.man.Files {
		dest := filepath.Join(tmp, filepath.FromSlash(f.Path))
		if err := within(tmp, dest); err != nil {
			if closeErr := a.Close(); closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return err
		}
		if err := extractChecked(a, f, dest); err != nil {
			if closeErr := a.Close(); closeErr != nil {
				return errors.Join(err, closeErr)
			}
			return err
		}
		files = append(files, sourceFile{abs: dest, entry: f})
	}
	man := a.man
	man.HMAC = ""
	if err := a.Close(); err != nil {
		return err
	}
	partial := path + ".partial"
	if err := os.Remove(partial); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if _, err := writeZip(partial, man, files, key); err != nil {
		if removeErr := os.Remove(partial); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(err, removeErr)
		}
		return err
	}
	if err := renameRetry(partial, path); err != nil {
		if removeErr := os.Remove(partial); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return errors.Join(err, removeErr)
		}
		return err
	}
	s.dropCache(id)
	return syncDir(s.Dir)
}

func entryFrom(id string, info os.FileInfo, man Manifest) Entry {
	return Entry{
		Cursor:     id,
		ID:         id,
		CreatedAt:  man.CreatedAt.UTC(),
		Bytes:      info.Size(),
		FileCount:  len(man.Files),
		Kind:       man.Kind,
		InstanceID: man.InstanceID,
	}
}

func validKind(kind string) bool {
	switch kind {
	case KindManual, KindUpdate, KindSafety:
		return true
	default:
		return false
	}
}

func addSize(total, n uint64) (uint64, error) {
	if total > ^uint64(0)-n {
		return 0, errors.New("backup is too large")
	}
	return total + n, nil
}

func spaceNeed(total uint64, files int) (uint64, error) {
	if files < 0 {
		return 0, errors.New("negative file count")
	}
	extra := uint64(files)*64 + 64*1024
	margin := total / 50
	if total > ^uint64(0)-extra || total+extra > ^uint64(0)-margin {
		return 0, errors.New("backup is too large")
	}
	return total + margin + extra, nil
}

// copyFile is used by restore when a hashed stream is written to a temp file.
func copyToFile(dest string, r io.Reader) (string, int64, error) {
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", 0, err
	}
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return "", 0, err
	}
	sum, n, copyErr := hashReader(io.TeeReader(r, out))
	if copyErr == nil {
		copyErr = out.Sync()
	}
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		if removeErr := os.Remove(dest); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return "", 0, errors.Join(copyErr, removeErr)
		}
		return "", 0, copyErr
	}
	return sum, n, nil
}
