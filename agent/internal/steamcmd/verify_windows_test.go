//go:build windows

package steamcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWindowsVerifyRejectsUnsigned(t *testing.T) {
	path := filepath.Join(t.TempDir(), "steamcmd.exe")
	if err := os.WriteFile(path, []byte("MZ"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := defaultVerify(path); err == nil {
		t.Fatal("unsigned steamcmd.exe was accepted")
	}
}
