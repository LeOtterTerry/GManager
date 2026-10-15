package backup

import (
	"archive/zip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// Zip-bomb caps. Tests lower them and restore the previous value.
var (
	maxZipEntries    = 100_000
	maxZipBytes      = uint64(32 << 30)
	maxZipFileBytes  = uint64(8 << 30)
	maxZipRatio      = uint64(1000)
	maxZipRatioFloor = uint64(64 << 20)
)

// Manifest is the integrity record stored inside every backup zip.
// Hashes are sha256 of the uncompressed file bytes. The manifest does not
// contain file contents, so a server password in server.json is not copied
// into backup metadata.
type Manifest struct {
	Version    int            `json:"version"`
	ID         string         `json:"id"`
	CreatedAt  time.Time      `json:"createdAt"`
	InstanceID string         `json:"instanceId"`
	GameID     string         `json:"gameId"`
	Kind       string         `json:"kind"`
	Saves      []SaveRoot     `json:"saves"`
	Files      []ManifestFile `json:"files"`
	HMAC       string         `json:"hmac,omitempty"`
}

// SaveRoot is one directory from GameModule.SavePaths, in that order.
type SaveRoot struct {
	Index int `json:"index"`
}

// ManifestFile is one regular file in the zip.
type ManifestFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
	Role   string `json:"role"`
	Root   int    `json:"root,omitempty"`
	Rel    string `json:"rel"`
}

type sourceFile struct {
	abs   string
	entry ManifestFile
}

type archive struct {
	man      Manifest
	files    map[string]*zip.File
	z        *zip.ReadCloser
	unsigned bool
}

func (a *archive) Close() error {
	if a == nil || a.z == nil {
		return nil
	}
	return a.z.Close()
}

func writeZip(path string, man Manifest, files []sourceFile, key []byte) (Manifest, error) {
	// 0600 because the zip contains server.json, including the server password.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return Manifest{}, err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		if closeErr := f.Close(); closeErr != nil {
			return
		}
	}()
	zw := zip.NewWriter(f)
	man.Files = make([]ManifestFile, 0, len(files))
	for _, src := range files {
		entry, err := addFile(zw, src)
		if err != nil {
			if closeErr := zw.Close(); closeErr != nil {
				return Manifest{}, errors.Join(err, closeErr)
			}
			return Manifest{}, err
		}
		man.Files = append(man.Files, entry)
	}
	man, err = sealManifest(man, key)
	if err != nil {
		if closeErr := zw.Close(); closeErr != nil {
			return Manifest{}, errors.Join(err, closeErr)
		}
		return Manifest{}, err
	}
	raw, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		if closeErr := zw.Close(); closeErr != nil {
			return Manifest{}, errors.Join(err, closeErr)
		}
		return Manifest{}, err
	}
	raw = append(raw, '\n')
	hdr := &zip.FileHeader{Name: manifestName, Method: zip.Deflate, Modified: man.CreatedAt.UTC()}
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		if closeErr := zw.Close(); closeErr != nil {
			return Manifest{}, errors.Join(err, closeErr)
		}
		return Manifest{}, err
	}
	if _, err := w.Write(raw); err != nil {
		if closeErr := zw.Close(); closeErr != nil {
			return Manifest{}, errors.Join(err, closeErr)
		}
		return Manifest{}, err
	}
	if err := zw.Close(); err != nil {
		return Manifest{}, err
	}
	if err := f.Sync(); err != nil {
		return Manifest{}, err
	}
	if err := f.Close(); err != nil {
		return Manifest{}, err
	}
	committed = true
	return man, nil
}

func addFile(zw *zip.Writer, src sourceFile) (ManifestFile, error) {
	in, err := os.Open(src.abs)
	if err != nil {
		return ManifestFile{}, err
	}
	defer func() {
		if closeErr := in.Close(); closeErr != nil {
			return
		}
	}()
	info, err := in.Stat()
	if err != nil {
		return ManifestFile{}, err
	}
	if !info.Mode().IsRegular() {
		return ManifestFile{}, fmt.Errorf("not a regular file: %s", src.entry.Path)
	}
	hdr := &zip.FileHeader{Name: src.entry.Path, Method: zip.Deflate, Modified: info.ModTime().UTC()}
	w, err := zw.CreateHeader(hdr)
	if err != nil {
		return ManifestFile{}, err
	}
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, sum), in)
	if err != nil {
		return ManifestFile{}, err
	}
	if n != info.Size() {
		return ManifestFile{}, fmt.Errorf("file changed while it was read: %s", src.entry.Path)
	}
	src.entry.SHA256 = hex.EncodeToString(sum.Sum(nil))
	src.entry.Size = n
	return src.entry, nil
}

