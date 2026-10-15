//go:build !windows

package main

import (
	"os"

	"github.com/LeOtterTerry/GManager/agent/internal/owner"
)

func restrictToOwner(path string) error {
	return owner.Restrict(path)
}

func tokenOpenToOthers(path string) (bool, error) {
	info, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return info.Mode().Perm()&0o077 != 0, nil
}
