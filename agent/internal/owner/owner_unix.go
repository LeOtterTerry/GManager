//go:build !windows

package owner

import "os"

// Restrict leaves the file readable and writable by its owner only.
func Restrict(path string) error {
	return os.Chmod(path, 0o600)
}
