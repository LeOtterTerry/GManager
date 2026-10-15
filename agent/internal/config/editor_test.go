package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestEditor(t *testing.T) (*Editor, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := filepath.Join(dir, "configs", "server.json")
	st := filepath.Join(dir, "state", "server.json.meta")
	ed, err := NewEditor(cfg, st)
	if err != nil {
		t.Fatal(err)
	}
	return ed, cfg
}

func writeFile(t *testing.T, path string, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func mustRead(t *testing.T, ed *Editor) ReadResult {
	t.Helper()
	res, err := ed.Read()
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestReadAdoptsExistingFileWithoutWriting(t *testing.T) {
	ed, path := newTestEditor(t)
	body := "{\n  \"serverName\": \"A\"\n}\n"
	writeFile(t, path, body)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	res := mustRead(t, ed)
	if res.Revision != 1 || res.ExternalChange {
		t.Fatalf("read = %+v", res)
	}
	if res.SHA256 == "" || res.Size != int64(len(body)) {
		t.Fatalf("snapshot sha=%q size=%d", res.SHA256, res.Size)
	}
	if readFile(t, path) != body {
		t.Fatal("adopting a file rewrote it")
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("adopting a file changed its mtime")
	}
}

func TestExternalEditIsDetectedAndNotOverwritten(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"serverName\":\"A\",\"password\":\"x\"}\n")
	first := mustRead(t, ed)

	external := "{\"serverName\":\"B\",\"password\":\"x\"}\n"
	writeFile(t, path, external)
	// Ensure mtime can move even if the write is fast.
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	second := mustRead(t, ed)
	if !second.ExternalChange {
		t.Fatal("expected an external change")
	}
	if second.Revision <= first.Revision {
		t.Fatalf("revision did not bump: %d -> %d", first.Revision, second.Revision)
	}
	if string(second.Content) != external {
		t.Fatalf("content = %s", second.Content)
	}
	if !bytes.Contains(second.LastKnown, []byte(`"serverName":"A"`)) && !bytes.Contains(second.LastKnown, []byte(`"serverName": "A"`)) {
		t.Fatalf("last known = %s", second.LastKnown)
	}
	if readFile(t, path) != external {
		t.Fatalf("read rewrote the external edit: %s", readFile(t, path))
	}

	_, err := ed.Save(first.Revision, []byte(`{"serverName":"C","password":"x"}`), nil, nil)
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("save with old revision = %v", err)
	}
	if readFile(t, path) != external {
		t.Fatalf("stale save overwrote external edit: %s", readFile(t, path))
	}
}

func TestNonConflictingExternalEditMerges(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"serverName\":\"A\",\"password\":\"x\",\"customFlag\":{\"keep\":true}}\n")
	mustRead(t, ed)
	external := "{\"serverName\":\"A\",\"password\":\"x\",\"customFlag\":{\"keep\":false}}\n"
	writeFile(t, path, external)
	seen := mustRead(t, ed)
	if !seen.ExternalChange {
		t.Fatal("expected external change")
	}

	saved, err := ed.Save(seen.Revision, []byte(`{"serverName":"A","password":"changed"}`), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Written {
		t.Fatal("expected a write for the merged password")
	}
	got := readFile(t, path)
	var doc map[string]any
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatal(err)
	}
	if doc["password"] != "changed" {
		t.Fatalf("password = %#v", doc["password"])
	}
	if doc["serverName"] != "A" {
		t.Fatalf("serverName = %#v", doc["serverName"])
	}
	flag, ok := doc["customFlag"].(map[string]any)
	if !ok || flag["keep"] != false {
		t.Fatalf("customFlag = %#v", doc["customFlag"])
	}
	if !strings.Contains(got, `"customFlag": {"keep":false}`) && !strings.Contains(got, `"customFlag":{"keep":false}`) {
		t.Fatalf("unknown key was not copied verbatim: %s", got)
	}
}

