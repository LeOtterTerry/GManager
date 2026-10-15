package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
)

// diskFree reports bytes available to the caller on the volume holding path.
// Platform files replace it. Tests replace it to simulate a full disk.
var diskFree = func(string) (uint64, error) {
	return 0, fmt.Errorf("free space is unavailable")
}

func hasFreeSpace(path string, need uint64) error {
	free, err := diskFree(existingDir(path))
	if err != nil {
		return err
	}
	if free < need {
		return fmt.Errorf("need %d bytes, %d free", need, free)
	}
	return nil
}

func existingDir(path string) string {
	for {
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return path
		}
		path = parent
	}
}

func treeBytes(root string) (uint64, error) {
	var total uint64
	err := filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info != nil && info.Mode().IsRegular() {
			total += uint64(info.Size())
		}
		return nil
	})
	return total, err
}
