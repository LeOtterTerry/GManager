// Package supervisor builds a launch plan from a GameModule.
// Process creation (Windows Job Object) is a later slice. Start does not
// read or write config files: the agent must not rewrite config on server start.
package supervisor

import "github.com/LeOtterTerry/GManager/agent/internal/core"

// Launch is the command the process supervisor would start.
type Launch struct {
	Exe  string
	Args []string
}

// Supervisor is the game-agnostic process host.
type Supervisor struct{}

// Start returns the module's launch plan.
// It intentionally does not open config files.
func (Supervisor) Start(inst core.Instance, mod core.GameModule) (Launch, error) {
	return Launch{
		Exe:  mod.ExePath(inst),
		Args: mod.LaunchArgs(inst),
	}, nil
}
