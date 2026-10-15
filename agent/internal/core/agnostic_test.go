package core

import (
	"regexp"
	"testing"
)

// fakeModule is a stand-in game. The core tests below only see GameModule.
type fakeModule struct {
	id    string
	exe   string
	args  []string
	ready *regexp.Regexp
}

func (f fakeModule) ID() string                   { return f.id }
func (f fakeModule) DisplayName() string          { return f.id }
func (f fakeModule) Install() InstallSpec         { return InstallSpec{Kind: "none"} }
func (f fakeModule) ExePath(Instance) string      { return f.exe }
func (f fakeModule) LaunchArgs(Instance) []string { return append([]string(nil), f.args...) }
func (f fakeModule) StopStrategy() StopSpec       { return StopSpec{Method: StopUnknown, ThenKill: true} }
func (f fakeModule) ConfigFiles(Instance) []ConfigFile {
	return []ConfigFile{{Name: "game.json", RelPath: "game.json", Format: "json"}}
}
func (f fakeModule) Validate([]byte) []FieldError   { return nil }
func (f fakeModule) ReadyMatcher() *regexp.Regexp   { return f.ready }
func (f fakeModule) CrashSignals() []*regexp.Regexp { return nil }
func (f fakeModule) SavePaths(Instance) ([]string, error) {
	return nil, ErrSavePathUnknown
}
func (f fakeModule) ConfigSaveDir([]byte) (string, bool) { return "", false }
func (f fakeModule) Ports([]byte) []PortSpec             { return nil }
func (f fakeModule) Warnings([]byte) []string {
	return nil
}

// planLaunch is the kind of call the supervisor makes: interface only.
func planLaunch(mod GameModule, inst Instance) (exe string, args []string) {
	return mod.ExePath(inst), mod.LaunchArgs(inst)
}

func TestCoreTreatsModulesAsInterchangeable(t *testing.T) {
	alpha := fakeModule{
		id:    "alpha",
		exe:   "alpha.exe",
		args:  []string{"--alpha"},
		ready: regexp.MustCompile(`ALPHA_READY`),
	}
	beta := fakeModule{
		id:    "beta",
		exe:   "beta.exe",
		args:  []string{"--beta"},
		ready: regexp.MustCompile(`BETA_READY`),
	}

	exe, args := planLaunch(alpha, Instance{Dir: "/servers/a"})
	if exe != "alpha.exe" || len(args) != 1 || args[0] != "--alpha" {
		t.Fatalf("alpha plan = %s %v", exe, args)
	}
	if !LineReady(alpha, "log ALPHA_READY") {
		t.Fatal("alpha ready line was not detected")
	}
	if LineReady(alpha, "log BETA_READY") {
		t.Fatal("alpha treated another module's ready line as its own")
	}

	exe, args = planLaunch(beta, Instance{Dir: "/servers/b"})
	if exe != "beta.exe" || len(args) != 1 || args[0] != "--beta" {
		t.Fatalf("beta plan = %s %v", exe, args)
	}
	if !LineReady(beta, "BETA_READY") || LineReady(beta, "ALPHA_READY") {
		t.Fatal("beta ready matcher was not used")
	}

	if _, err := alpha.SavePaths(Instance{}); err != ErrSavePathUnknown {
		t.Fatalf("unknown save path error = %v", err)
	}
}
