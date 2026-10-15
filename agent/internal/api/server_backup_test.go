package api

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/backup"
	"github.com/LeOtterTerry/GManager/agent/internal/fakepop"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func TestBackupCursorRestoreAndIdempotency(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 10)
	_, httpSrv, cfgPath := newAPIOpt(t, 200, Options{Archives: archives})
	original := configJSON(t, save, "kept")
	writeCfg(t, cfgPath, original)
	writeSaveFile(t, save, "world.dat", "alpha")

	var ids []string
	for i := 0; i < 3; i++ {
		status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
		if status != http.StatusAccepted {
			t.Fatalf("backup = %d %s", status, body)
		}
		result := succeededResult(t, httpSrv.URL, body)
		if result["restarted"] != false || result["kind"] != backup.KindManual {
			t.Fatalf("result = %+v", result)
		}
		id, _ := result["id"].(string)
		ids = append(ids, id)
		if i == 0 {
			writeSaveFile(t, save, "world.dat", "beta")
			writeSaveFile(t, save, "extra.dat", "later")
		}
	}
	status, body, _ := do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/backups?limit=1", "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d %s", status, body)
	}
	if bytes.Contains(body, []byte("placeholder-password")) {
		t.Fatalf("list leaked a password: %s", body)
	}
	var page backupPage
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if page.BackupDir != archives.Dir || page.Keep != 10 || len(page.Items) != 1 || page.Items[0].ID != ids[0] || page.NextCursor != ids[0] {
		t.Fatalf("page = %+v", page)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/backups?cursor="+page.NextCursor+"&limit=10", "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("next = %d %s", status, body)
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Items) != 2 || page.Items[0].ID != ids[1] || page.Items[1].ID != ids[2] {
		t.Fatalf("rest = %+v", page)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/backups?cursor=nope", "test-token", "", nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("invalid_cursor")) {
		t.Fatalf("cursor = %d %s", status, body)
	}

	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, map[string]string{"Idempotency-Key": "bak-1"})
	if status != http.StatusAccepted {
		t.Fatalf("keyed backup = %d %s", status, body)
	}
	jobID := jsonString(t, body, "jobId")
	status, again, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, map[string]string{"Idempotency-Key": "bak-1"})
	if status != http.StatusAccepted || jsonString(t, again, "jobId") != jobID {
		t.Fatalf("replay = %d %s", status, again)
	}
	status, conflict, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{"other":true}`, map[string]string{"Idempotency-Key": "bak-1"})
	if status != http.StatusConflict || !bytes.Contains(conflict, []byte("idempotency_conflict")) {
		t.Fatalf("conflict = %d %s", status, conflict)
	}

	writeCfg(t, cfgPath, configJSON(t, save, "changed"))
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true}`, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("backup_id_required")) {
		t.Fatalf("missing id = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"id":"../x"}`, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("invalid_backup_id")) {
		t.Fatalf("bad id = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"id":"20261015T120000.000000000Z"}`, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown = %d %s", status, body)
	}

	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"id":"`+ids[0]+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore = %d %s", status, body)
	}
	result := succeededResult(t, httpSrv.URL, body)
	if result["state"] != "stopped" || result["id"] != ids[0] || result["safetyId"] == "" {
		t.Fatalf("restore result = %+v", result)
	}
	if readSaveFile(t, save, "world.dat") != "alpha" {
		t.Fatalf("world = %q", readSaveFile(t, save, "world.dat"))
	}
	if _, err := os.Stat(filepath.Join(save, "extra.dat")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("extra file err = %v", err)
	}
	if mustRead(t, cfgPath) != original {
		t.Fatalf("config = %s", mustRead(t, cfgPath))
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/config", "test-token", "", nil)
	if status != http.StatusOK || bytes.Contains(body, []byte(`"externalChange":true`)) || !bytes.Contains(body, []byte("kept")) {
		t.Fatalf("config view = %d %s", status, body)
	}
	safetyID, _ := result["safetyId"].(string)
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"id":"`+safetyID+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("safety restore = %d %s", status, body)
	}
	if readSaveFile(t, save, "world.dat") != "beta" {
		t.Fatalf("safety world = %q", readSaveFile(t, save, "world.dat"))
	}
}