func TestConflictDoesNotWriteUntilResolved(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"serverName\":\"A\",\"password\":\"x\"}\n")
	mustRead(t, ed)
	external := "{\"serverName\":\"B\",\"password\":\"x\"}\n"
	writeFile(t, path, external)
	seen := mustRead(t, ed)

	_, err := ed.Save(seen.Revision, []byte(`{"serverName":"C","password":"y"}`), nil, nil)
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("err = %v", err)
	}
	if len(conflict.Conflicts) != 1 || conflict.Conflicts[0].Field != "serverName" {
		t.Fatalf("conflicts = %+v", conflict.Conflicts)
	}
	if readFile(t, path) != external {
		t.Fatalf("conflict save wrote the file: %s", readFile(t, path))
	}

	saved, err := ed.Save(seen.Revision, []byte(`{"serverName":"C","password":"y"}`), map[string]string{"serverName": "disk"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !saved.Written {
		t.Fatal("expected resolved save to write")
	}
	var doc map[string]any
	if err := json.Unmarshal(saved.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["serverName"] != "B" || doc["password"] != "y" {
		t.Fatalf("disk resolution = %#v", doc)
	}
}

func TestConflictCanTakeTheEdit(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"serverName\":\"A\",\"password\":\"x\"}\n")
	mustRead(t, ed)
	writeFile(t, path, "{\"serverName\":\"B\",\"password\":\"x\"}\n")
	seen := mustRead(t, ed)

	saved, err := ed.Save(seen.Revision, []byte(`{"serverName":"C","password":"x"}`), map[string]string{"serverName": "edit"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(saved.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["serverName"] != "C" || doc["password"] != "x" {
		t.Fatalf("edit resolution = %#v", doc)
	}
}

func TestUnknownKeySurvivesSaveWhenOmitted(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"serverName\":\"A\",\"modSetting\":1,\"password\":\"x\"}\n")
	seen := mustRead(t, ed)

	saved, err := ed.Save(seen.Revision, []byte(`{"serverName":"A","password":"next"}`), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(saved.Body, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["modSetting"] != float64(1) {
		t.Fatalf("modSetting = %#v", doc["modSetting"])
	}
	if doc["password"] != "next" {
		t.Fatalf("password = %#v", doc["password"])
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
}

func TestStaleRevisionDoesNotWrite(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"serverName\":\"A\"}\n")
	seen := mustRead(t, ed)
	_, err := ed.Save(seen.Revision+10, []byte(`{"serverName":"Z"}`), nil, nil)
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(readFile(t, path), `"serverName":"A"`) && !strings.Contains(readFile(t, path), `"A"`) {
		t.Fatalf("file changed: %s", readFile(t, path))
	}
	if strings.Contains(readFile(t, path), "Z") {
		t.Fatal("stale save wrote the new name")
	}
}

func TestAtomicWriteKeepsBakAndNoTemp(t *testing.T) {
	ed, path := newTestEditor(t)
	first, err := ed.Save(0, []byte(`{"serverName":"one"}`), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ed.Save(first.Revision, []byte(`{"serverName":"two"}`), nil, nil); err != nil {
		t.Fatal(err)
	}
	matches, err := filepath.Glob(path + ".*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("baks = %v", matches)
	}
	bak, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(bak, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["serverName"] != "one" {
		t.Fatalf("bak = %#v", doc)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".tmp-") {
			t.Fatalf("temp file left behind: %s", entry.Name())
		}
	}
}

func TestKeyOrderPreserved(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"zebra\":1,\"alpha\":2,\"password\":\"x\"}\n")
	seen := mustRead(t, ed)
	if _, err := ed.Save(seen.Revision, []byte(`{"password":"y"}`), nil, nil); err != nil {
		t.Fatal(err)
	}
	got := readFile(t, path)
	zebra := strings.Index(got, `"zebra"`)
	alpha := strings.Index(got, `"alpha"`)
	password := strings.Index(got, `"password"`)
	if zebra < 0 || alpha < 0 || password < 0 || !(zebra < alpha && alpha < password) {
		t.Fatalf("order not preserved: %s", got)
	}
}

func TestExternalDeleteIsNotRecreated(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"serverName\":\"A\"}\n")
	seen := mustRead(t, ed)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := ed.Save(seen.Revision, []byte(`{"serverName":"A"}`), nil, nil)
	var stale *StaleError
	if !errors.As(err, &stale) {
		t.Fatalf("first save after delete = %v", err)
	}
	_, err = ed.Save(stale.Revision, []byte(`{"serverName":"B"}`), nil, nil)
	if !errors.Is(err, ErrExternalDelete) {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatal("deleted config was recreated")
	}
}

func TestValidationFailureDoesNotWrite(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"password\":\"ok\"}\n")
	seen := mustRead(t, ed)
	_, err := ed.Save(seen.Revision, []byte(`{"password":"way-too-long"}`), nil, func([]byte) error {
		return errors.New("rejected")
	})
	if err == nil || err.Error() != "rejected" {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(readFile(t, path), `"ok"`) {
		t.Fatalf("file = %s", readFile(t, path))
	}
}

func TestIdenticalSaveDoesNotRewriteExternalBytes(t *testing.T) {
	ed, path := newTestEditor(t)
	body := "{ \"serverName\" : \"A\", \"extra\": [1, 2] }\n"
	writeFile(t, path, body)
	seen := mustRead(t, ed)
	saved, err := ed.Save(seen.Revision, []byte(`{"serverName":"A","extra":[1, 2]}`), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Written {
		t.Fatal("save rewrote a file the merge did not change")
	}
	if readFile(t, path) != body {
		t.Fatalf("bytes changed:\n%s", readFile(t, path))
	}
}

func TestMtimeOnlyChangeIsExternalAndDoesNotRewrite(t *testing.T) {
	ed, path := newTestEditor(t)
	body := "{\"serverName\":\"A\"}\n"
	writeFile(t, path, body)
	mustRead(t, ed)
	future := time.Now().Add(5 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}
	seen := mustRead(t, ed)
	if !seen.ExternalChange {
		t.Fatal("mtime change was ignored")
	}
	if string(seen.Content) != body {
		t.Fatalf("content = %s", seen.Content)
	}
	if _, err := ed.Save(seen.Revision, []byte(`{"serverName":"A"}`), nil, nil); err != nil {
		t.Fatal(err)
	}
	if readFile(t, path) != body {
		t.Fatalf("mtime-only file was rewritten: %s", readFile(t, path))
	}
}

func TestNewEditorRemovesLeftoverTemps(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "configs", "server.json")
	if err := os.MkdirAll(filepath.Dir(cfg), 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := cfg + ".tmp-orphan"
	if err := os.WriteFile(orphan, []byte("junk"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEditor(cfg, filepath.Join(dir, "state", "server.json.meta")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan temp: %v", err)
	}
}

func TestMetaDoesNotStorePassword(t *testing.T) {
	ed, path := newTestEditor(t)
	const secret = "pw-PLAINTEXT-do-not-store"
	writeFile(t, path, "{\"serverName\":\"A\",\"password\":\""+secret+"\"}\n")
	seen := mustRead(t, ed)
	metaPath := filepath.Join(filepath.Dir(filepath.Dir(path)), "state", "server.json.meta")
	meta := readFile(t, metaPath)
	if strings.Contains(meta, secret) {
		t.Fatalf("metadata contains the password:\n%s", meta)
	}
	var snap struct {
		Body     []byte            `json:"body"`
		Redacted map[string]string `json:"redacted"`
	}
	if err := json.Unmarshal([]byte(meta), &snap); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(snap.Body), secret) || bytes.Contains(snap.Body, []byte(`"password"`)) {
		t.Fatalf("stored body still has the password: %s", snap.Body)
	}
	if snap.Redacted["password"] == "" {
		t.Fatal("password digest was not stored")
	}

	reloaded, err := NewEditor(path, metaPath)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := reloaded.Save(seen.Revision, []byte(`{"serverName":"B"}`), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved.Body), secret) {
		t.Fatalf("reloaded save dropped the password: %s", saved.Body)
	}
	if !strings.Contains(readFile(t, path), secret) || !strings.Contains(readFile(t, path), `"B"`) {
		t.Fatalf("config file = %s", readFile(t, path))
	}
	again := readFile(t, metaPath)
	if strings.Contains(again, secret) {
		t.Fatalf("metadata contains the password after save:\n%s", again)
	}
}

func TestAcknowledgeRecordsRestoreWithoutRewriting(t *testing.T) {
	ed, path := newTestEditor(t)
	writeFile(t, path, "{\"serverName\":\"A\"}\n")
	before := mustRead(t, ed)
	restored := "{\"serverName\":\"restored\",\"password\":\"placeholder-password\"}\n"
	writeFile(t, path, restored)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ed.Acknowledge()
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalChange || got.Revision != before.Revision+1 || string(got.Content) != restored {
		t.Fatalf("ack = %+v", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(info.ModTime()) || readFile(t, path) != restored {
		t.Fatal("acknowledge rewrote the config file")
	}
	meta := readFile(t, filepath.Join(filepath.Dir(path), "..", "state", "server.json.meta"))
	// state path is beside the temp root's state dir; read it from the editor by a second acknowledge.
	again, err := ed.Acknowledge()
	if err != nil {
		t.Fatal(err)
	}
	if again.Revision != got.Revision || again.ExternalChange {
		t.Fatalf("second ack = %+v", again)
	}
	view := mustRead(t, ed)
	if view.ExternalChange || string(view.Content) != restored {
		t.Fatalf("read = %+v", view)
	}
	if strings.Contains(meta, "placeholder-password") {
		t.Fatalf("metadata contains the password:\n%s", meta)
	}
}
