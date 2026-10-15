package supervisor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/internal/fakepop"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func testPolicy() Policy {
	return Policy{
		CrashLimit:   3,
		CrashWindow:  time.Minute,
		Backoff:      []time.Duration{40 * time.Millisecond, 40 * time.Millisecond, 40 * time.Millisecond},
		ReadyTimeout: 3 * time.Second,
		StopTimeout:  500 * time.Millisecond,
	}
}

func TestPlanAndMissingExeDoNotTouchConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "configs", "server.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\n  \"serverName\": \"kept-by-hand\"\n}\n")
	if err := os.WriteFile(path, original, 0o644); err != nil {
		t.Fatal(err)
	}
	mod := &recordingModule{ready: regexp.MustCompile(`READY`)}
	sup := New(testPolicy(), nil)
	launch := sup.Plan(core.Instance{ID: "default", Dir: dir}, mod)
	if launch.Exe != filepath.Join(dir, "Game.exe") {
		t.Fatalf("exe = %s", launch.Exe)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := sup.Start(ctx, core.Instance{ID: "default", Dir: dir}, mod); err == nil {
		t.Fatal("missing executable started")
	}
	if mod.validateCalls != 0 {
		t.Fatalf("Validate calls = %d", mod.validateCalls)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("config changed\n%s", got)
	}
}

func TestFakeReachesReadyAndStopDoesNotRestart(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"20ms"}`)
	original := readConfig(t, inst)
	sup := New(testPolicy(), nil)
	t.Cleanup(func() { stopNow(t, sup) })
	if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	st := sup.Status()
	if st.State != StateRunning || !st.Ready || st.PID == 0 {
		t.Fatalf("status = %+v", st)
	}
	if runtime.GOOS == "linux" && (!st.MemoryKnown || st.MemoryBytes == 0) {
		t.Fatalf("memory = %+v", st)
	}
	started := time.Now()
	if err := sup.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("stop took %s", time.Since(started))
	}
	time.Sleep(150 * time.Millisecond)
	st = sup.Status()
	if st.State != StateStopped || st.Ready || st.Restarts != 0 || st.PID != 0 {
		t.Fatalf("after stop = %+v", st)
	}
	if !bytes.Equal(readConfig(t, inst), original) {
		t.Fatal("config changed")
	}
}

func TestCrashBackoffThenHalt(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"20ms","crashAfter":"180ms"}`)
	pol := testPolicy()
	sup := New(pol, nil)
	t.Cleanup(func() { stopNow(t, sup) })
	if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		st := sup.Status()
		if st.State == StateCrashed {
			if st.Restarts != 2 || !strings.Contains(st.Message, "halted") {
				t.Fatalf("status = %+v", st)
			}
			if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
				t.Fatal(err)
			}
			if sup.Status().State != StateRunning {
				t.Fatalf("manual start = %+v", sup.Status())
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("did not halt: %+v", sup.Status())
}

func TestExitBeforeReadyHalts(t *testing.T) {
	inst := newFake(t, `{"exitBeforeReady":true}`)
	pol := testPolicy()
	pol.CrashLimit = 1
	sup := New(pol, nil)
	t.Cleanup(func() { stopNow(t, sup) })
	err := sup.Start(context.Background(), inst, plainsofpain.New())
	if err == nil || !strings.Contains(err.Error(), "halted") {
		t.Fatalf("err = %v", err)
	}
	st := sup.Status()
	if st.State != StateCrashed {
		t.Fatalf("status = %+v", st)
	}
}

func TestHangIsKilledWithoutInterrupt(t *testing.T) {
	inst := newFake(t, `{"hang":true,"ignoreStop":true}`)
	pol := testPolicy()
	pol.ReadyTimeout = 250 * time.Millisecond
	sup := New(pol, nil)
	t.Cleanup(func() { stopNow(t, sup) })
	started := time.Now()
	err := sup.Start(context.Background(), inst, plainsofpain.New())
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("hang became ready")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("kill waited %s", elapsed)
	}
	st := sup.Status()
	if st.State == StateRunning || st.Ready || st.PID != 0 {
		t.Fatalf("status = %+v", st)
	}
}