func TestRunningFakeServerBackupAndTamperedRestore(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 10)
	srv, httpSrv, cfgPath := newAPIOpt(t, 200, Options{
		Archives:     archives,
		ReadyTimeout: 3 * time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Supervisor().Stop(ctx); err != nil {
			t.Errorf("cleanup stop: %v", err)
		}
	})
	inst := filepath.Dir(filepath.Dir(cfgPath))
	installFake(t, inst)
	original := configJSON(t, save, "kept")
	writeCfg(t, cfgPath, original)
	writeSaveFile(t, save, "world.dat", "alpha")

	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/start", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start = %d %s", status, body)
	}
	if jobText(t, httpSrv.URL, body) == "" || !bytes.Contains(jobByID(t, httpSrv.URL, jsonString(t, body, "jobId")), []byte(`"state":"succeeded"`)) {
		t.Fatalf("start job = %s", jobByID(t, httpSrv.URL, jsonString(t, body, "jobId")))
	}

	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	result := succeededResult(t, httpSrv.URL, body)
	if result["restarted"] != true {
		t.Fatalf("result = %+v", result)
	}
	id, _ := result["id"].(string)
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/status", "test-token", "", nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"state":"running"`)) || !bytes.Contains(body, []byte(`"ready":true`)) {
		t.Fatalf("status after backup = %d %s", status, body)
	}
	if mustRead(t, cfgPath) != original {
		t.Fatal("backup rewrote config")
	}

	writeSaveFile(t, save, "world.dat", "live")
	flipZip(t, filepath.Join(archives.Dir, id+".zip"))
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore = %d %s", status, body)
	}
	jobBody := jobByID(t, httpSrv.URL, jsonString(t, body, "jobId"))
	if !bytes.Contains(jobBody, []byte(`"state":"failed"`)) || !bytes.Contains(jobBody, []byte("integrity")) {
		t.Fatalf("tampered job = %s", jobBody)
	}
	if readSaveFile(t, save, "world.dat") != "live" {
		t.Fatalf("world = %q", readSaveFile(t, save, "world.dat"))
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/status", "test-token", "", nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"state":"running"`)) {
		t.Fatalf("status after rejected restore = %d %s", status, body)
	}
}

func TestRunningRestoreRestartsTheServer(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 10)
	srv, httpSrv, cfgPath := newAPIOpt(t, 200, Options{
		Archives:     archives,
		ReadyTimeout: 3 * time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Supervisor().Stop(ctx); err != nil {
			t.Errorf("cleanup stop: %v", err)
		}
	})
	inst := filepath.Dir(filepath.Dir(cfgPath))
	installFake(t, inst)
	original := configJSON(t, save, "kept")
	writeCfg(t, cfgPath, original)
	writeSaveFile(t, save, "world.dat", "alpha")

	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/start", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("start = %d %s", status, body)
	}
	if !bytes.Contains(jobByID(t, httpSrv.URL, jsonString(t, body, "jobId")), []byte(`"state":"succeeded"`)) {
		t.Fatalf("start job = %s", jobByID(t, httpSrv.URL, jsonString(t, body, "jobId")))
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	id, _ := succeededResult(t, httpSrv.URL, body)["id"].(string)
	writeSaveFile(t, save, "world.dat", "live")
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore = %d %s", status, body)
	}
	result := succeededResult(t, httpSrv.URL, body)
	if result["state"] != "running" {
		t.Fatalf("result = %+v", result)
	}
	if readSaveFile(t, save, "world.dat") != "alpha" {
		t.Fatalf("world = %q", readSaveFile(t, save, "world.dat"))
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/status", "test-token", "", nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"state":"running"`)) {
		t.Fatalf("status = %d %s", status, body)
	}
}

