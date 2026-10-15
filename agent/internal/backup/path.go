package backup

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	roleSave   = "save"
	roleConfig = "config"

	manifestName = "manifest.json"
	manifestVer  = 1
)

// idPattern matches the timestamped ids allocateID writes.
// A numeric suffix is zero-padded so lexicographic order matches creation order.
var idPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z(?:-[0-9]{3})?$`)

func validID(id string) bool {
	return idPattern.MatchString(id)
}

func validRel(rel string) error {
	if rel == "" || rel == "." {
		return errors.New("empty relative path")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return errors.New("absolute path")
	}
	if strings.Contains(rel, `\`) {
		return errors.New("backslash in relative path")
	}
	// A colon is a drive letter or an NTFS alternate data stream.
	if strings.ContainsRune(rel, ':') {
		return errors.New("colon in relative path")
	}
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path segment %q", part)
		}
		if strings.HasSuffix(part, ".") || strings.HasSuffix(part, " ") {
			return fmt.Errorf("trailing dot or space in %q", part)
		}
		if reservedName(part) {
			return fmt.Errorf("reserved name %q", part)
		}
	}
	return nil
}

// reservedName reports Windows device names. The check uses the text before
// the first dot, which is how those names are reserved (CON.txt as well as CON).
func reservedName(part string) bool {
	name := part
	if i := strings.IndexByte(part, '.'); i >= 0 {
		name = part[:i]
	}
	switch strings.ToLower(name) {
	case "con", "prn", "aux", "nul",
		"com1", "com2", "com3", "com4", "com5", "com6", "com7", "com8", "com9",
		"lpt1", "lpt2", "lpt3", "lpt4", "lpt5", "lpt6", "lpt7", "lpt8", "lpt9":
		return true
	default:
		return false
	}
}

func zipPathFor(role, rel string, root int) (string, error) {
	if err := validRel(rel); err != nil {
		return "", err
	}
	switch role {
	case roleSave:
		if root < 0 {
			return "", errors.New("negative save index")
		}
		return "saves/" + strconv.Itoa(root) + "/" + rel, nil
	case roleConfig:
		return "config/" + rel, nil
	default:
		return "", errors.New("unknown backup role")
	}
}

func normalize(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return abs, nil
}

func sameOrInside(parent, child string) bool {
	sep := string(filepath.Separator)
	parent = strings.TrimRight(parent, sep)
	child = strings.TrimRight(child, sep)
	if parent == child {
		return true
	}
	return strings.HasPrefix(child, parent+sep)
}

func isVolumeRoot(path string) bool {
	clean := filepath.Clean(path)
	vol := filepath.VolumeName(clean)
	if vol == "" {
		return clean == string(filepath.Separator)
	}
	rest := strings.TrimPrefix(clean, vol)
	rest = strings.Trim(rest, `/\`)
	return rest == ""
}

func guardSaveRoot(instDir, backupDir, save string) error {
	if isVolumeRoot(save) {
		return fmt.Errorf("save path %s is a volume root", save)
	}
	inst, err := normalize(instDir)
	if err != nil {
		return err
	}
	root, err := normalize(save)
	if err != nil {
		return err
	}
	back, err := normalize(backupDir)
	if err != nil {
		return err
	}
	if sameOrInside(root, inst) {
		return errors.New("save path contains the server directory")
	}
	if sameOrInside(root, back) || sameOrInside(back, root) {
		return errors.New("save path overlaps the backup directory")
	}
	return nil
}

// ErrBackupDir means the backup directory failed validation.
var ErrBackupDir = errors.New("invalid backup directory")

// ErrUnsafeDest means a restore would overwrite a non-empty directory that
// is not the save folder currently configured for this server.
var ErrUnsafeDest = errors.New("refusing to overwrite a non-empty directory that is not the save folder")

// ValidateDir requires an absolute backup directory that is not a UNC share
// and does not overlap the server directory. The agent does not choose a
// drive letter on its own.
func ValidateDir(backupDir, serverDir string) error {
	if strings.TrimSpace(backupDir) == "" {
		return fmt.Errorf("%w: empty", ErrBackupDir)
	}
	if isUNC(backupDir) {
		return fmt.Errorf("%w: UNC paths are not allowed", ErrBackupDir)
	}
	if !filepath.IsAbs(backupDir) {
		return fmt.Errorf("%w: must be absolute", ErrBackupDir)
	}
	if serverDir == "" {
		return nil
	}
	back, err := normalize(backupDir)
	if err != nil {
		return err
	}
	inst, err := normalize(serverDir)
	if err != nil {
		return err
	}
	if sameOrInside(inst, back) {
		return fmt.Errorf("%w: backup directory is inside the server directory", ErrBackupDir)
	}
	if sameOrInside(back, inst) {
		return fmt.Errorf("%w: server directory is inside the backup directory", ErrBackupDir)
	}
	return nil
}

func isUNC(path string) bool {
	if strings.HasPrefix(path, `\\`) || strings.HasPrefix(path, `//`) {
		return true
	}
	vol := filepath.VolumeName(path)
	return strings.HasPrefix(vol, `\\`) || strings.HasPrefix(vol, `//`)
}

// SamePath reports whether two paths name the same place.
// Comparison is case-insensitive on Windows. A missing path is not an error.
func SamePath(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	na, err := normalize(a)
	if err != nil {
		return false
	}
	nb, err := normalize(b)
	if err != nil {
		return false
	}
	return na == nb
}

// RefuseForeign rejects a directory that already holds files and is not the
// save folder the operator currently has configured. A missing directory and
// an empty directory are allowed. Callers use this before they retarget a
// restore at the path named in an older server.json.
func RefuseForeign(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%w: %s is a symlink", ErrUnsafeDest, path)
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: %s is not a directory", ErrUnsafeDest, path)
	}
	empty, err := dirEmpty(path)
	if err != nil {
		return err
	}
	if !empty {
		return fmt.Errorf("%w: %s", ErrUnsafeDest, path)
	}
	return nil
}

func dirEmpty(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	names, readErr := f.Readdirnames(1)
	closeErr := f.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		if closeErr != nil {
			return false, errors.Join(readErr, closeErr)
		}
		return false, readErr
	}
	if closeErr != nil {
		return false, closeErr
	}
	return len(names) == 0, nil
}

func within(root, target string) error {
	rootN, err := normalize(root)
	if err != nil {
		return err
	}
	targetN, err := normalize(target)
	if err != nil {
		return err
	}
	if rootN == targetN || !sameOrInside(rootN, targetN) {
		return fmt.Errorf("path %s escapes %s", target, root)
	}
	return nil
}
