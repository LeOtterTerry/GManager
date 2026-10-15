//go:build !windows

package main

import "os"

func restrictToOwner(path string) error {
	return os.Chmod(path, 0o600)
}
