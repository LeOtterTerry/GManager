//go:build windows

package backup

// syncDir is a no-op on Windows. renameOnce uses MOVEFILE_WRITE_THROUGH,
// which flushes the metadata a directory sync would flush elsewhere.
func syncDir(string) error { return nil }
