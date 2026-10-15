//go:build windows

package supervisor

import "golang.org/x/sys/windows"

func init() {
	diskFree = windowsDiskFree
}

func windowsDiskFree(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &free, nil, nil); err != nil {
		return 0, err
	}
	return free, nil
}
