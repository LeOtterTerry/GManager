//go:build unix

package config

import (
	"errors"
	"os"
	"syscall"
)

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	syncErr := f.Sync()
	closeErr := f.Close()
	if syncErr != nil && !syncUnsupported(syncErr) {
		return syncErr
	}
	return closeErr
}

func syncUnsupported(err error) bool {
	return errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOTSUP) || errors.Is(err, syscall.EOPNOTSUPP)
}
