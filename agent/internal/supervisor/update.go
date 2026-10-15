package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

// ErrBackupNotConfigured means update refused to continue without a backup hook.
var ErrBackupNotConfigured = errors.New("backup hook is not configured")

// AppUpdater runs an anonymous SteamCMD app update. Tests substitute a double.
// The real client lives in the steamcmd package so this package does not
// download Steam.
type AppUpdater interface {
	AppUpdate(ctx context.Context, appID int, installDir string, validate, anonymous bool, log func(string)) error
}

// BackupFunc runs before an update replaces server files.
// A nil hook fails closed. The supervisor does not invent a backup.
type BackupFunc func(ctx context.Context, inst core.Instance) error

// Install downloads the dedicated server into the instance directory and
// checks that the module executable exists. It does not start the server
// and it does not take a backup.
func (s *Supervisor) Install(ctx context.Context, updater AppUpdater) error {
	if updater == nil {
		return errors.New("updater is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.mod == nil || s.inst.Dir == "" {
		s.mu.Unlock()
		return errors.New("no instance")
	}
	switch s.state {
	case StateStarting, StateRunning, StateUpdating:
		s.mu.Unlock()
		return fmt.Errorf("stop the server before install (%s)", s.state)
	}
	spec := s.mod.Install()
	dir := s.inst.Dir
	exe := s.mod.ExePath(s.inst)
	s.updating = true
	s.state = StateUpdating
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.updating = false
		if s.state == StateUpdating {
			s.state = StateStopped
		}
		s.mu.Unlock()
	}()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := updater.AppUpdate(ctx, spec.AppID, dir, spec.Validate, spec.Anonymous, s.log); err != nil {
		return err
	}
	info, err := os.Stat(exe)
	if err != nil || info.IsDir() {
		return fmt.Errorf("install finished but %s is missing", exe)
	}
	return nil
}

// Update stops a running server, runs the backup hook, snapshots game files
// except module config files, applies the update, and starts. A failed update
// or a server that does not become ready restores the snapshot. When the
// server was running, the previous build is started again.
func (s *Supervisor) Update(ctx context.Context, updater AppUpdater, backup BackupFunc) error {
	if updater == nil {
		return errors.New("updater is required")
	}
	if backup == nil {
		return ErrBackupNotConfigured
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.mod == nil || s.inst.Dir == "" {
		s.mu.Unlock()
		return errors.New("no instance")
	}
	if s.state == StateUpdating {
		s.mu.Unlock()
		return errors.New("update already in progress")
	}
	wasRunning := s.state == StateRunning || s.state == StateStarting
	inst := s.inst
	mod := s.mod
	spec := mod.Install()
	s.updating = true
	s.state = StateUpdating
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		s.updating = false
		if s.state == StateUpdating {
			s.state = StateStopped
		}
		s.mu.Unlock()
	}()

	if wasRunning {
		if err := s.Stop(ctx); err != nil {
			s.mu.Lock()
			if s.pid > 0 && s.state == StateUpdating {
				s.state = StateRunning
			}
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Lock()
	s.updating = true
	s.stopReq = true
	if s.pid == 0 {
		s.state = StateStopped
	}
	s.mu.Unlock()

	if err := backup(ctx, inst); err != nil {
		return s.recoverPrevious(ctx, inst, mod, wasRunning, err)
	}
	s.mu.Lock()
	s.updating = true
	s.state = StateUpdating
	s.mu.Unlock()
	preserve := preservedRel(mod, inst)
	snap, err := snapshotTree(inst.Dir, preserve)
	if err != nil {
		return s.recoverPrevious(ctx, inst, mod, wasRunning, err)
	}
	defer func() {
		if removeErr := os.RemoveAll(snap); removeErr != nil && s.log != nil {
			s.log("remove snapshot: " + removeErr.Error())
		}
	}()
	if err := updater.AppUpdate(ctx, spec.AppID, inst.Dir, spec.Validate, spec.Anonymous, s.log); err != nil {
		return s.rollback(ctx, snap, preserve, inst, mod, wasRunning, err)
	}
	if err := s.start(ctx, inst, mod, true); err != nil {
		return s.rollback(ctx, snap, preserve, inst, mod, wasRunning, err)
	}
	return nil
}

func (s *Supervisor) recoverPrevious(ctx context.Context, inst core.Instance, mod core.GameModule, wasRunning bool, cause error) error {
	if !wasRunning {
		return cause
	}
	if err := s.start(ctx, inst, mod, true); err != nil {
		return fmt.Errorf("%w; restarting the previous build failed: %v", cause, err)
	}
	return cause
}

func (s *Supervisor) rollback(ctx context.Context, snap string, preserve map[string]struct{}, inst core.Instance, mod core.GameModule, wasRunning bool, cause error) error {
	s.mu.Lock()
	alive := s.pid != 0
	s.mu.Unlock()
	if alive {
		stopCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		stopErr := s.Stop(stopCtx)
		cancel()
		if stopErr != nil {
			return fmt.Errorf("update failed: %v; stop before rollback failed: %w", cause, stopErr)
		}
	}
	s.mu.Lock()
	s.updating = true
	s.state = StateUpdating
	s.mu.Unlock()
	if err := restoreTree(inst.Dir, snap, preserve); err != nil {
		return fmt.Errorf("update failed: %v; restore failed: %w", cause, err)
	}
	if wasRunning {
		if err := s.start(ctx, inst, mod, true); err != nil {
			return fmt.Errorf("update failed: %v; previous build was restored but did not become ready: %w", cause, err)
		}
	}
	return fmt.Errorf("update failed and the previous build was restored: %w", cause)
}
