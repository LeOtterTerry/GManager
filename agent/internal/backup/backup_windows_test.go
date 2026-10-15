//go:build windows

package backup

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func TestWindowsBackupRestoreLongPath(t *testing.T) {
	base := t.TempDir()
	deep := base
	for len(deep) < 280 {
		deep = filepath.Join(deep, "nest")
	}
	save := filepath.Join(deep, "saves")
	instDir := filepath.Join(base, "server")
	if err := os.MkdirAll(filepath.Join(instDir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(save, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]string{
		"serverName": "kept",
		"password":   "placeholder-password",
		"userdir":    save,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "configs", "server.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "round-trip")
	store := &Store{Dir: filepath.Join(base, "backups"), Keep: 2, Key: testKey()}
	inst := core.Instance{ID: "default", Dir: instDir, UserDir: save}
	mod := plainsofpain.New()
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "changed")
	if err := store.Restore(context.Background(), entry.ID, inst, mod); err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "world.dat"); got != "round-trip" {
		t.Fatalf("world = %q", got)
	}
}

func TestWindowsRestoreLockedSaveFile(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 2, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	locked := filepath.Join(save, "world.dat")
	path, err := windows.UTF16PtrFromString(locked)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(path, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- store.Restore(context.Background(), entry.ID, inst, mod)
	}()
	select {
	case err := <-done:
		t.Fatalf("restore finished while the save file was still locked: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "world.dat"); got != "alpha" {
		t.Fatalf("world = %q", got)
	}
}

func TestWindowsCaseCollidingZipEntries(t *testing.T) {
	TestCaseCollidingZipEntries(t)
}
