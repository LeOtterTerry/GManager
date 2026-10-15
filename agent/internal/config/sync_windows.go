//go:build windows

package config

// syncDir is a no-op on Windows. The replace itself is flushed by
// MoveFileEx(MOVEFILE_WRITE_THROUGH) in moveFileReplace.
func syncDir(string) error { return nil }
