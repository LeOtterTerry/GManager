// Package backup writes timestamped zip archives of a module's save
// directories and config files. Each zip carries a sha256 manifest signed
// with an HMAC key that stays outside the zip.
// The caller must already have stopped the game process. This package does
// not invent a save path: GameModule.SavePaths decides that, and an unknown
// path fails the backup.
package backup

import (
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
	// directory. It is created on first use when it is missing.
	KeyPath string

	keyMu     sync.Mutex
	loadedKey []byte
	// swapFail, when set, runs before each live swap during restore.
	// Tests use it to force a partial swap. Production leaves it nil.
	swapFail func() error
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
			seenZip[zipName] = struct{}{}
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
		seenZip[zipName] = struct{}{}
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
		if errors.Is(zipErr, os.ErrNotExist) && errors.Is(partErr, os.ErrNotExist) {
			return id, nil
		}
		if zipErr != nil && !errors.Is(zipErr, os.ErrNotExist) {
			return "", zipErr
		}
		if partErr != nil && !errors.Is(partErr, os.ErrNotExist) {
			return "", partErr
		}
	}
	return "", errors.New("could not allocate a backup id")
}

type zipMeta struct {
	id       string
	kind     string
	verified bool
}

func (s *Store) prune(protect []string) error {
	inv, err := s.inventory()
	if err != nil {
		return err
	}
	keepN := s.keepN()
	keep := map[string]struct{}{}
	byKind := map[string][]zipMeta{}
	for _, z := range inv {
		byKind[z.kind] = append(byKind[z.kind], z)
	}
	for kind, group := range byKind {
		if kind == "" {
			continue
		}
		start := 0
		if len(group) > keepN {
			start = len(group) - keepN
		}
		for _, z := range group[start:] {
			keep[z.id] = struct{}{}
		}
	}
	for _, id := range protect {
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
		path, err := s.zipPath(z.id)
		if err != nil {
			return err
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (s *Store) inventory() ([]zipMeta, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	key, err := s.integrityKey()
	if err != nil {
		return nil, err
	}
	out := make([]zipMeta, 0, len(ids))
	for _, id := range ids {
		path, err := s.zipPath(id)
		if err != nil {
			return nil, err
		}
		meta := zipMeta{id: id}
		a, err := openArchive(path, openOpts{hash: true, mac: true, key: key})
		if err != nil {
			out = append(out, meta)
			continue
		}
		meta.kind = a.man.Kind
		meta.verified = a.man.ID == id
		if closeErr := a.Close(); closeErr != nil {
			meta.verified = false
		}
		out = append(out, meta)
	}
	return out, nil
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
	if err := s.sweepPartial(); err != nil {
		return nil, err
	}
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	key, err := s.integrityKey()
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, id := range ids {
		path, err := s.zipPath(id)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, err
		}
		a, err := openArchive(path, openOpts{hash: false, mac: true, key: key})
		if err != nil {
			out = append(out, corruptEntry(id, info))
			continue
		}
		man := a.man
		closeErr := a.Close()
		if closeErr != nil || man.ID != id {
			out = append(out, corruptEntry(id, info))
			continue
		}
		out = append(out, entryFrom(id, info, man))
	}
	return out, nil
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
		return s.Key, nil
	}
	s.keyMu.Lock()
	defer s.keyMu.Unlock()
	if len(s.loadedKey) >= 32 {
		return append([]byte(nil), s.loadedKey...), nil
	}
	if s.KeyPath == "" {
		return nil, errors.New("backup integrity key is not configured")
	}
	b, err := os.ReadFile(s.KeyPath)
	if errors.Is(err, os.ErrNotExist) {
		b = make([]byte, 32)
		if _, err := rand.Read(b); err != nil {
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(s.KeyPath), 0o755); err != nil {
			return nil, err
		}
		if err := writeKeyFile(s.KeyPath, b); err != nil {
			if !errors.Is(err, os.ErrExist) {
				return nil, err
			}
			b, err = os.ReadFile(s.KeyPath)
			if err != nil {
				return nil, err
			}
		}
	} else if err != nil {
		return nil, err
	}
	if len(b) < 32 {
		return nil, errors.New("backup integrity key is too short")
	}
	s.loadedKey = append([]byte(nil), b...)
	return b, nil
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
