// Package plainsofpain is the Plains of Pain dedicated-server module.
//
// Verified facts used here are limited to the v0.1 architecture note:
// SteamCMD app 2227360 (anonymous), the Windows launch arguments, the ready
// line "Set Game State: RUNNING", and the server.json field rules that are
// already known. Save path, clean stop, and the -cc flag stay unresolved.
package plainsofpain

import (
	_ "embed"
	"path/filepath"
	"regexp"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

const (
	// ModuleID is the stable module name.
	ModuleID = "plains-of-pain"
	// SteamAppID is the dedicated server's SteamCMD app id.
	SteamAppID = 2227360
	// Executable is the dedicated server binary name.
	Executable = "PlainsOfPain.exe"
	// ConfigRelPath is the config path passed to -config, relative to the install.
	ConfigRelPath = "configs/server.json"
	// ReadyLine is the log text that means the server is up.
	ReadyLine = "Set Game State: RUNNING"

	// DefaultGamePort is the UDP game port. Non-default values warn.
	DefaultGamePort = 7777
	// DefaultQueryPort is the UDP query port. Non-default values warn.
	DefaultQueryPort = 27016

	// PasswordMaxRunes is the longest accepted password.
	PasswordMaxRunes = 20
	// MaxPlayersMin is the smallest accepted maxPlayers.
	MaxPlayersMin = 1
	// MaxPlayersMax is the largest accepted maxPlayers.
	MaxPlayersMax = 200
)

//go:embed schema/server.schema.json
var schemaJSON []byte

// readyMatcher matches the verified ready line and not a longer token such as
// "RUNNINGNOW". A timestamp prefix on the same line is allowed.
var readyMatcher = regexp.MustCompile(regexp.QuoteMeta(ReadyLine) + `(?:[^A-Za-z0-9]|$)`)

// Module implements core.GameModule for Plains of Pain.
type Module struct{}

// New returns the Plains of Pain module.
func New() *Module { return &Module{} }

var _ core.GameModule = (*Module)(nil)

func (m *Module) ID() string { return ModuleID }

func (m *Module) DisplayName() string { return "Plains of Pain" }

func (m *Module) Install() core.InstallSpec {
	return core.InstallSpec{
		Kind:      "steamcmd",
		AppID:     SteamAppID,
		Anonymous: true,
		Validate:  true,
	}
}

func (m *Module) ExePath(inst core.Instance) string {
	return filepath.Join(inst.Dir, Executable)
}

func (m *Module) LaunchArgs(inst core.Instance) []string {
	args := []string{
		"-nographics",
		"-batchmode",
		"-ignorecompilererrors",
		"-config",
		ConfigRelPath,
	}
	// TODO(pop): whether -cc works on Windows is UNVERIFIED. It stays off
	// unless this instance opts in. Do not add it by default.
	if inst.EnableConsoleCommands {
		args = append(args, "-cc")
	}
	return args
}

func (m *Module) StopStrategy() core.StopSpec {
	// TODO(pop): clean stop on Windows is UNVERIFIED. Method stays unknown.
	// The supervisor must not invent a signal, window message, or stdin command.
	// ThenKill is the architecture's fallback and can lose data until a clean
	// stop is confirmed.
	return core.StopSpec{Method: core.StopUnknown, Timeout: 0, ThenKill: true}
}

func (m *Module) ConfigFiles(core.Instance) []core.ConfigFile {
	return []core.ConfigFile{{
		Name:    "server.json",
		RelPath: ConfigRelPath,
		Format:  "json",
		Schema:  "schema/server.schema.json",
	}}
}

func (m *Module) ReadyMatcher() *regexp.Regexp { return readyMatcher }

func (m *Module) CrashSignals() []*regexp.Regexp { return nil }

func (m *Module) SavePaths(inst core.Instance) ([]string, error) {
	// TODO(pop): the default save path is UNVERIFIED. Do not guess one.
	// Backups use userdir when it is set, and fail closed when it is not.
	if inst.UserDir == "" {
		return nil, core.ErrSavePathUnknown
	}
	return []string{inst.UserDir}, nil
}

// Schema returns the embedded JSON Schema for server.json.
func (m *Module) Schema() []byte { return schemaJSON }