func TestCorruptBackupDoesNotFailTheList(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 10)
	_, httpSrv, cfgPath := newAPIOpt(t, 100, Options{Archives: archives})
	writeCfg(t, cfgPath, configJSON(t, save, "kept"))
	writeSaveFile(t, save, "world.dat", "alpha")
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	succeededResult(t, httpSrv.URL, body)
	bad := filepath.Join(archives.Dir, "20261015T010101.000000000Z.zip")
	if err := os.WriteFile(bad, []byte("not a zip"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/backups", "test-token", "", nil)
	if status != http.StatusOK {
		t.Fatalf("list = %d %s", status, body)
	}
	if !bytes.Contains(body, []byte(`"corrupt":true`)) || !bytes.Contains(body, []byte(`"kind":"manual"`)) {
		t.Fatalf("list = %s", body)
	}
}

func TestUpdateUsesBackupHook(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 10)
	steam := &recordingSteam{exe: fakeBytes(t)}
	srv, httpSrv, cfgPath := newAPIOpt(t, 200, Options{
		Archives:     archives,
		Steam:        steam,
		ReadyTimeout: 3 * time.Second,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := srv.Supervisor().Stop(ctx); err != nil {
			t.Errorf("cleanup stop: %v", err)
		}
	})
	inst := filepath.Dir(filepath.Dir(cfgPath))
	installFake(t, inst)
	original := configJSON(t, save, "kept")
	writeCfg(t, cfgPath, original)
	writeSaveFile(t, save, "world.dat", "alpha")

	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/update", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("update = %d %s", status, body)
	}
	result := succeededResult(t, httpSrv.URL, body)
	if result["ready"] != true || result["state"] != "running" {
		t.Fatalf("result = %+v", result)
	}
	if steam.calls != 1 || steam.appID != plainsofpain.SteamAppID || !steam.validate || !steam.anonymous {
		t.Fatalf("steam = %+v", steam)
	}
	if mustRead(t, cfgPath) != original {
		t.Fatal("update rewrote config")
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodGet, "/api/v1/servers/default/backups", "test-token", "", nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"kind":"update"`)) {
		t.Fatalf("backups = %d %s", status, body)
	}
	if readSaveFile(t, save, "world.dat") != "alpha" {
		t.Fatalf("world = %q", readSaveFile(t, save, "world.dat"))
	}
}

func TestRestoreWritesTheCurrentSaveDir(t *testing.T) {
	saveA := t.TempDir()
	saveB := t.TempDir()
	archives := testArchives(t, 10)
	_, httpSrv, cfgPath := newAPIOpt(t, 100, Options{Archives: archives})
	original := configJSON(t, saveA, "kept")
	writeCfg(t, cfgPath, original)
	writeSaveFile(t, saveA, "world.dat", "alpha")
	writeSaveFile(t, saveB, "world.dat", "nope")

	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	id, _ := succeededResult(t, httpSrv.URL, body)["id"].(string)
	moved := configJSON(t, saveB, "moved")
	writeCfg(t, cfgPath, moved)
	writeSaveFile(t, saveA, "world.dat", "mutated")
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore = %d %s", status, body)
	}
	if job := jobByID(t, httpSrv.URL, jsonString(t, body, "jobId")); !bytes.Contains(job, []byte(`"state":"succeeded"`)) {
		t.Fatalf("job = %s", job)
	}
	if readSaveFile(t, saveA, "world.dat") != "mutated" {
		t.Fatalf("archived save = %q", readSaveFile(t, saveA, "world.dat"))
	}
	if readSaveFile(t, saveB, "world.dat") != "alpha" {
		t.Fatalf("current save = %q", readSaveFile(t, saveB, "world.dat"))
	}
	named, ok := plainsofpain.New().ConfigSaveDir([]byte(mustRead(t, cfgPath)))
	if !ok || !backup.SamePath(named, saveB) {
		t.Fatalf("config userdir = %q ok=%v body=%s", named, ok, mustRead(t, cfgPath))
	}
}

func TestRestoreRefusesForeignSaveDirUnlessItIsEmpty(t *testing.T) {
	saveA := t.TempDir()
	saveB := t.TempDir()
	archives := testArchives(t, 10)
	_, httpSrv, cfgPath := newAPIOpt(t, 100, Options{Archives: archives})
	writeCfg(t, cfgPath, configJSON(t, saveA, "kept"))
	writeSaveFile(t, saveA, "world.dat", "alpha")
	writeSaveFile(t, saveB, "world.dat", "nope")
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	id, _ := succeededResult(t, httpSrv.URL, body)["id"].(string)
	moved := configJSON(t, saveB, "moved")
	writeCfg(t, cfgPath, moved)
	writeSaveFile(t, saveA, "world.dat", "mutated")
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"confirmArchivedSaveDir":true,"id":"`+id+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore = %d %s", status, body)
	}
	job := jobByID(t, httpSrv.URL, jsonString(t, body, "jobId"))
	if !bytes.Contains(job, []byte(`"state":"failed"`)) || !bytes.Contains(job, []byte("not the save folder")) {
		t.Fatalf("job = %s", job)
	}
	if readSaveFile(t, saveA, "world.dat") != "mutated" {
		t.Fatalf("archived save = %q", readSaveFile(t, saveA, "world.dat"))
	}
	if readSaveFile(t, saveB, "world.dat") != "nope" {
		t.Fatalf("current save = %q", readSaveFile(t, saveB, "world.dat"))
	}
	if mustRead(t, cfgPath) != moved {
		t.Fatal("config changed")
	}

	if err := os.RemoveAll(saveA); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(saveA, 0o755); err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"confirmArchivedSaveDir":true,"id":"`+id+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("empty restore = %d %s", status, body)
	}
	if job := jobByID(t, httpSrv.URL, jsonString(t, body, "jobId")); !bytes.Contains(job, []byte(`"state":"succeeded"`)) {
		t.Fatalf("job = %s", job)
	}
	if readSaveFile(t, saveA, "world.dat") != "alpha" {
		t.Fatalf("empty archived = %q", readSaveFile(t, saveA, "world.dat"))
	}
	if readSaveFile(t, saveB, "world.dat") != "nope" {
		t.Fatalf("current save = %q", readSaveFile(t, saveB, "world.dat"))
	}
}