type openOpts struct {
	hash bool
	mac  bool
	key  []byte
}

func openArchive(path string, opt openOpts) (*archive, error) {
	z, err := zip.OpenReader(path)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrIntegrity, err)
	}
	if err := rejectBomb(z.File); err != nil {
		if closeErr := z.Close(); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	a := &archive{files: map[string]*zip.File{}, z: z}
	var manifest *zip.File
	for _, f := range z.File {
		if f.FileInfo().IsDir() {
			if closeErr := a.Close(); closeErr != nil {
				return nil, errors.Join(fmt.Errorf("%w: unexpected directory %s", ErrIntegrity, f.Name), closeErr)
			}
			return nil, fmt.Errorf("%w: unexpected directory %s", ErrIntegrity, f.Name)
		}
		if f.Name == manifestName {
			if manifest != nil {
				if closeErr := a.Close(); closeErr != nil {
					return nil, errors.Join(fmt.Errorf("%w: duplicate manifest", ErrIntegrity), closeErr)
				}
				return nil, fmt.Errorf("%w: duplicate manifest", ErrIntegrity)
			}
			manifest = f
			continue
		}
		if _, ok := a.files[f.Name]; ok {
			if closeErr := a.Close(); closeErr != nil {
				return nil, errors.Join(fmt.Errorf("%w: duplicate %s", ErrIntegrity, f.Name), closeErr)
			}
			return nil, fmt.Errorf("%w: duplicate %s", ErrIntegrity, f.Name)
		}
		a.files[f.Name] = f
	}
	if manifest == nil {
		if closeErr := a.Close(); closeErr != nil {
			return nil, errors.Join(fmt.Errorf("%w: manifest missing", ErrIntegrity), closeErr)
		}
		return nil, fmt.Errorf("%w: manifest missing", ErrIntegrity)
	}
	if err := readManifest(manifest, &a.man); err != nil {
		if closeErr := a.Close(); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	if opt.mac {
		err := checkManifestMAC(a.man, opt.key)
		if errors.Is(err, ErrUnsigned) {
			a.unsigned = true
		} else if err != nil {
			if closeErr := a.Close(); closeErr != nil {
				return nil, errors.Join(err, closeErr)
			}
			return nil, err
		}
	}
	if err := checkManifest(a.man, a.files, opt.hash); err != nil {
		if closeErr := a.Close(); closeErr != nil {
			return nil, errors.Join(err, closeErr)
		}
		return nil, err
	}
	return a, nil
}

func readManifest(f *zip.File, man *Manifest) error {
	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("%w: manifest: %v", ErrIntegrity, err)
	}
	defer func() {
		if closeErr := rc.Close(); closeErr != nil {
			return
		}
	}()
	dec := json.NewDecoder(io.LimitReader(rc, 32<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(man); err != nil {
		return fmt.Errorf("%w: manifest: %v", ErrIntegrity, err)
	}
	return nil
}

func checkManifest(man Manifest, files map[string]*zip.File, hash bool) error {
	if man.Version != manifestVer {
		return fmt.Errorf("%w: manifest version %d", ErrIntegrity, man.Version)
	}
	if !validID(man.ID) {
		return fmt.Errorf("%w: manifest id", ErrIntegrity)
	}
	if !validKind(man.Kind) {
		return fmt.Errorf("%w: manifest kind", ErrIntegrity)
	}
	if err := checkSaves(man); err != nil {
		return err
	}
	seen := map[string]struct{}{}
	folded := map[string]string{}
	for _, f := range man.Files {
		expect, err := zipPathFor(f.Role, f.Rel, f.Root)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrIntegrity, err)
		}
		if f.Path != expect {
			return fmt.Errorf("%w: path %s", ErrIntegrity, f.Path)
		}
		fold := strings.ToLower(f.Path)
		if prev, ok := folded[fold]; ok && prev != f.Path {
			return fmt.Errorf("%w: case-colliding paths %s and %s", ErrIntegrity, prev, f.Path)
		}
		folded[fold] = f.Path
		if f.Role == roleSave && (f.Root < 0 || f.Root >= len(man.Saves)) {
			return fmt.Errorf("%w: save index for %s", ErrIntegrity, f.Path)
		}
		if f.Size < 0 {
			return fmt.Errorf("%w: negative size %s", ErrIntegrity, f.Path)
		}
		zf, ok := files[f.Path]
		if !ok {
			return fmt.Errorf("%w: missing %s", ErrIntegrity, f.Path)
		}
		if hash {
			sum, n, err := hashZip(zf)
			if err != nil {
				return fmt.Errorf("%w: %s: %v", ErrIntegrity, f.Path, err)
			}
			if sum != f.SHA256 || n != f.Size {
				return fmt.Errorf("%w: %s", ErrIntegrity, f.Path)
			}
		}
		seen[f.Path] = struct{}{}
	}
	for name := range files {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("%w: unexpected %s", ErrIntegrity, name)
		}
	}
	return nil
}

