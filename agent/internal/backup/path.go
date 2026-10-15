package backup

import (
	"errors"
	"fmt"
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
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return fmt.Errorf("invalid path segment %q", part)
		}
	}
	return nil
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
