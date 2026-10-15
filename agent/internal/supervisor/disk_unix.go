//go:build unix

package supervisor

import "syscall"

func init() {
	diskFree = unixDiskFree
}

func unixDiskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bsize) * uint64(st.Bavail), nil
}