func checkSaves(man Manifest) error {
	if len(man.Saves) == 0 {
		return fmt.Errorf("%w: no save directory", ErrIntegrity)
	}
	seen := map[int]struct{}{}
	for _, root := range man.Saves {
		if root.Index < 0 || root.Index >= len(man.Saves) {
			return fmt.Errorf("%w: save index %d", ErrIntegrity, root.Index)
		}
		if _, ok := seen[root.Index]; ok {
			return fmt.Errorf("%w: duplicate save index %d", ErrIntegrity, root.Index)
		}
		seen[root.Index] = struct{}{}
	}
	return nil
}

func hashZip(f *zip.File) (string, int64, error) {
	if f.UncompressedSize64 > maxZipFileBytes {
		return "", 0, errors.New("file too large")
	}
	rc, err := f.Open()
	if err != nil {
		return "", 0, err
	}
	defer func() {
		if closeErr := rc.Close(); closeErr != nil {
			return
		}
	}()
	limit := int64(f.UncompressedSize64) + 1
	return hashReader(io.LimitReader(rc, limit))
}

func sealManifest(man Manifest, key []byte) (Manifest, error) {
	sum, err := macManifest(man, key)
	if err != nil {
		return Manifest{}, err
	}
	man.HMAC = sum
	return man, nil
}

func macManifest(man Manifest, key []byte) (string, error) {
	if len(key) < 32 {
		return "", errors.New("backup integrity key is not configured")
	}
	man.HMAC = ""
	raw, err := json.Marshal(man)
	if err != nil {
		return "", err
	}
	m := hmac.New(sha256.New, key)
	if _, err := m.Write(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(m.Sum(nil)), nil
}

func manifestDigest(man Manifest) (string, error) {
	man.HMAC = ""
	raw, err := json.Marshal(man)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func checkManifestMAC(man Manifest, key []byte) error {
	if man.HMAC == "" {
		// Version 1 archives written before the key existed have no hmac.
		// Callers may hash-check them and then sign them in place.
		return ErrUnsigned
	}
	sum, err := macManifest(man, key)
	if err != nil {
		return err
	}
	if !hmac.Equal([]byte(sum), []byte(man.HMAC)) {
		return fmt.Errorf("%w: manifest hmac", ErrIntegrity)
	}
	return nil
}

func rejectBomb(files []*zip.File) error {
	if len(files) > maxZipEntries {
		return fmt.Errorf("%w: too many entries", ErrIntegrity)
	}
	var raw, packed uint64
	for _, f := range files {
		if f.UncompressedSize64 > maxZipFileBytes {
			return fmt.Errorf("%w: %s is too large", ErrIntegrity, f.Name)
		}
		next, err := addSize(raw, f.UncompressedSize64)
		if err != nil || next > maxZipBytes {
			return fmt.Errorf("%w: uncompressed size", ErrIntegrity)
		}
		raw = next
		nextPacked, err := addSize(packed, f.CompressedSize64)
		if err != nil {
			return fmt.Errorf("%w: compressed size", ErrIntegrity)
		}
		packed = nextPacked
	}
	if raw > maxZipRatioFloor && packed > 0 && raw/packed > maxZipRatio {
		return fmt.Errorf("%w: compression ratio", ErrIntegrity)
	}
	return nil
}

func hashReader(r io.Reader) (string, int64, error) {
	sum := sha256.New()
	n, err := io.Copy(sum, r)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(sum.Sum(nil)), n, nil
}
