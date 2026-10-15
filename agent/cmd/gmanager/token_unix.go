//go:build !windows

package main

import "os"

func restrictToOwner(path string) error {
	return os.Chmod(path, 0o600)
}

func tokenOpenToOthers(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 != 0, nil
}
