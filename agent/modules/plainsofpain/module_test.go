package plainsofpain

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

func TestInstallAndLaunch(t *testing.T) {
	mod := New()
	if mod.ID() != "plains-of-pain" || mod.DisplayName() != "Plains of Pain" {
		t.Fatalf("id=%s name=%s", mod.ID(), mod.DisplayName())
	}
	spec := mod.Install()
	if spec.Kind != "steamcmd" || spec.AppID != 2227360 || !spec.Anonymous || !spec.Validate {
		t.Fatalf("install = %+v", spec)
	}

	inst := core.Instance{Dir: filepath.Join("servers", "default")}
	if got := mod.ExePath(inst); got != filepath.Join("servers", "default", "PlainsOfPain.exe") {
		t.Fatalf("exe = %s", got)
	}
	args := mod.LaunchArgs(inst)
	want := []string{"-nographics", "-batchmode", "-ignorecompilererrors", "-config", "configs/server.json"}
	if len(args) != len(want) {
		t.Fatalf("args = %v", args)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Fatalf("args = %v", args)
		}
	}
	for _, arg := range args {
		if arg == "-cc" {
			t.Fatal("launch args include -cc by default")
		}
	}

	inst.EnableConsoleCommands = true
	args = mod.LaunchArgs(inst)
	if args[len(args)-1] != "-cc" {
		t.Fatalf("opt-in args = %v", args)
	}
}

func TestReadyLine(t *testing.T) {
	mod := New()
	cases := []struct {
		line string
		want bool
	}{
		{"Set Game State: RUNNING", true},
		{"[info] Set Game State: RUNNING", true},
		{"Set Game State: RUNNING\r", true},
		{"Set Game State: LOADING", false},
		{"Set Game State: RUNNINGNOW", false},
		{"set game state: running", false},
	}
	for _, tc := range cases {
		if got := core.LineReady(mod, tc.line); got != tc.want {
			t.Errorf("LineReady(%q) = %v, want %v", tc.line, got, tc.want)
		}
	}
	if mod.CrashSignals() != nil {
		t.Fatal("crash signals are not verified; got a pattern")
	}
}

func TestStopAndSaveStayUnresolved(t *testing.T) {
	mod := New()
	stop := mod.StopStrategy()
	if stop.Method != core.StopUnknown || stop.Timeout != 0 || !stop.ThenKill {
		t.Fatalf("stop = %+v", stop)
	}
	if _, err := mod.SavePaths(core.Instance{}); err != core.ErrSavePathUnknown {
		t.Fatalf("empty userdir err = %v", err)
	}
	got, err := mod.SavePaths(core.Instance{UserDir: `D:\saves\placeholder`})
	if err != nil || len(got) != 1 || got[0] != `D:\saves\placeholder` {
		t.Fatalf("paths=%v err=%v", got, err)
	}
}

func TestUserDirHelper(t *testing.T) {
	if _, ok := UserDir([]byte(`{"serverName":"A"}`)); ok {
		t.Fatal("missing userdir reported as set")
	}
	got, ok := UserDir([]byte(`{"userdir":"C:/GManager/saves/placeholder"}`))
	if !ok || got != "C:/GManager/saves/placeholder" {
		t.Fatalf("userdir = %q ok=%v", got, ok)
	}
	if _, ok := UserDir([]byte(`{"userdir":""}`)); ok {
		t.Fatal("empty userdir reported as set")
	}
}

func TestPortsAndConfigFile(t *testing.T) {
	mod := New()
	files := mod.ConfigFiles(core.Instance{})
	if len(files) != 1 || files[0].RelPath != "configs/server.json" || files[0].Format != "json" {
		t.Fatalf("files = %+v", files)
	}
	ports := mod.Ports([]byte(`{}`))
	if len(ports) != 2 || ports[0].Port != 7777 || ports[0].Protocol != "udp" || ports[1].Port != 27016 {
		t.Fatalf("default ports = %+v", ports)
	}
	ports = mod.Ports([]byte(`{"port":7778,"queryPort":27017}`))
	if ports[0].Port != 7778 || ports[1].Port != 27017 {
		t.Fatalf("custom ports = %+v", ports)
	}
}

func TestValidFixture(t *testing.T) {
	body, err := os.ReadFile("fixtures/server.valid.json")
	if err != nil {
		t.Fatal(err)
	}
	mod := New()
	if errs := mod.Validate(body); len(errs) != 0 {
		t.Fatalf("fixture errors = %+v", errs)
	}
	if w := mod.Warnings(body); len(w) != 0 {
		t.Fatalf("fixture warnings = %v", w)
	}
	if bytesContainSteamID(body) {
		t.Fatal("fixture contains a real-looking Steam ID")
	}
}

func bytesContainSteamID(b []byte) bool {
	// 7656119 is the public prefix of modern SteamID64 values. The fixture
	// must keep using a placeholder instead.
	for i := 0; i+7 <= len(b); i++ {
		if string(b[i:i+7]) == "7656119" {
			return true
		}
	}
	return false
}