func TestRestoreRechecksForeignDirAfterStop(t *testing.T) {
	saveA := t.TempDir()
	saveB := t.TempDir()
	archives := testArchives(t, 10)
	srv, httpSrv, cfgPath := newAPIOpt(t, 100, Options{Archives: archives})
	writeCfg(t, cfgPath, configJSON(t, saveA, "kept"))
	writeSaveFile(t, saveA, "world.dat", "alpha")
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	id, _ := succeededResult(t, httpSrv.URL, body)["id"].(string)
	writeCfg(t, cfgPath, configJSON(t, saveB, "moved"))
	writeSaveFile(t, saveB, "world.dat", "nope")
	if err := os.RemoveAll(saveA); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(saveA, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.afterQuiesce = func() {
		writeSaveFile(t, saveA, "late.dat", "appeared")
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"confirmArchivedSaveDir":true,"id":"`+id+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore = %d %s", status, body)
	}
	job := jobByID(t, httpSrv.URL, jsonString(t, body, "jobId"))
	if !bytes.Contains(job, []byte(`"state":"failed"`)) || !bytes.Contains(job, []byte("not the save folder")) {
		t.Fatalf("job = %s", job)
	}
	if readSaveFile(t, saveB, "world.dat") != "nope" {
		t.Fatalf("current save = %q", readSaveFile(t, saveB, "world.dat"))
	}
}