func TestBackoffCapsAt30s(t *testing.T) {
	sup := New(Policy{Backoff: []time.Duration{time.Minute}}, nil)
	if got := sup.backoffDelay(0); got != maxBackoff {
		t.Fatalf("backoff = %s", got)
	}
}

type recordingModule struct {
	validateCalls int
	ready         *regexp.Regexp
}

func (m *recordingModule) ID() string          { return "recording" }
func (m *recordingModule) DisplayName() string { return "Recording" }
func (m *recordingModule) Install() core.InstallSpec {
	return core.InstallSpec{Kind: "none"}
}
func (m *recordingModule) ExePath(inst core.Instance) string {
	return filepath.Join(inst.Dir, "Game.exe")
}
func (m *recordingModule) LaunchArgs(core.Instance) []string {
	return []string{"-config", "configs/server.json"}
}
func (m *recordingModule) StopStrategy() core.StopSpec {
	return core.StopSpec{Method: core.StopUnknown, ThenKill: true}
}
func (m *recordingModule) ConfigFiles(core.Instance) []core.ConfigFile {
	return []core.ConfigFile{{Name: "server.json", RelPath: "configs/server.json", Format: "json"}}
}
func (m *recordingModule) Validate([]byte) []core.FieldError {
	m.validateCalls++
	return nil
}
func (m *recordingModule) ReadyMatcher() *regexp.Regexp   { return m.ready }
func (m *recordingModule) CrashSignals() []*regexp.Regexp { return nil }
func (m *recordingModule) SavePaths(core.Instance) ([]string, error) {
	return nil, core.ErrSavePathUnknown
}
func (m *recordingModule) Ports([]byte) []core.PortSpec { return nil }
func (m *recordingModule) Warnings([]byte) []string     { return nil }

func newFake(t *testing.T, behavior string) core.Instance {
	t.Helper()
	dir := t.TempDir()
	src := fakepop.Binary(t)
	dst := filepath.Join(dir, plainsofpain.Executable)
	if err := copyFile(src, dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := []byte("{\n  \"serverName\": \"keep-me\"\n}\n")
	if err := os.WriteFile(filepath.Join(dir, "configs", "server.json"), body, 0o644); err != nil {
		t.Fatal(err)
	}
	if behavior != "" {
		if err := os.WriteFile(filepath.Join(dir, "fakeserver.json"), []byte(behavior), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return core.Instance{ID: "default", Dir: dir}
}

func readConfig(t *testing.T, inst core.Instance) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(inst.Dir, "configs", "server.json"))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func stopNow(t *testing.T, sup *Supervisor) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sup.Stop(ctx); err != nil {
		t.Errorf("cleanup stop: %v", err)
	}
}

type scriptedSteam struct {
	calls      int
	appID      int
	installDir string
	validate   bool
	anonymous  bool
	err        error
	before     func(dir string)
	lines      []string
}

func (s *scriptedSteam) AppUpdate(_ context.Context, appID int, installDir string, validate, anonymous bool, log func(string)) error {
	s.calls++
	s.appID = appID
	s.installDir = installDir
	s.validate = validate
	s.anonymous = anonymous
	if log != nil {
		for _, line := range s.lines {
			log(line)
		}
	}
	if s.before != nil {
		s.before(installDir)
	}
	return s.err
}

func TestUpdateRollsBackWhenSteamFails(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"20ms"}`)
	original := readConfig(t, inst)
	sup := New(testPolicy(), nil)
	t.Cleanup(func() { stopNow(t, sup) })
	if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	steam := &scriptedSteam{err: errors.New("steam broke"), lines: []string{"steam log"}}
	var sawStopped bool
	backup := func(context.Context, core.Instance) error {
		st := sup.Status()
		sawStopped = st.State == StateStopped && st.PID == 0
		return nil
	}
	err := sup.Update(context.Background(), steam, backup)
	if err == nil || !strings.Contains(err.Error(), "steam broke") {
		t.Fatalf("err = %v", err)
	}
	if !sawStopped {
		t.Fatal("backup hook did not see a stopped server")
	}
	if steam.calls != 1 || steam.appID != plainsofpain.SteamAppID || !steam.validate || !steam.anonymous {
		t.Fatalf("steam = %+v", steam)
	}
	st := sup.Status()
	if st.State != StateRunning || !st.Ready {
		t.Fatalf("previous build = %+v", st)
	}
	if !bytes.Equal(readConfig(t, inst), original) {
		t.Fatal("config changed")
	}
}

func TestUpdateRollbackRestoresExecutable(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"20ms"}`)
	original := readConfig(t, inst)
	if err := os.WriteFile(filepath.Join(inst.Dir, "other.dat"), []byte("keep-file"), 0o644); err != nil {
		t.Fatal(err)
	}
	sup := New(testPolicy(), nil)
	t.Cleanup(func() { stopNow(t, sup) })
	if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	steam := &scriptedSteam{before: func(dir string) {
		script := []byte("#!/bin/sh\necho FAILED\nexit 1\n")
		if err := os.WriteFile(filepath.Join(dir, plainsofpain.Executable), script, 0o755); err != nil {
			t.Errorf("replace exe: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "NEWBUILD"), []byte("x"), 0o644); err != nil {
			t.Errorf("marker: %v", err)
		}
	}}
	err := sup.Update(context.Background(), steam, func(context.Context, core.Instance) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "restored") {
		t.Fatalf("err = %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(inst.Dir, "NEWBUILD")); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("marker after rollback: %v", statErr)
	}
	other, err := os.ReadFile(filepath.Join(inst.Dir, "other.dat"))
	if err != nil {
		t.Fatal(err)
	}
	if string(other) != "keep-file" {
		t.Fatalf("other.dat = %s", other)
	}
	st := sup.Status()
	if st.State != StateRunning || !st.Ready {
		t.Fatalf("rolled back status = %+v", st)
	}
	if !bytes.Equal(readConfig(t, inst), original) {
		t.Fatalf("config = %s", readConfig(t, inst))
	}
}

func TestBackupFailureSkipsSteamAndRestarts(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"20ms"}`)
	sup := New(testPolicy(), nil)
	t.Cleanup(func() { stopNow(t, sup) })
	if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	steam := &scriptedSteam{err: errors.New("should not run")}
	err := sup.Update(context.Background(), steam, func(context.Context, core.Instance) error {
		return errors.New("disk full")
	})
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v", err)
	}
	if steam.calls != 0 {
		t.Fatalf("steam calls = %d", steam.calls)
	}
	st := sup.Status()
	if st.State != StateRunning || !st.Ready {
		t.Fatalf("status = %+v", st)
	}
}

func TestNilBackupFailsClosed(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"20ms"}`)
	sup := New(testPolicy(), nil)
	sup.Attach(inst, plainsofpain.New())
	err := sup.Update(context.Background(), &scriptedSteam{}, nil)
	if !errors.Is(err, ErrBackupNotConfigured) {
		t.Fatalf("err = %v", err)
	}
}

