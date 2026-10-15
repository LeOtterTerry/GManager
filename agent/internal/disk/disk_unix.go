//go:build unix

package disk

import "syscall"

func platformFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bsize) * uint64(st.Bavail), nil
}