func TestDiscardRemovesOnlyQuarantine(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 10)
	_, httpSrv, cfgPath := newAPIOpt(t, 100, Options{Archives: archives})
	writeCfg(t, cfgPath, configJSON(t, save, "kept"))
	writeSaveFile(t, save, "world.dat", "alpha")
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	id, _ := succeededResult(t, httpSrv.URL, body)["id"].(string)
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard", "test-token", `{"id":"`+id+`"}`, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("confirm_required")) {
		t.Fatalf("confirm = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusConflict || !bytes.Contains(body, []byte("not_quarantined")) {
		t.Fatalf("live = %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(archives.Dir, id+".zip")); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(archives.Dir, id+".zip"), filepath.Join(archives.Dir, id+".zip.quarantine")); err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusOK {
		t.Fatalf("discard = %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(archives.Dir, id+".zip.quarantine")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("quarantine err = %v", err)
	}
}

func TestSignAndRelease(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 10)
	_, httpSrv, cfgPath := newAPIOpt(t, 100, Options{Archives: archives})
	writeCfg(t, cfgPath, configJSON(t, save, "kept"))
	writeSaveFile(t, save, "world.dat", "alpha")
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	id, _ := succeededResult(t, httpSrv.URL, body)["id"].(string)
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/sign", "test-token", `{"id":"`+id+`"}`, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"action":"sign-preview"`)) || !bytes.Contains(body, []byte("manifestSHA256")) {
		t.Fatalf("sign preview = %d %s", status, body)
	}
	var preview struct {
		ManifestSHA256 string `json:"manifestSHA256"`
		Files          []struct {
			SHA256 string `json:"sha256"`
		} `json:"files"`
	}
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.ManifestSHA256 == "" || len(preview.Files) == 0 || preview.Files[0].SHA256 == "" {
		t.Fatalf("preview = %s", body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/sign", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("manifest_required")) {
		t.Fatalf("sign confirm = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/sign", "test-token", `{"confirm":true,"id":"`+id+`","manifestSHA256":"deadbeef"}`, nil)
	if status != http.StatusConflict || !bytes.Contains(body, []byte("manifest_mismatch")) {
		t.Fatalf("sign mismatch = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/sign", "test-token", `{"confirm":true,"id":"`+id+`","manifestSHA256":"`+preview.ManifestSHA256+`"}`, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"action":"sign"`)) {
		t.Fatalf("sign = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/release", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusConflict || !bytes.Contains(body, []byte("not_quarantined")) {
		t.Fatalf("release live = %d %s", status, body)
	}
	if err := os.Rename(filepath.Join(archives.Dir, id+".zip"), filepath.Join(archives.Dir, id+".zip.quarantine")); err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/release", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"action":"release"`)) {
		t.Fatalf("release = %d %s", status, body)
	}
	if _, err := os.Stat(filepath.Join(archives.Dir, id+".zip")); err != nil {
		t.Fatal(err)
	}
}

func TestDiscardHoldAPI(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 5)
	_, httpSrv, cfgPath := newAPIOpt(t, 50, Options{Archives: archives})
	writeCfg(t, cfgPath, configJSON(t, save, "kept"))
	hold := filepath.Join(filepath.Dir(save), ".gmanager-hold-"+filepath.Base(save))
	if err := os.MkdirAll(hold, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hold, "world.dat"), []byte("only-copy"), 0o644); err != nil {
		t.Fatal(err)
	}
	noConfirm, err := json.Marshal(map[string]any{"path": hold})
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard-hold", "test-token", string(noConfirm), nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"action":"discard-hold-preview"`)) {
		t.Fatalf("preview = %d %s", status, body)
	}
	var preview struct {
		Path    string `json:"path"`
		Size    int64  `json:"size"`
		ModTime string `json:"modTime"`
	}
	if err := json.Unmarshal(body, &preview); err != nil {
		t.Fatal(err)
	}
	if preview.Path != hold || preview.ModTime == "" {
		t.Fatalf("preview body = %s", body)
	}
	if _, err := os.Stat(hold); err != nil {
		t.Fatal("preview removed the hold")
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard-hold", "test-token", `{"confirm":true}`, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("hold_required")) {
		t.Fatalf("path = %d %s", status, body)
	}
	confirmOnly, err := json.Marshal(map[string]any{"confirm": true, "path": hold})
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard-hold", "test-token", string(confirmOnly), nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("hold_stat_required")) {
		t.Fatalf("stat = %d %s", status, body)
	}
	badPath, err := json.Marshal(map[string]any{"confirm": true, "path": save, "size": preview.Size, "modTime": preview.ModTime})
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard-hold", "test-token", string(badPath), nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("invalid_hold")) {
		t.Fatalf("invalid = %d %s", status, body)
	}
	changed, err := json.Marshal(map[string]any{"confirm": true, "path": hold, "size": preview.Size + 1, "modTime": preview.ModTime})
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard-hold", "test-token", string(changed), nil)
	if status != http.StatusConflict || !bytes.Contains(body, []byte("hold_changed")) {
		t.Fatalf("changed = %d %s", status, body)
	}
	if _, err := os.Stat(hold); err != nil {
		t.Fatal("changed stat removed the hold")
	}
	raw, err := json.Marshal(map[string]any{"confirm": true, "path": hold, "size": preview.Size, "modTime": preview.ModTime})
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard-hold", "test-token", string(raw), nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"action":"discard-hold"`)) {
		t.Fatalf("discard = %d %s", status, body)
	}
	if _, err := os.Stat(hold); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("hold err = %v", err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/discard-hold", "test-token", string(raw), nil)
	if status != http.StatusNotFound {
		t.Fatalf("missing = %d %s", status, body)
	}
}

