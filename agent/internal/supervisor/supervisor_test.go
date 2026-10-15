package supervisor

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

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

func TestStartDoesNotRewriteConfig(t *testing.T) {
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
	launch, err := (Supervisor{}).Start(core.Instance{ID: "default", Dir: dir}, mod)
	if err != nil {
		t.Fatal(err)
	}
	if launch.Exe != filepath.Join(dir, "Game.exe") {
		t.Fatalf("exe = %s", launch.Exe)
	}
	if mod.validateCalls != 0 {
		t.Fatalf("Start called Validate %d times", mod.validateCalls)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("config changed on start\n got %s\nwant %s", got, original)
	}
	if !core.LineReady(mod, "READY") {
		t.Fatal("ready matcher was not the module's")
	}
}
