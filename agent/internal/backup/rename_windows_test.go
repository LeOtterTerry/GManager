//go:build windows

package backup

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestRenameRetriesWhileFileIsLocked(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.dat")
	dst := filepath.Join(dir, "dst.dat")
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	path, err := windows.UTF16PtrFromString(dst)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- renameRetry(src, dst)
	}()
	time.Sleep(30 * time.Millisecond)
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("dst = %q", b)
	}
}

func TestRenameCaseOnlyCollision(t *testing.T) {
	dir := t.TempDir()
	existing := filepath.Join(dir, "World.dat")
	src := filepath.Join(dir, "staged.dat")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "world.dat")
	if err := renameRetry(src, target); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(existing)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "new" {
		t.Fatalf("content = %q", b)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("files = %d", n)
	}
}

func TestRenameLongPath(t *testing.T) {
	deep := t.TempDir()
	for len(deep) < 280 {
		deep = filepath.Join(deep, "nest")
	}
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(deep, "src.dat")
	dst := filepath.Join(deep, "dst.dat")
	if err := os.WriteFile(src, []byte("long"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := renameRetry(src, dst); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "long" {
		t.Fatalf("dst = %q", b)
	}
}
