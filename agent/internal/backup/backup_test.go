package backup

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func testKey() []byte {
	return bytes.Repeat([]byte{0x11}, 32)
}

func TestSuggestAndDefaultDir(t *testing.T) {
	got, ok := SuggestDir(func(string) bool { return true })
	if !ok || got != SuggestedDir {
		t.Fatalf("suggest = %q %v", got, ok)
	}
	if _, ok := SuggestDir(func(string) bool { return false }); ok {
		t.Fatal("missing P: was treated as a suggestion")
	}
	if DefaultDir("data") != filepath.Join("data", "backups") {
		t.Fatalf("default = %s", DefaultDir("data"))
	}
	if DefaultDir("data") == SuggestedDir {
		t.Fatal("default selected the P: suggestion")
	}
	if !VolumeExists(t.TempDir()) {
		t.Fatal("temp dir was not a volume")
	}
	if VolumeExists(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("missing path existed")
	}
}

func TestRoundTripRetentionAndCursor(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "slot/world.dat", "alpha")
	writeSave(t, save, "notes/empty.dat", "")
	clock := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	store := &Store{
		Dir:        filepath.Join(t.TempDir(), "backups"),
		JournalDir: t.TempDir(),
		Keep:       10,
		Key:        testKey(),
		Now: func() time.Time {
			clock = clock.Add(time.Millisecond)
			return clock
		},
	}
	ctx := context.Background()
	var ids []string
	for i := 0; i < 3; i++ {
		if i == 1 {
			writeSave(t, save, "slot/world.dat", "beta")
			writeSave(t, save, "slot/extra.dat", "gone-later")
		}
		entry, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
		if err != nil {
			t.Fatal(err)
		}
		if entry.ID == "" || entry.Cursor != entry.ID || entry.FileCount < 2 || entry.Kind != KindManual {
			t.Fatalf("entry = %+v", entry)
		}
		ids = append(ids, entry.ID)
	}
	page, next, err := store.List("", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].ID != ids[0] || next != ids[0] {
		t.Fatalf("page = %+v next = %s", page, next)
	}
	page, next, err = store.List(next, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != ids[1] || page[1].ID != ids[2] || next != ids[2] {
		t.Fatalf("rest = %+v next = %s", page, next)
	}
	empty, next, err := store.List(next, 10)
	if err != nil || len(empty) != 0 || next != ids[2] {
		t.Fatalf("tail = %+v %s %v", empty, next, err)
	}
	if _, _, err := store.List("../nope", 10); !errors.Is(err, ErrInvalidCursor) {
		t.Fatalf("cursor err = %v", err)
	}

	writeSave(t, save, "slot/world.dat", "mutated")
	writeSave(t, save, "slot/extra.dat", "still-here")
	if err := os.WriteFile(filepath.Join(inst.Dir, "configs", "server.json"), []byte("{\"serverName\":\"changed\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, ids[0], inst, mod); err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "slot/world.dat"); got != "alpha" {
		t.Fatalf("world = %q", got)
	}
	if _, err := os.Stat(filepath.Join(save, "slot", "extra.dat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("extra file survived restore: %v", err)
	}
	if readSave(t, save, "notes/empty.dat") != "" {
		t.Fatal("empty file was not restored")
	}
	cfg, err := os.ReadFile(filepath.Join(inst.Dir, "configs", "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cfg, []byte(`"serverName": "kept"`)) && !bytes.Contains(cfg, []byte(`"serverName":"kept"`)) {
		t.Fatalf("config = %s", cfg)
	}
	raw := manifestJSON(t, filepath.Join(store.Dir, ids[0]+".zip"))
	if bytes.Contains(raw, []byte("placeholder-password")) {
		t.Fatal("manifest contains the server password")
	}
	if !bytes.Contains(raw, []byte(`"sha256"`)) {
		t.Fatal("manifest has no sha256")
	}
}

func TestRetentionPoolsKeepManualAndOneVerified(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	clock := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	store := &Store{
		Dir:  filepath.Join(t.TempDir(), "backups"),
		Keep: 1,
		Key:  testKey(),
		Now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
	}
	ctx := context.Background()
	manual, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	update1, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindUpdate})
	if err != nil {
		t.Fatal(err)
	}
	update2, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindUpdate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, manual.ID+".zip")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, update1.ID+".zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("older update err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, update2.ID+".zip")); err != nil {
		t.Fatal(err)
	}

	store.Keep = 2
	older, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	newer, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	corruptZipByte(t, filepath.Join(store.Dir, newer.ID+".zip"), false)
	corruptZipByte(t, filepath.Join(store.Dir, update2.ID+".zip"), false)
	// Bump mtime so a same-size rewrite is not served from the inventory cache.
	bumped := time.Now().Add(2 * time.Second)
	for _, id := range []string{newer.ID, update2.ID} {
		path := filepath.Join(store.Dir, id+".zip")
		if err := os.Chtimes(path, bumped, bumped); err != nil {
			t.Fatal(err)
		}
	}
	store.Keep = 1
	if err := store.prune(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, older.ID+".zip")); err != nil {
		t.Fatalf("only verified manual was deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, newer.ID+".zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unreadable zip err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, newer.ID+".zip.quarantine")); err != nil {
		t.Fatalf("unreadable zip was deleted instead of quarantined: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, update2.ID+".zip.quarantine")); err != nil {
		t.Fatalf("unreadable update was deleted instead of quarantined: %v", err)
	}
	page, _, err := store.List("", 20)
	if err != nil {
		t.Fatal(err)
	}
	var sawQuarantine bool
	for _, item := range page {
		if item.ID == newer.ID && item.Corrupt && item.Quarantined {
			sawQuarantine = true
		}
	}
	if !sawQuarantine {
		t.Fatalf("page = %+v", page)
	}
}

