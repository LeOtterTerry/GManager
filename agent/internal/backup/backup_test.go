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
	"testing"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func TestSuggestAndDefaultDir(t *testing.T) {
	got, ok := SuggestDir(func(string) bool { return true })
	if !ok || got != SuggestedDir {
		t.Fatalf("suggest = %q %v", got, ok)
	}
	if _, ok := SuggestDir(func(string) bool { return false }); ok {
		t.Fatal("missing P: was treated as a suggestion")
	}
	if DefaultDir("data", func(string) bool { return false }) != filepath.Join("data", "backups") {
		t.Fatalf("default = %s", DefaultDir("data", func(string) bool { return false }))
	}
	if DefaultDir("data", func(string) bool { return true }) != SuggestedDir {
		t.Fatal("default ignored P:")
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
		Dir:  filepath.Join(t.TempDir(), "backups"),
		Keep: 10,
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

func TestRetentionAndProtectedID(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	clock := time.Date(2026, 10, 15, 13, 0, 0, 0, time.UTC)
	store := &Store{
		Dir:  filepath.Join(t.TempDir(), "backups"),
		Keep: 1,
		Now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
	}
	first, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(context.Background(), Request{
		Instance: inst,
		Module:   mod,
		Kind:     KindSafety,
		Protect:  []string{first.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, first.ID+".zip")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, second.ID+".zip")); err != nil {
		t.Fatal(err)
	}
	third, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindUpdate})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, first.ID+".zip")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oldest zip err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(store.Dir, third.ID+".zip")); err != nil {
		t.Fatal(err)
	}
}

func TestFreeSpaceLeavesNoPartial(t *testing.T) {
	inst, mod, save := newWorld(t)
	writeSave(t, save, "world.dat", "v")
	dir := filepath.Join(t.TempDir(), "backups")
	store := &Store{
		Dir:  dir,
		Keep: 5,
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
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5}
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
	store := &Store{Dir: filepath.Join(dir, "backups"), Keep: 5}
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
	store := &Store{Dir: filepath.Join(t.TempDir(), "backups"), Keep: 5}
	_, err := store.Create(context.Background(), Request{Instance: inst, Module: mod, Kind: KindManual})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("symlink")) {
		t.Fatalf("err = %v", err)
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
