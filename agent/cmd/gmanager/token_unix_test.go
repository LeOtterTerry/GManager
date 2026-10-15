//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestExistingTokenTightenedOrRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	body := strings.Repeat("a", MinTokenLength) + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GMANAGER_TOKEN", "")
	if _, err := ResolveToken(dir); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}

	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	orig := restrictTokenFile
	restrictTokenFile = func(string) error { return errors.New("acl") }
	t.Cleanup(func() { restrictTokenFile = orig })
	if _, err := ResolveToken(dir); err == nil {
		t.Fatal("open token file was accepted when it could not be restricted")
	}
}
