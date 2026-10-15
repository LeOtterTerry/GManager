// Package core is the game-agnostic center of the agent.
// Supervisors, backups, and the API call GameModule. They do not branch on a game.
package core

import (
	"errors"
	"regexp"
	"time"
)

// ErrSavePathUnknown means the module will not name a save directory.
// Callers must not substitute a guessed path.
var ErrSavePathUnknown = errors.New("save path unknown")

// GameModule is the only game-specific surface the core is allowed to call.
// v0.1 compiles modules into one binary. There is no runtime plugin loader.
type GameModule interface {
	ID() string
	DisplayName() string

	Install() InstallSpec
	ExePath(inst Instance) string

	LaunchArgs(inst Instance) []string
	StopStrategy() StopSpec

	ConfigFiles(inst Instance) []ConfigFile
	Validate(cfg []byte) []FieldError

	ReadyMatcher() *regexp.Regexp
	CrashSignals() []*regexp.Regexp

	// SavePaths returns directories to back up.
	// A non-nil error means the location is not known. Do not guess.
	SavePaths(inst Instance) ([]string, error)

	Ports(cfg []byte) []PortSpec
	Warnings(cfg []byte) []string
}

// Instance is one game server directory. The model allows more than one;
// v0.1 runs a single instance.
type Instance struct {
	ID  string
	Dir string
	// UserDir is the save directory from the game config when one is set.
	// Empty means the game default. For Plains of Pain that default is
	// unverified — leave this empty rather than inventing a path.
	UserDir string
	// EnableConsoleCommands opts into a module's console-command flag.
	// Plains of Pain's -cc flag is unverified on Windows. Leave false until
	// that is confirmed on a real install.
	EnableConsoleCommands bool
}

// InstallSpec describes how the core should obtain the dedicated server.
type InstallSpec struct {
	Kind      string
	AppID     int
	Anonymous bool
	Validate  bool
}

// StopMethod is how a module asks its process to stop cleanly.
// The empty method means no clean-stop mechanism is confirmed.
type StopMethod string

const (
	// StopUnknown means the core must not invent a graceful stop
	// (no guessed signal, window message, or stdin command).
	StopUnknown StopMethod = ""
	// StopInterrupt asks the OS to interrupt the process (SIGTERM on Unix).
	// This is not a confirmed Plains of Pain clean stop. Leave the module on
	// StopUnknown until a real install shows a method that works.
	StopInterrupt StopMethod = "interrupt"
)

// StopSpec is the module's stop policy.
type StopSpec struct {
	Method StopMethod
	// Timeout is how long to wait after a graceful request before kill.
	// It is zero when Method is StopUnknown: there is no request to wait on.
	Timeout time.Duration
	// ThenKill reports that the process may be terminated after the graceful
	// attempt. When Method is unknown, termination is immediate and the
	// data-loss risk stands until a clean stop is verified.
	ThenKill bool
}

// ConfigFile is one config the editor knows how to load.
type ConfigFile struct {
	Name    string
	RelPath string
	Format  string
	Schema  string
}

// FieldError is one config validation failure.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// PortSpec is a network port the game server binds.
type PortSpec struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
	Port     int    `json:"port"`
}

// LineReady reports whether a log line matches the module's ready pattern.
func LineReady(mod GameModule, line string) bool {
	if mod == nil {
		return false
	}
	re := mod.ReadyMatcher()
	if re == nil {
		return false
	}
	return re.MatchString(line)
}