func TestJournalAPI(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 5)
	_, httpSrv, cfgPath := newAPIOpt(t, 50, Options{Archives: archives})
	writeCfg(t, cfgPath, configJSON(t, save, "kept"))
	path := filepath.Join(archives.JournalDir, ".gmanager-restore-journal-default")
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/journal", "test-token", `{}`, nil)
	if status != http.StatusNotFound || !bytes.Contains(body, []byte("not_found")) {
		t.Fatalf("missing preview = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/journal", "test-token", `{"confirm":true}`, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("op_required")) {
		t.Fatalf("op = %d %s", status, body)
	}
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/journal", "test-token", `{}`, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"action":"journal-preview"`)) || !bytes.Contains(body, []byte(`"authOK":false`)) {
		t.Fatalf("preview = %d %s", status, body)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("preview removed the journal")
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/journal", "test-token", `{"confirm":true,"op":"sideways"}`, nil)
	if status != http.StatusBadRequest || !bytes.Contains(body, []byte("invalid_op")) {
		t.Fatalf("bad op = %d %s", status, body)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/journal", "test-token", `{"confirm":true,"op":"rollback"}`, nil)
	if status != http.StatusConflict || !bytes.Contains(body, []byte(`"journal"`)) {
		t.Fatalf("rollback = %d %s", status, body)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("failed rollback removed the journal")
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/journal", "test-token", `{"confirm":true,"op":"discard"}`, nil)
	if status != http.StatusOK || !bytes.Contains(body, []byte(`"action":"journal-discard"`)) {
		t.Fatalf("discard = %d %s", status, body)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("discarded journal err = %v", err)
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/journal", "test-token", `{"confirm":true,"op":"discard"}`, nil)
	if status != http.StatusNotFound {
		t.Fatalf("second discard = %d %s", status, body)
	}
}

func TestDiscardSwappedJournalAPI(t *testing.T) {
	save := t.TempDir()
	archives := testArchives(t, 5)
	_, httpSrv, cfgPath := newAPIOpt(t, 50, Options{Archives: archives})
	writeCfg(t, cfgPath, configJSON(t, save, "kept"))
	hold := filepath.Join(filepath.Dir(save), ".gmanager-hold-"+filepath.Base(save))
	if err := os.MkdirAll(hold, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hold, "world.dat"), []byte("old-save"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(save, "world.dat"), []byte("new-save"), 0o644); err != nil {
		t.Fatal(err)
	}
	const restoreID = "abc123abc123abc123abc123abc123ab"
	path := filepath.Join(archives.JournalDir, ".gmanager-restore-journal-default")
	if err := os.WriteFile(path, sealedPendingJournal(t, archives.Key, save, hold, restoreID), 0o600); err != nil {
		t.Fatal(err)
	}
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backups/journal", "test-token", `{"confirm":true,"op":"discard"}`, nil)
	if status != http.StatusConflict || !bytes.Contains(body, []byte("rollback_required")) {
		t.Fatalf("discard = %d %s", status, body)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("journal was removed")
	}
	if _, err := os.Stat(hold); err != nil {
		t.Fatal("hold was removed")
	}
	if got, err := os.ReadFile(filepath.Join(save, "world.dat")); err != nil || string(got) != "new-save" {
		t.Fatalf("world = %q %v", got, err)
	}
	if got, err := os.ReadFile(filepath.Join(hold, "world.dat")); err != nil || string(got) != "old-save" {
		t.Fatalf("hold world = %q %v", got, err)
	}
}

type apiJournalTarget struct {
	Dest    string `json:"dest"`
	Hold    string `json:"hold,omitempty"`
	Dir     bool   `json:"dir"`
	Swapped bool   `json:"swapped,omitempty"`
}

type apiJournalFile struct {
	Version   int                `json:"version"`
	State     string             `json:"state"`
	Instance  string             `json:"instance"`
	RestoreID string             `json:"restoreID"`
	Targets   []apiJournalTarget `json:"targets"`
	HMAC      string             `json:"hmac,omitempty"`
}

func sealedPendingJournal(t *testing.T, key []byte, save, hold, restoreID string) []byte {
	t.Helper()
	j := apiJournalFile{
		Version:   1,
		State:     "pending",
		Instance:  "default",
		RestoreID: restoreID,
		Targets:   []apiJournalTarget{{Dest: save, Hold: hold, Dir: true, Swapped: true}},
	}
	mac, err := json.Marshal(j)
	if err != nil {
		t.Fatal(err)
	}
	sum := hmac.New(sha256.New, key)
	if _, err := sum.Write(mac); err != nil {
		t.Fatal(err)
	}
	j.HMAC = hex.EncodeToString(sum.Sum(nil))
	raw, err := json.MarshalIndent(j, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func TestUserDirOverrideWinsOverConfig(t *testing.T) {
	flagDir := t.TempDir()
	configDir := t.TempDir()
	archives := testArchives(t, 5)
	_, httpSrv, cfgPath := newAPIOpt(t, 50, Options{Archives: archives, UserDir: flagDir})
	writeCfg(t, cfgPath, configJSON(t, configDir, "kept"))
	writeSaveFile(t, flagDir, "world.dat", "from-flag")
	writeSaveFile(t, configDir, "world.dat", "from-config")
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	succeededResult(t, httpSrv.URL, body)
	writeSaveFile(t, flagDir, "world.dat", "changed")
	entries, err := os.ReadDir(archives.Dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("zips = %v %v", entries, err)
	}
	id := entries[0].Name()
	id = id[:len(id)-len(".zip")]
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/restore", "test-token", `{"confirm":true,"id":"`+id+`"}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("restore = %d %s", status, body)
	}
	if job := jobByID(t, httpSrv.URL, jsonString(t, body, "jobId")); !bytes.Contains(job, []byte(`"state":"succeeded"`)) {
		t.Fatalf("job = %s", job)
	}
	if readSaveFile(t, flagDir, "world.dat") != "from-flag" {
		t.Fatalf("flag dir = %q", readSaveFile(t, flagDir, "world.dat"))
	}
	if readSaveFile(t, configDir, "world.dat") != "from-config" {
		t.Fatalf("config dir = %q", readSaveFile(t, configDir, "world.dat"))
	}
}

func TestBackupRequiresAKnownSavePath(t *testing.T) {
	archives := testArchives(t, 5)
	steam := &recordingSteam{exe: []byte("not-the-game")}
	_, httpSrv, cfgPath := newAPIOpt(t, 100, Options{Archives: archives, Steam: steam})
	writeCfg(t, cfgPath, "{\"serverName\":\"kept\"}\n")
	status, body, _ := do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("backup = %d %s", status, body)
	}
	jobBody := jobByID(t, httpSrv.URL, jsonString(t, body, "jobId"))
	if !bytes.Contains(jobBody, []byte(`"state":"failed"`)) || !bytes.Contains(jobBody, []byte("save path unknown")) {
		t.Fatalf("job = %s", jobBody)
	}
	entries, err := os.ReadDir(archives.Dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("zips = %d", len(entries))
	}
	status, body, _ = do(t, httpSrv.URL, http.MethodPost, "/api/v1/servers/default/update", "test-token", `{}`, nil)
	if status != http.StatusAccepted {
		t.Fatalf("update = %d %s", status, body)
	}
	jobBody = jobByID(t, httpSrv.URL, jsonString(t, body, "jobId"))
	if !bytes.Contains(jobBody, []byte(`"state":"failed"`)) || !bytes.Contains(jobBody, []byte("save path unknown")) {
		t.Fatalf("update job = %s", jobBody)
	}
	if steam.calls != 0 {
		t.Fatalf("steam calls = %d", steam.calls)
	}

	_, bare, _ := newAPI(t, 20)
	status, body, _ = do(t, bare.URL, http.MethodGet, "/api/v1/servers/default/backups", "test-token", "", nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("list without store = %d %s", status, body)
	}
	status, body, _ = do(t, bare.URL, http.MethodPost, "/api/v1/servers/default/backup", "test-token", `{}`, nil)
	if status != http.StatusNotImplemented {
		t.Fatalf("backup without store = %d %s", status, body)
	}
}

type backupPage struct {
	BackupDir  string         `json:"backupDir"`
	Keep       int            `json:"keep"`
	Items      []backup.Entry `json:"items"`
	NextCursor string         `json:"nextCursor"`
}

func succeededResult(t *testing.T, base string, accepted []byte) map[string]any {
	t.Helper()
	raw := jobByID(t, base, jsonString(t, accepted, "jobId"))
	var job struct {
		State  string         `json:"state"`
		Error  string         `json:"error"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(raw, &job); err != nil {
		t.Fatal(err)
	}
	if job.State != string(JobSucceeded) {
		t.Fatalf("job = %s", raw)
	}
	return job.Result
}

func jobText(t *testing.T, base string, accepted []byte) string {
	t.Helper()
	return string(jobByID(t, base, jsonString(t, accepted, "jobId")))
}

func configJSON(t *testing.T, save, name string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{
		"serverName": name,
		"password":   "placeholder-password",
		"userdir":    save,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

func writeSaveFile(t *testing.T, root, name, body string) {
	t.Helper()
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readSaveFile(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func installFake(t *testing.T, dir string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, plainsofpain.Executable), fakeBytes(t), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fakeserver.json"), []byte(`{"readyDelay":"20ms"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func fakeBytes(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile(fakepop.Binary(t))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func testArchives(t *testing.T, keep int) *backup.Store {
	t.Helper()
	return &backup.Store{Dir: t.TempDir(), JournalDir: t.TempDir(), Keep: keep, Key: bytes.Repeat([]byte{3}, 32)}
}

func flipZip(t *testing.T, path string) {
	t.Helper()
	r, err := zip.OpenReader(path)
	if err != nil {
		t.Fatal(err)
	}
	var off int64
	found := false
	for _, f := range r.File {
		if f.Name == "manifest.json" || f.UncompressedSize64 == 0 {
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
		t.Fatal("no payload to corrupt")
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