func TestInstallWritesExecutableAndRefusesWhileRunning(t *testing.T) {
	fresh := core.Instance{ID: "default", Dir: t.TempDir()}
	var lines []string
	sup := New(testPolicy(), func(line string) { lines = append(lines, line) })
	sup.Attach(fresh, plainsofpain.New())
	src := fakepop.Binary(t)
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	steam := &scriptedSteam{
		lines: []string{"Update state (0x61) downloading"},
		before: func(dir string) {
			if err := os.WriteFile(filepath.Join(dir, plainsofpain.Executable), body, 0o755); err != nil {
				t.Errorf("write exe: %v", err)
			}
		},
	}
	if err := sup.Install(context.Background(), steam); err != nil {
		t.Fatal(err)
	}
	if steam.calls != 1 || steam.appID != 2227360 || !steam.validate || !steam.anonymous {
		t.Fatalf("steam = %+v", steam)
	}
	if _, err := os.Stat(filepath.Join(fresh.Dir, plainsofpain.Executable)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(lines, "\n"), "downloading") {
		t.Fatalf("logs = %v", lines)
	}
	if sup.Status().State != StateStopped {
		t.Fatalf("install started the server: %+v", sup.Status())
	}
	if err := os.WriteFile(filepath.Join(fresh.Dir, "fakeserver.json"), []byte(`{"readyDelay":"20ms"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := sup.Start(context.Background(), fresh, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { stopNow(t, sup) })
	calls := steam.calls
	if err := sup.Install(context.Background(), steam); err == nil {
		t.Fatal("install while running")
	}
	if steam.calls != calls {
		t.Fatal("steam was called while running")
	}
}
