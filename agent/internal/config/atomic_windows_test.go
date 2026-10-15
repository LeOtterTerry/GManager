//go:build windows

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMoveFileReplaceFlushesOverExisting(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "server.json")
	src := filepath.Join(dir, "server.json.tmp-test")
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := moveFileReplace(src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("dst = %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
}