func TestFreeSpaceLeavesNoPartial(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	dir := filepath.Join(t.TempDir(), "backups")
	store := &Store{
		Dir:  dir,
		Keep: 5,
		Key:  testKey(),
		Free: func(string) (uint64, error) { return 1, nil },
	}
	_, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if !errors.Is(err, ErrNoSpace) {
		t.Fatalf("err = %v", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("leftovers = %d", len(entries))
	}
}

func TestTamperDoesNotRestore(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "good")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	tamperZip(t, filepath.Join(store.Dir, entry.ID+".zip"))
	err = store.Restore(context.Background(), entry.ID, inst, mod)
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
	if err := store.Verify(entry.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("verify = %v", err)
	}
}

func TestUnknownSavePathAndOverlap(t *testing.T) {
	mod := plainsofpain.New()
	dir := t.TempDir()
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	_, err := store.Create(context.Background(), Request{
		Instance: core.Instance{ID: "default", Dir: dir},
		Module:   mod,
		Kind:     KindManual,
	})
	if !errors.Is(err, core.ErrSavePathUnknown) {
		t.Fatalf("err = %v", err)
	}
	inst := core.Instance{ID: "default", Dir: dir, UserDir: dir}
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "configs", "server.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("server directory")) {
		t.Fatalf("err = %v", err)
	}

	root := string(filepath.Separator)
	if runtime.GOOS == "windows" {
		vol := filepath.VolumeName(dir)
		if vol == "" {
			t.Fatal("no volume")
		}
		root = vol + `\`
	}
	_, err = store.Create(context.Background(), Request{
		Instance: core.Instance{ID: "default", Dir: dir, UserDir: root},
		Module:   mod,
		Kind:     KindManual,
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("volume root")) {
		t.Fatalf("root err = %v", err)
	}
	if err := store.Exists("../x"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("exists = %v", err)
	}
	if err := store.Exists("20261015T120000.000000000Z"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing = %v", err)
	}
}

func TestSymlinkIsRefused(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	outside := filepath.Join(t.TempDir(), "outside.dat")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(save, "link.dat")); err != nil {
		t.Skip("symlink: " + err.Error())
	}
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	_, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("symlink")) {
		t.Fatalf("err = %v", err)
	}
}

func TestValidRelAndBackupDir(t *testing.T) {
	bad := []string{"a:b", "CON", "con.txt", "aux", "COM1", "LPT9", "foo.", "foo ", "foo/bar.", "dir/nul", `dir\file`}
	for _, rel := range bad {
		if err := validRel(rel); err == nil {
			t.Fatalf("accepted %q", rel)
		}
	}
	if err := validRel("slot/world.dat"); err != nil {
		t.Fatal(err)
	}
	server := t.TempDir()
	if err := ValidateDir(filepath.Join(server, "backups"), server); !errors.Is(err, ErrBackupDir) {
		t.Fatalf("inside = %v", err)
	}
	parent := t.TempDir()
	backupDir := filepath.Join(parent, "backups")
	server = filepath.Join(backupDir, "server")
	if err := ValidateDir(backupDir, server); !errors.Is(err, ErrBackupDir) {
		t.Fatalf("contains server = %v", err)
	}
	if err := ValidateDir("backups", t.TempDir()); !errors.Is(err, ErrBackupDir) {
		t.Fatalf("relative = %v", err)
	}
	if err := ValidateDir(`\\nas\backups`, t.TempDir()); !errors.Is(err, ErrBackupDir) {
		t.Fatalf("unc = %v", err)
	}
	outside := t.TempDir()
	if err := ValidateDir(outside, server); err != nil {
		t.Fatal(err)
	}
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	store := &Store{Dir: filepath.Join(inst.Dir, "backups"), Keep: 2, Key: testKey()}
	if _, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual}); !errors.Is(err, ErrBackupDir) {
		t.Fatalf("create = %v", err)
	}
}

func TestPayloadAndManifestTamper(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "good")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	ctx := context.Background()
	entry, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, entry.ID+".zip")
	writeSave(t, save, "world.dat", "live")
	corruptZipByte(t, path, false)
	if err := store.Restore(ctx, entry.ID, inst, mod); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("payload = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}

	entry, err = store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(store.Dir, entry.ID+".zip")
	corruptZipByte(t, path, true)
	if err := store.Verify(entry.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("manifest byte = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world after manifest byte = %q", got)
	}

	entry, err = store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(store.Dir, entry.ID+".zip")
	rewriteManifestHash(t, path)
	err = store.Verify(entry.ID)
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "hmac") {
		t.Fatalf("hmac = %v", err)
	}
}

func TestFailedSwapRollsBackFromSafety(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: t.TempDir(), Keep: 5, Key: testKey()}
	ctx := context.Background()
	old, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	mutated, err := json.Marshal(map[string]string{
		"serverName": "mutated",
		"password":   "placeholder-password",
		"userdir":    save,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst.Dir, "configs", "server.json"), append(mutated, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	safety, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindSafety, Protect: []string{old.ID}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.swapFail = func() error {
		calls++
		if calls != 2 {
			return nil
		}
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(save), ".gmanager-hold-*"))
		if err != nil {
			return err
		}
		for _, match := range matches {
			if err := os.RemoveAll(match); err != nil {
				return err
			}
		}
		return errors.New("injected swap failure")
	}
	err = store.RestoreInto(ctx, old.ID, inst, mod, safety.ID, nil)
	if err == nil || !strings.Contains(err.Error(), "rolled back from safety backup") {
		t.Fatalf("err = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
	cfg, err := os.ReadFile(filepath.Join(inst.Dir, "configs", "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(cfg, []byte("mutated")) {
		t.Fatalf("config = %s", cfg)
	}
}

func TestFailedSwapUndoesBeforeSafetyZip(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: t.TempDir(), Keep: 5, Key: testKey()}
	ctx := context.Background()
	old, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	mutated, err := json.Marshal(map[string]string{
		"serverName": "mutated",
		"password":   "placeholder-password",
		"userdir":    save,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(inst.Dir, "configs", "server.json"), append(mutated, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	safety, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindSafety, Protect: []string{old.ID}})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	store.swapFail = func() error {
		calls++
		if calls != 2 {
			return nil
		}
		return errors.New("injected swap failure")
	}
	err = store.RestoreInto(ctx, old.ID, inst, mod, safety.ID, nil)
	if err == nil || strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("err = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(save), ".gmanager-hold-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("holds left behind: %v", matches)
	}
}

func TestCancelDuringSwapStillRestores(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: t.TempDir(), Keep: 5, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.swapFail = func() error {
		cancel()
		return nil
	}
	if err := store.Restore(ctx, entry.ID, inst, mod); err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "world.dat"); got != "alpha" {
		t.Fatalf("world = %q", got)
	}
}

func TestCorruptZipAndPartialDoNotBreakList(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	partial := filepath.Join(store.Dir, "20261015T120000.000000000Z.zip.partial")
	if err := os.WriteFile(partial, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	badID := "20261015T010101.000000000Z"
	if err := os.WriteFile(filepath.Join(store.Dir, badID+".zip"), []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	page, _, err := store.List("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); err != nil {
		t.Fatal("list removed the partial")
	}
	if _, err := os.Stat(filepath.Join(store.Dir, badID+".zip")); err != nil {
		t.Fatal("list quarantined or removed the bad zip")
	}
	var sawGood, sawBad bool
	for _, item := range page {
		if item.ID == entry.ID && !item.Corrupt {
			sawGood = true
		}
		if item.ID == badID && item.Corrupt {
			sawBad = true
		}
	}
	if !sawGood || !sawBad {
		t.Fatalf("page = %+v", page)
	}
}

func TestZipBombCaps(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", strings.Repeat("a", 256<<10))
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 2, Key: testKey()}
	prevEntries := maxZipEntries
	maxZipEntries = 1
	_, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	maxZipEntries = prevEntries
	if err == nil || !strings.Contains(err.Error(), "too many files") {
		t.Fatalf("entries = %v", err)
	}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	prevRatio, prevFloor := maxZipRatio, maxZipRatioFloor
	maxZipRatio = 1
	maxZipRatioFloor = 1
	err = store.Verify(entry.ID)
	maxZipRatio, maxZipRatioFloor = prevRatio, prevFloor
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "compression ratio") {
		t.Fatalf("ratio = %v", err)
	}
}

func TestMissingKeyDoesNotMintOrDelete(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	dir := filepath.Join(t.TempDir(), "backups")
	keyPath := filepath.Join(t.TempDir(), "backup.hmac")
	store := &Store{Dir: dir, Keep: 5, KeyPath: keyPath}
	ctx := context.Background()
	if _, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual}); err != nil {
		t.Fatal(err)
	}
	copies := []string{keyPath, keyPath + ".bak", filepath.Join(filepath.Dir(keyPath), "integrity.key")}
	for _, path := range copies {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s mode = %o", path, info.Mode().Perm())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "integrity.key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("integrity key was written into the backup directory")
	}
	if err := os.Remove(keyPath); err != nil {
		t.Fatal(err)
	}
	store.loadedKey = nil
	if _, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(keyPath); err != nil {
		t.Fatalf("primary key was not restored from a copy: %v", err)
	}
	for _, path := range copies {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	store.loadedKey = nil
	if _, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual}); !errors.Is(err, ErrKeyMissing) {
		t.Fatalf("err = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".zip") {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("zips = %d", n)
	}
}

func TestUnsignedBackupIsSignedInPlace(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	data := t.TempDir()
	keyPath := filepath.Join(data, "backup.hmac")
	if err := os.WriteFile(keyPath, testKey(), 0o600); err != nil {
		t.Fatal(err)
	}
	keyWhen := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(keyPath, keyWhen, keyWhen); err != nil {
		t.Fatal(err)
	}
	store := &Store{
		Dir:        filepath.Join(t.TempDir(), "backups"),
		JournalDir: data,
		Keep:       5,
		Key:        testKey(),
		KeyPath:    keyPath,
	}
	ctx := context.Background()
	entry, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, entry.ID+".zip")
	clearHMAC(t, path, false)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	page, _, err := store.List("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || !page[0].Unsigned || page[0].Corrupt {
		t.Fatalf("page = %+v", page)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("list rewrote the unsigned zip")
	}
	later, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !later.ModTime().Equal(info.ModTime()) || later.Size() != info.Size() {
		t.Fatal("list changed the unsigned zip")
	}
	if err := store.Verify(entry.ID); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("verify = %v", err)
	}
	writeSave(t, save, "world.dat", "live")
	if err := store.Restore(ctx, entry.ID, inst, mod); !errors.Is(err, ErrUnsigned) {
		t.Fatalf("restore = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
	view, err := store.PreviewSign(entry.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !view.Unsigned || view.ManifestSHA256 == "" || len(view.Files) == 0 || view.Files[0].SHA256 == "" {
		t.Fatalf("preview = %+v", view)
	}
	unsigned, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Sign(entry.ID, view.ManifestSHA256); !errors.Is(err, ErrTooNew) {
		t.Fatalf("newer than key = %v", err)
	}
	stayed, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(unsigned, stayed) {
		t.Fatal("a zip newer than the key was rewritten")
	}
	older := keyWhen.Add(-time.Hour)
	if err := os.Chtimes(path, older, older); err != nil {
		t.Fatal(err)
	}
	if err := store.Sign(entry.ID, "not-the-manifest"); !errors.Is(err, ErrManifestAck) {
		t.Fatalf("ack = %v", err)
	}
	if err := store.Sign(entry.ID, view.ManifestSHA256); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(manifestJSON(t, path), []byte(`"hmac"`)) {
		t.Fatal("sign left the archive unsigned")
	}
	if err := store.Verify(entry.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Restore(ctx, entry.ID, inst, mod); err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "world.dat"); got != "alpha" {
		t.Fatalf("world = %q", got)
	}

	entry, err = store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(store.Dir, entry.ID+".zip")
	clearHMAC(t, path, true)
	if err := os.Chtimes(path, time.Now().Add(2*time.Second), time.Now().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Sign(entry.ID, view.ManifestSHA256); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("sign bad hash = %v", err)
	}
	if err := store.Verify(entry.ID); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("bad hash = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	store.Keep = 1
	if err := store.prune([]string{entry.ID}); err != nil {
		t.Fatal(err)
	}
	if err := store.prune(nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unclassifiable zip still present: %v", err)
	}
	if _, err := os.Stat(path + ".quarantine"); err != nil {
		t.Fatalf("quarantine = %v", err)
	}
	if err := store.Discard(entry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + ".quarantine"); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discard left the quarantine file")
	}
	kept, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Discard(kept.ID); !errors.Is(err, ErrNotQuarantined) {
		t.Fatalf("live discard = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, kept.ID+".zip")); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryCacheSkipsUnchangedZips(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store.Dir, entry.ID+".zip")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	corruptZipByte(t, path, false)
	if err := os.Chtimes(path, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	page, _, err := store.List("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Corrupt {
		t.Fatalf("cached page = %+v", page)
	}
	now := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
	page, _, err = store.List("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || !page[0].Corrupt {
		t.Fatalf("restatted page = %+v", page)
	}
}

func TestJournalRollsBackPendingAndLeavesStrayHolds(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "new")
	parent := filepath.Dir(save)
	hold := filepath.Join(parent, ".gmanager-hold-"+filepath.Base(save))
	if err := os.MkdirAll(hold, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hold, "world.dat"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(parent, ".gmanager-restore-abc")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(parent, ".gmanager-migrate-abc")
	if err := os.MkdirAll(stray, 0o755); err != nil {
		t.Fatal(err)
	}
	pending := journalFile{
		Version: journalVersion,
		State:   journalPending,
		Targets: []journalTarget{{Dest: save, Hold: hold, Dir: true}},
		Staging: []string{staging},
	}
	if err := writeRawJournal(t, inst.Dir, pending); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: data, Keep: 5, Key: testKey()}
	if err := store.Recover(inst, mod); err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "world.dat"); got != "new" {
		t.Fatalf("server-dir journal changed the save: %q", got)
	}
	if _, err := os.Stat(journalPath(inst.Dir)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hold); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(data, pending, testKey()); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(journalPath(data))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("journal mode = %o", info.Mode().Perm())
	}
	if err := store.Recover(inst, mod); err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "world.dat"); got != "old" {
		t.Fatalf("world = %q", got)
	}
	if _, err := os.Stat(hold); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pending hold was kept")
	}
	if _, err := os.Stat(staging); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journaled staging was kept")
	}
	if _, err := os.Stat(stray); err != nil {
		t.Fatal("unlisted staging was removed")
	}
	if _, err := os.Stat(journalPath(data)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journal was kept")
	}

	writeSave(t, save, "world.dat", "new")
	extra := hold + ".2"
	if err := os.MkdirAll(extra, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(extra, "world.dat"), []byte("held"), 0o644); err != nil {
		t.Fatal(err)
	}
	committed := journalFile{
		Version: journalVersion,
		State:   journalCommitted,
		Targets: []journalTarget{{Dest: save, Hold: extra, Dir: true}},
	}
	if err := writeJournal(data, committed, testKey()); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(inst, mod); err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "world.dat"); got != "new" {
		t.Fatalf("committed world = %q", got)
	}
	if _, err := os.Stat(extra); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("committed hold was kept")
	}

	orphan := hold
	if err := os.MkdirAll(orphan, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "world.dat"), []byte("only-copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(inst, mod); err != nil {
		t.Fatalf("stray hold stopped recovery: %v", err)
	}
	found, err := store.StrayHolds(inst, mod)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || !SamePath(found[0], orphan) {
		t.Fatalf("stray = %v", found)
	}
	if _, err := os.Stat(orphan); err != nil {
		t.Fatal("unjournaled hold was deleted")
	}
}

func TestForgedJournalIsNotApplied(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "live")
	victim := filepath.Join(t.TempDir(), "victim.dat")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: data, Keep: 5, Key: testKey()}
	forged := journalFile{
		Version: journalVersion,
		State:   journalCommitted,
		Targets: []journalTarget{{Dest: save, Hold: victim, Dir: false}},
		HMAC:    "0000000000000000000000000000000000000000000000000000000000000000",
	}
	if err := writeRawJournal(t, data, forged); err != nil {
		t.Fatal(err)
	}
	err := store.Recover(inst, mod)
	if !errors.Is(err, ErrJournalAuth) {
		t.Fatalf("forged = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
		t.Fatalf("victim = %q %v", got, err)
	}
	if _, err := os.Stat(journalPath(data)); err != nil {
		t.Fatal("forged journal was removed")
	}

	secret := filepath.Join(t.TempDir(), "secret.dat")
	if err := os.WriteFile(secret, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(journalPath(data)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, journalPath(data)); err != nil {
		t.Logf("symlink journal not tested: %v", err)
		return
	}
	err = store.Recover(inst, mod)
	if err == nil || !errors.Is(err, ErrJournal) {
		t.Fatalf("symlink journal = %v", err)
	}
	if got, err := os.ReadFile(secret); err != nil || string(got) != "secret" {
		t.Fatalf("secret = %q %v", got, err)
	}
}

func TestPathTraversalJournalIsNotApplied(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "live")
	outside := t.TempDir()
	victim := filepath.Join(outside, "victim.dat")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	parent := filepath.Dir(save)
	hold := filepath.Join(parent, ".gmanager-hold-"+filepath.Base(save))
	if err := os.MkdirAll(hold, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hold, "world.dat"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	data := t.TempDir()
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: data, Keep: 5, Key: testKey()}
	cases := []journalFile{
		{
			Version: journalVersion,
			State:   journalPending,
			Targets: []journalTarget{{Dest: victim, Dir: false}},
		},
		{
			Version: journalVersion,
			State:   journalPending,
			Targets: []journalTarget{{Dest: save, Hold: victim, Dir: true}},
		},
		{
			Version: journalVersion,
			State:   journalPending,
			Targets: []journalTarget{{Dest: save, Hold: hold, Dir: true}},
			Staging: []string{victim},
		},
		{
			Version: journalVersion,
			State:   journalPending,
			Targets: []journalTarget{{Dest: save, Hold: filepath.Join(outside, ".gmanager-hold-"+filepath.Base(save)), Dir: true}},
		},
	}
	for i, pending := range cases {
		if err := writeJournal(data, pending, testKey()); err != nil {
			t.Fatal(err)
		}
		err := store.Recover(inst, mod)
		if !errors.Is(err, ErrJournal) {
			t.Fatalf("case %d = %v", i, err)
		}
		if got := readSave(t, save, "world.dat"); got != "live" {
			t.Fatalf("case %d world = %q", i, got)
		}
		if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
			t.Fatalf("case %d victim = %q %v", i, got, err)
		}
		if _, err := os.Stat(hold); err != nil {
			t.Fatalf("case %d hold: %v", i, err)
		}
		if _, err := os.Stat(journalPath(data)); err != nil {
			t.Fatalf("case %d journal removed: %v", i, err)
		}
	}

	target := filepath.Join(t.TempDir(), "link-target")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "world.dat"), []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(save); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, save); err != nil {
		t.Logf("symlink dest not tested: %v", err)
		return
	}
	linked := journalFile{
		Version: journalVersion,
		State:   journalPending,
		Targets: []journalTarget{{Dest: save, Hold: hold, Dir: true}},
	}
	if err := writeJournal(data, linked, testKey()); err != nil {
		t.Fatal(err)
	}
	err := store.Recover(inst, mod)
	if err == nil || !errors.Is(err, ErrJournal) {
		t.Fatalf("symlink dest = %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(target, "world.dat")); err != nil || string(got) != "secret" {
		t.Fatalf("link target = %q %v", got, err)
	}
}

func TestMissingHoldIsReported(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "live")
	hold := filepath.Join(filepath.Dir(save), ".gmanager-hold-"+filepath.Base(save))
	data := t.TempDir()
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: data, Keep: 5, Key: testKey()}
	pending := journalFile{
		Version: journalVersion,
		State:   journalPending,
		Targets: []journalTarget{{Dest: save, Hold: hold, Dir: true}},
	}
	if err := writeJournal(data, pending, testKey()); err != nil {
		t.Fatal(err)
	}
	err := store.Recover(inst, mod)
	if !errors.Is(err, ErrMissingHold) {
		t.Fatalf("missing hold = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
	if _, err := os.Stat(journalPath(data)); err != nil {
		t.Fatal("journal was removed")
	}
}

func TestStrayHoldBlocksRestoreAndDiscardHold(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: t.TempDir(), Keep: 5, Key: testKey()}
	ctx := context.Background()
	entry, err := store.Create(ctx, Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	parent := filepath.Dir(save)
	hold := filepath.Join(parent, ".gmanager-hold-"+filepath.Base(save))
	if err := os.MkdirAll(hold, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hold, "world.dat"), []byte("only-copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(parent, ".gmanager-hold-other")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := store.Recover(inst, mod); err != nil {
		t.Fatal(err)
	}
	found, err := store.StrayHolds(inst, mod)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || !SamePath(found[0], hold) {
		t.Fatalf("stray = %v", found)
	}
	if err := store.Restore(ctx, entry.ID, inst, mod); !errors.Is(err, ErrUnjournaledHold) {
		t.Fatalf("restore = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
	victim := filepath.Join(t.TempDir(), "victim.dat")
	if err := os.WriteFile(victim, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.DiscardHold(inst, mod, victim); !errors.Is(err, ErrInvalidHold) {
		t.Fatalf("victim discard = %v", err)
	}
	if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
		t.Fatalf("victim = %q %v", got, err)
	}
	linkHold := hold + ".2"
	if err := os.Symlink(victim, linkHold); err != nil {
		t.Logf("symlink hold not tested: %v", err)
	} else {
		if err := store.DiscardHold(inst, mod, linkHold); !errors.Is(err, ErrJournal) {
			t.Fatalf("symlink discard = %v", err)
		}
		if got, err := os.ReadFile(victim); err != nil || string(got) != "keep" {
			t.Fatalf("symlink victim = %q %v", got, err)
		}
		if _, err := os.Lstat(linkHold); err != nil {
			t.Fatal("symlink hold was removed")
		}
		if err := os.Remove(linkHold); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.DiscardHold(inst, mod, hold); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(hold); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("hold was kept")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("unrelated hold name was removed")
	}
	if err := store.Restore(ctx, entry.ID, inst, mod); err != nil {
		t.Fatal(err)
	}
	if got := readSave(t, save, "world.dat"); got != "alpha" {
		t.Fatalf("world = %q", got)
	}

	if err := os.MkdirAll(hold, 0o755); err != nil {
		t.Fatal(err)
	}
	named := journalFile{
		Version: journalVersion,
		State:   journalPending,
		Targets: []journalTarget{{Dest: save, Hold: hold, Dir: true}},
	}
	if err := writeJournal(store.JournalDir, named, testKey()); err != nil {
		t.Fatal(err)
	}
	if err := store.DiscardHold(inst, mod, hold); !errors.Is(err, ErrHoldInUse) {
		t.Fatalf("in use = %v", err)
	}
	if _, err := os.Stat(hold); err != nil {
		t.Fatal("journaled hold was deleted")
	}
}

func TestCommitCleanupFailure(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: t.TempDir(), Keep: 5, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	hookCommitCleanup = func() error { return errors.New("disk full") }
	t.Cleanup(func() { hookCommitCleanup = nil })
	err = store.Restore(context.Background(), entry.ID, inst, mod)
	if err == nil || !strings.Contains(err.Error(), "restored; cleanup failed") {
		t.Fatalf("err = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "alpha" {
		t.Fatalf("world = %q", got)
	}
}

func writeRawJournal(t *testing.T, dir string, j journalFile) error {
	t.Helper()
	raw, err := json.Marshal(j)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(journalPath(dir), append(raw, '\n'), 0o600)
}

func TestCaseCollidingZipEntries(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.dat")
	b := filepath.Join(dir, "b.dat")
	if err := os.WriteFile(a, []byte("A"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	id := "20261015T120000.000000000Z"
	man := Manifest{
		Version:    manifestVer,
		ID:         id,
		CreatedAt:  time.Now().UTC(),
		InstanceID: "default",
		GameID:     "plainsofpain",
		Kind:       KindManual,
		Saves:      []SaveRoot{{Index: 0}},
	}
	files := []sourceFile{
		{abs: a, entry: ManifestFile{Path: "saves/0/World.dat", Role: roleSave, Root: 0, Rel: "World.dat"}},
		{abs: b, entry: ManifestFile{Path: "saves/0/world.dat", Role: roleSave, Root: 0, Rel: "world.dat"}},
	}
	zipPath := filepath.Join(dir, id+".zip")
	if _, err := writeZip(zipPath, man, files, testKey()); err != nil {
		t.Fatal(err)
	}
	store := &Store{Dir: dir, Keep: 5, Key: testKey()}
	err := store.Verify(id)
	if !errors.Is(err, ErrIntegrity) || !strings.Contains(err.Error(), "case-colliding") {
		t.Fatalf("err = %v", err)
	}
}

func TestAdaptConfigRefusesBeforeSwap(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	err = store.RestoreInto(context.Background(), entry.ID, inst, mod, "", func(string, []byte) ([]byte, error) {
		return nil, errors.New("refusing to restore")
	})
	if err == nil || !strings.Contains(err.Error(), "refusing to restore") {
		t.Fatalf("err = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
}

func TestPendingJournalIsWrittenBeforeSwap(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "alpha")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), JournalDir: t.TempDir(), Keep: 5, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	writeSave(t, save, "world.dat", "live")
	store.swapFail = func() error {
		j, err := readJournal(store.JournalDir, testKey())
		if err != nil {
			return err
		}
		if j.State != journalPending || len(j.Targets) == 0 || j.HMAC == "" {
			return errors.New("journal was not pending before the swap")
		}
		info, err := os.Lstat(journalPath(store.JournalDir))
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("journal is a symlink")
		}
		if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
			return errors.New("journal mode is not 0600")
		}
		return errors.New("stop before swap")
	}
	err = store.Restore(context.Background(), entry.ID, inst, mod)
	if err == nil || !strings.Contains(err.Error(), "stop before swap") {
		t.Fatalf("err = %v", err)
	}
	if got := readSave(t, save, "world.dat"); got != "live" {
		t.Fatalf("world = %q", got)
	}
	if _, err := os.Stat(journalPath(store.JournalDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal left behind: %v", err)
	}
	if _, err := os.Stat(journalPath(inst.Dir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("journal was written into the server directory")
	}
}

func TestClassifyRetriesOpenOnce(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	calls := 0
	classifyOpenHook = func() error {
		calls++
		if calls == 1 {
			return errors.New("transient")
		}
		return nil
	}
	t.Cleanup(func() { classifyOpenHook = nil })
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("opens = %d", calls)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, entry.ID+".zip")); err != nil {
		t.Fatal(err)
	}
	page, _, err := store.List("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || page[0].Corrupt || page[0].Quarantined {
		t.Fatalf("page = %+v", page)
	}
}

func TestPruneAndDiscardShareTheMutex(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	if _, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual}); err != nil {
		t.Fatal(err)
	}
	badID := "20261015T010101.000000000Z"
	if err := os.WriteFile(filepath.Join(store.Dir, badID+".zip"), []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	otherID := "20261015T010102.000000000Z"
	quarantine := filepath.Join(store.Dir, otherID+".zip.quarantine")
	if err := os.WriteFile(quarantine, []byte("aside"), 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	store.hookQuarantine = func() {
		once.Do(func() { close(started) })
		<-release
	}
	pruneErr := make(chan error, 1)
	go func() {
		pruneErr <- store.prune(nil)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("quarantine did not start")
	}
	discardErr := make(chan error, 1)
	go func() {
		discardErr <- store.Discard(otherID)
	}()
	select {
	case err := <-discardErr:
		t.Fatalf("discard finished while quarantine held the mutex: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	close(release)
	if err := <-pruneErr; err != nil {
		t.Fatal(err)
	}
	if err := <-discardErr; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(quarantine); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("discard left the quarantine file")
	}
	if _, err := os.Stat(filepath.Join(store.Dir, badID+".zip.quarantine")); err != nil {
		t.Fatalf("bad zip was not quarantined: %v", err)
	}
}

func TestReleaseRestoresOneQuarantineFile(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5, Key: testKey()}
	entry, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(store.Dir, entry.ID+".zip")
	aside := live + ".quarantine"
	if err := os.Rename(live, aside); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(entry.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(aside); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("quarantine file was kept")
	}
	if err := os.WriteFile(aside, []byte("extra"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.Release(entry.ID); !errors.Is(err, ErrLiveBackup) {
		t.Fatalf("live = %v", err)
	}
	if _, err := os.Stat(live); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(aside); err != nil {
		t.Fatal(err)
	}
}

func newWorld(t *testing.T) (core.Instance, *plainsofpain.Module, string) {
	t.Helper()
	root := t.TempDir()
	instDir := filepath.Join(root, "server")
	save := filepath.Join(root, "saves")
	if err := os.MkdirAll(filepath.Join(instDir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(save, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]string{
		"serverName": "kept",
		"password":   "placeholder-password",
		"userdir":    save,
	})
	if err != nil {
		t.Fatal(err)
	}
	body = append(body, '\n')
	if err := os.WriteFile(filepath.Join(instDir, "configs", "server.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	return core.Instance{ID: "default", Dir: instDir, UserDir: save}, plainsofpain.New(), save
}

func writeSave(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readSave(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func manifestJSON(t *testing.T, path string) []byte {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for _, f := range r.File {
		if f.Name != manifestName {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		defer rc.Close()
		b, err := io.ReadAll(rc)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	t.Fatal("manifest missing")
	return nil
}

func tamperZip(t *testing.T, path string) {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		name string
		body []byte
	}
	var items []item
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if closeErr := rc.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item{name: f.Name, body: b})
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	changed := false
	for i := range items {
		if items[i].name == manifestName {
			continue
		}
		items[i].body = append(append([]byte{}, items[i].body...), 'X')
		changed = true
		break
	}
	if !changed {
		t.Fatal("no file to tamper")
	}
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for _, item := range items {
		w, err := zw.Create(item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(item.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func corruptZipByte(t *testing.T, path string, manifest bool) {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	var off int64
	found := false
	for _, f := range r.File {
		isManifest := f.Name == manifestName
		if isManifest != manifest {
			continue
		}
		if !manifest && f.UncompressedSize64 == 0 {
			continue
		}
		off, err = f.DataOffset()
		if err != nil {
			t.Fatal(err)
		}
		found = true
		break
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no zip entry to corrupt")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if off < 0 || int(off) >= len(b) {
		t.Fatalf("data offset %d outside zip of %d", off, len(b))
	}
	b[off] ^= 0xff
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func clearHMAC(t *testing.T, path string, badHash bool) {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		name string
		body []byte
	}
	var items []item
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if closeErr := rc.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item{name: f.Name, body: b})
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	changed := false
	for i := range items {
		if items[i].name != manifestName {
			continue
		}
		var man Manifest
		if err := json.Unmarshal(items[i].body, &man); err != nil {
			t.Fatal(err)
		}
		man.HMAC = ""
		if badHash {
			if len(man.Files) == 0 || len(man.Files[0].SHA256) == 0 {
				t.Fatal("manifest has nothing to rewrite")
			}
			sum := []byte(man.Files[0].SHA256)
			if sum[0] == '0' {
				sum[0] = '1'
			} else {
				sum[0] = '0'
			}
			man.Files[0].SHA256 = string(sum)
		}
		raw, err := json.MarshalIndent(man, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		items[i].body = append(raw, '\n')
		changed = true
		break
	}
	if !changed {
		t.Fatal("manifest missing")
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for _, item := range items {
		w, err := zw.Create(item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(item.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}

func rewriteManifestHash(t *testing.T, path string) {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	type item struct {
		name string
		body []byte
	}
	var items []item
	for _, f := range r.File {
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		b, err := io.ReadAll(rc)
		if closeErr := rc.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, item{name: f.Name, body: b})
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	changed := false
	for i := range items {
		if items[i].name != manifestName {
			continue
		}
		var man Manifest
		if err := json.Unmarshal(items[i].body, &man); err != nil {
			t.Fatal(err)
		}
		if len(man.Files) == 0 || len(man.Files[0].SHA256) == 0 || man.HMAC == "" {
			t.Fatal("manifest has nothing to rewrite")
		}
		sum := []byte(man.Files[0].SHA256)
		if sum[0] == '0' {
			sum[0] = '1'
		} else {
			sum[0] = '0'
		}
		man.Files[0].SHA256 = string(sum)
		raw, err := json.MarshalIndent(man, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		items[i].body = append(raw, '\n')
		changed = true
		break
	}
	if !changed {
		t.Fatal("manifest missing")
	}
	out, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(out)
	for _, item := range items {
		w, err := zw.Create(item.name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(item.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
}
