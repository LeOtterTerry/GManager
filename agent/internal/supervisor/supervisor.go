// Package supervisor runs one game server process.
// It does not open or write the game config. Callers that need a launch
// plan without a process use Plan.
package supervisor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
)

const maxBackoff = 30 * time.Second

// State is the process lifecycle the API reports.
type State string

const (
	StateStopped  State = "stopped"
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateCrashed  State = "crashed"
	StateUpdating State = "updating"
)

// Policy is the crash and ready behavior. Zero values are filled by New.
type Policy struct {
	CrashLimit   int
	CrashWindow  time.Duration
	Backoff      []time.Duration
	ReadyTimeout time.Duration
	StopTimeout  time.Duration
}

// DefaultPolicy matches the v0.1 crash rule: 3 unexpected exits in 10
// minutes, then halt. Backoff is 1s, 5s, 15s, and never waits longer than 30s.
func DefaultPolicy() Policy {
	return Policy{
		CrashLimit:   3,
		CrashWindow:  10 * time.Minute,
		Backoff:      []time.Duration{time.Second, 5 * time.Second, 15 * time.Second},
		ReadyTimeout: 2 * time.Minute,
		StopTimeout:  10 * time.Second,
	}
}

// Launch is the command a module asked to run.
type Launch struct {
	Exe  string
	Args []string
}

// Status is a point-in-time view. CPU and memory are set only when the
// host provided a sample.
type Status struct {
	State       State
	Ready       bool
	PID         int
	StartedAt   time.Time
	ReadyAt     time.Time
	Restarts    int
	Message     string
	MemoryBytes uint64
	MemoryKnown bool
	CPUPercent  float64
	CPUKnown    bool
}

// Supervisor is the game-agnostic process host.
// The log callback may run while a process is starting. It must not call
// back into the supervisor.
type Supervisor struct {
	mu       sync.Mutex
	maint    sync.Mutex
	policy   Policy
	log      func(string)
	override core.StopMethod

	inst core.Instance
	mod  core.GameModule

	state     State
	ready     bool
	pid       int
	cmd       *exec.Cmd
	startedAt time.Time
	readyAt   time.Time
	restarts  int
	message   string
	crashes   []time.Time
	resources resourcePoint

	stopReq       bool
	updating      bool
	cancelBackoff chan struct{}
	gen           int
	killFn        func() error
	closeFn       func() error
}

const ioWaitDelay = 5 * time.Second

// New returns a supervisor. log may be nil.
func New(policy Policy, log func(string)) *Supervisor {
	if policy.CrashLimit <= 0 {
		policy.CrashLimit = DefaultPolicy().CrashLimit
	}
	if policy.CrashWindow <= 0 {
		policy.CrashWindow = DefaultPolicy().CrashWindow
	}
	if len(policy.Backoff) == 0 {
		policy.Backoff = DefaultPolicy().Backoff
	}
	if policy.ReadyTimeout <= 0 {
		policy.ReadyTimeout = DefaultPolicy().ReadyTimeout
	}
	if policy.StopTimeout <= 0 {
		policy.StopTimeout = DefaultPolicy().StopTimeout
	}
	return &Supervisor{policy: policy, log: log, state: StateStopped}
}

// SetStopMethod overrides the module stop method.
// StopInterrupt is SIGTERM on Unix. It is not a confirmed Plains of Pain
// clean stop. An empty method uses the module.
func (s *Supervisor) SetStopMethod(method core.StopMethod) {
	s.mu.Lock()
	s.override = method
	s.mu.Unlock()
}

// Attach remembers the instance a later stop, restart, install, or update uses.
func (s *Supervisor) Attach(inst core.Instance, mod core.GameModule) {
	s.mu.Lock()
	s.inst = inst
	s.mod = mod
	if s.state == "" {
		s.state = StateStopped
	}
	s.mu.Unlock()
}

// Plan returns the module launch command. It does not look at the disk.
func (s *Supervisor) Plan(inst core.Instance, mod core.GameModule) Launch {
	return Launch{Exe: mod.ExePath(inst), Args: mod.LaunchArgs(inst)}
}

// Start spawns the server and waits until the module ready line or an error.
// A ready timeout stops the process. An unexpected exit before ready counts
// toward the crash limit.
func (s *Supervisor) Start(ctx context.Context, inst core.Instance, mod core.GameModule) error {
	return s.start(ctx, inst, mod, false)
}

func (s *Supervisor) start(ctx context.Context, inst core.Instance, mod core.GameModule, fromUpdate bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	release, err := s.beginStart(inst, mod, fromUpdate)
	if release != nil {
		close(release)
	}
	if err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, s.policy.ReadyTimeout)
	defer cancel()
	if err := s.waitReady(readyCtx); err != nil {
		s.abandon()
		return err
	}
	return nil
}

// Stop requests the configured stop, then kill when the module allows it.
// A requested stop is not a crash and does not schedule a restart.
func (s *Supervisor) Stop(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	s.stopReq = true
	s.cancelRestartLocked()
	cmd := s.cmd
	killFn := s.killFn
	method, timeout, thenKill := s.stopChoiceLocked()
	if cmd == nil || cmd.Process == nil {
		s.markStoppedLocked()
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()

	if method == core.StopInterrupt {
		err := interruptProcess(cmd)
		if err == nil && s.waitExit(ctx, timeout) {
			return nil
		}
		if !thenKill {
			if err != nil {
				return err
			}
			return errors.New("process did not exit after interrupt")
		}
	} else if !thenKill {
		return errors.New("no stop method is configured")
	}
	if err := terminateProcess(cmd, killFn); err != nil {
		return err
	}
	if !s.waitExit(ctx, timeout) {
		return errors.New("process did not exit")
	}
	return nil
}

// Hold keeps install, update, backup, and restore from running together.
// It does not wait. The returned function releases the hold.
func (s *Supervisor) Hold() (func(), error) {
	if !s.maint.TryLock() {
		return nil, ErrBusy
	}
	return s.maint.Unlock, nil
}

// RunStopped runs fn while the game process is stopped.
// When the server was running or starting, it is stopped first and started
// again afterward. A failure from fn still attempts that restart. The restart
// uses a fresh context so a cancelled backup can bring the process back.
// wasRunning is true when a restart was required.
func (s *Supervisor) RunStopped(ctx context.Context, fn func(context.Context) error) (wasRunning bool, err error) {
	if fn == nil {
		return false, errors.New("missing function")
	}
	release, err := s.Hold()
	if err != nil {
		return false, err
	}
	defer release()
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	wasRunning = s.state == StateRunning || s.state == StateStarting
	inst := s.inst
	mod := s.mod
	s.mu.Unlock()
	if wasRunning {
		if err := s.Stop(ctx); err != nil {
			return true, err
		}
	}
	err = fn(ctx)
	if !wasRunning {
		return false, err
	}
	if startErr := s.start(context.Background(), inst, mod, false); startErr != nil {
		if err != nil {
			return true, fmt.Errorf("%w; restart failed: %v", err, startErr)
		}
		return true, fmt.Errorf("server did not start again: %w", startErr)
	}
	return true, err
}

// Restart stops, then starts the attached instance.
func (s *Supervisor) Restart(ctx context.Context) error {
	if err := s.Stop(ctx); err != nil {
		return err
	}
	s.mu.Lock()
	inst := s.inst
	mod := s.mod
	s.mu.Unlock()
	return s.Start(ctx, inst, mod)
}

// Status samples the current process when one is running.
func (s *Supervisor) Status() Status {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Status{
		State:     s.state,
		Ready:     s.ready,
		PID:       s.pid,
		StartedAt: s.startedAt,
		ReadyAt:   s.readyAt,
		Restarts:  s.restarts,
		Message:   s.message,
	}
	if st.State == "" {
		st.State = StateStopped
	}
	if s.pid > 0 {
		mem, memOK, cpu, cpuOK, next := sampleResources(s.pid, s.resources)
		s.resources = next
		st.MemoryBytes = mem
		st.MemoryKnown = memOK
		st.CPUPercent = cpu
		st.CPUKnown = cpuOK
	}
	return st
}

func (s *Supervisor) beginStart(inst core.Instance, mod core.GameModule, fromUpdate bool) (chan struct{}, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mod != nil {
		s.mod = mod
	}
	if inst.Dir != "" {
		s.inst = inst
	}
	switch s.state {
	case StateStarting, StateRunning:
		return nil, fmt.Errorf("cannot start while %s", s.state)
	case StateUpdating:
		if !fromUpdate {
			return nil, errors.New("cannot start during an update")
		}
	}
	s.cancelRestartLocked()
	s.stopReq = false
	if !fromUpdate {
		s.crashes = nil
		s.restarts = 0
		s.message = ""
	}
	return s.spawnLocked()
}

func (s *Supervisor) spawnLocked() (chan struct{}, error) {
	if s.mod == nil {
		return nil, errors.New("no module")
	}
	if s.inst.Dir == "" {
		return nil, errors.New("instance directory is required")
	}
	exe := s.mod.ExePath(s.inst)
	resolved, err := resolveContained(s.inst.Dir, exe)
	if err != nil {
		if s.state != StateUpdating {
			s.state = StateStopped
		}
		return nil, err
	}
	cmd := exec.Command(resolved, s.mod.LaunchArgs(s.inst)...)
	cmd.Dir = s.inst.Dir
	cmd.WaitDelay = ioWaitDelay
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdoutR, stdoutW)
		return nil, err
	}
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	killFn, closeFn, err := startProcess(cmd)
	if err != nil {
		closeAll(stdoutR, stdoutW, stderrR, stderrW)
		if s.state != StateUpdating {
			s.state = StateCrashed
		}
		s.message = err.Error()
		return nil, err
	}
	if err := stdoutW.Close(); err != nil {
		s.logAbort(abortUnwatched(cmd, killFn, closeFn, stdoutR, stderrW, stderrR))
		return nil, err
	}
	if err := stderrW.Close(); err != nil {
		s.logAbort(abortUnwatched(cmd, killFn, closeFn, stdoutR, stderrR))
		return nil, err
	}
	if s.closeFn != nil {
		if closeErr := s.closeFn(); closeErr != nil && s.log != nil {
			s.log("close previous process: " + closeErr.Error())
		}
	}
	s.killFn = killFn
	s.closeFn = closeFn
	s.cancelBackoff = nil
	s.cmd = cmd
	s.pid = cmd.Process.Pid
	s.ready = false
	s.readyAt = time.Time{}
	s.startedAt = time.Now()
	s.state = StateStarting
	s.resources = resourcePoint{}
	s.gen++
	gen := s.gen
	release := make(chan struct{})
	go func() {
		<-release
		s.watch(stdoutR, stderrR, cmd, gen)
	}()
	return release, nil
}

func (s *Supervisor) watch(stdout, stderr io.ReadCloser, cmd *exec.Cmd, gen int) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		s.scan(stdout, gen)
	}()
	go func() {
		defer wg.Done()
		s.scan(stderr, gen)
	}()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	err := <-done
	// The process has exited. Read the remaining log before closing the
	// pipes, so the last lines are not discarded with the unread buffer.
	drainThenClose(&wg, stdout, stderr)
	s.onExit(gen, err)
}

// closeJobHandle drops the Windows job handle without TerminateJobObject.
// KILL_ON_JOB_CLOSE then ends the server and every process in the job.
// Unix has no job handle, so this is a no-op there.
func (s *Supervisor) closeJobHandle() error {
	s.mu.Lock()
	fn := s.closeFn
	s.closeFn = nil
	// killFn closes over the same job handle. Drop it so a later stop
	// does not call TerminateJobObject on a handle we already closed.
	s.killFn = nil
	s.mu.Unlock()
	if fn == nil {
		return nil
	}
	return fn()
}

func drainThenClose(wg *sync.WaitGroup, pipes ...io.Closer) {
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(ioWaitDelay):
	}
	closeAll(pipes...)
	<-drained
}

func closeAll(closers ...io.Closer) {
	for _, c := range closers {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			continue
		}
	}
}

func abortUnwatched(cmd *exec.Cmd, killFn, closeFn func() error, pipes ...io.Closer) error {
	var killErr, waitErr, closeErr error
	if killFn != nil {
		killErr = killFn()
	}
	if (killFn == nil || killErr != nil) && cmd != nil && cmd.Process != nil {
		if err := cmd.Process.Kill(); err != nil {
			killErr = err
		}
	}
	if cmd != nil && cmd.Process != nil {
		waitErr = cmd.Wait()
	}
	if closeFn != nil {
		closeErr = closeFn()
	}
	closeAll(pipes...)
	if killErr != nil || waitErr != nil || closeErr != nil {
		return fmt.Errorf("kill: %v; wait: %v; close: %v", killErr, waitErr, closeErr)
	}
	return nil
}

func (s *Supervisor) logAbort(err error) {
	if err != nil && s.log != nil {
		s.log("abort unwatched process: " + err.Error())
	}
}

func (s *Supervisor) scan(r io.Reader, gen int) {
	sc := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		s.observe(sc.Text(), gen)
	}
	if err := sc.Err(); err != nil && s.log != nil {
		s.log("log scan: " + err.Error())
	}
}

func (s *Supervisor) observe(line string, gen int) {
	if s.log != nil {
		s.log(line)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen || s.mod == nil || s.pid == 0 {
		return
	}
	if !core.LineReady(s.mod, line) {
		return
	}
	s.state = StateRunning
	s.ready = true
	if s.readyAt.IsZero() {
		s.readyAt = time.Now()
	}
	s.message = ""
}

func (s *Supervisor) onExit(gen int, waitErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if gen != s.gen {
		return
	}
	if s.closeFn != nil {
		if err := s.closeFn(); err != nil && s.log != nil {
			s.log("close process: " + err.Error())
		}
		s.closeFn = nil
		s.killFn = nil
	}
	s.cmd = nil
	s.pid = 0
	s.ready = false
	if s.stopReq || s.updating {
		s.state = StateStopped
		if waitErr != nil && !s.stopReq {
			s.message = waitErr.Error()
		}
		return
	}
	now := time.Now()
	window := s.policy.CrashWindow
	kept := make([]time.Time, 0, len(s.crashes)+1)
	for _, ts := range s.crashes {
		if now.Sub(ts) <= window {
			kept = append(kept, ts)
		}
	}
	kept = append(kept, now)
	s.crashes = kept
	if len(kept) >= s.policy.CrashLimit {
		s.state = StateCrashed
		s.message = fmt.Sprintf("halted after %d crashes in %s", len(kept), window)
		return
	}
	delay := s.backoffDelay(len(kept) - 1)
	s.restarts++
	s.state = StateStarting
	s.message = fmt.Sprintf("restarting after crash in %s", delay)
	ch := make(chan struct{})
	s.cancelBackoff = ch
	go s.restartAfter(delay, ch, gen)
}

func (s *Supervisor) restartAfter(delay time.Duration, cancel chan struct{}, gen int) {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-cancel:
		return
	}
	s.mu.Lock()
	if s.stopReq || s.updating || s.cancelBackoff != cancel || s.gen != gen {
		s.mu.Unlock()
		return
	}
	release, err := s.spawnLocked()
	genNow := s.gen
	timeout := s.policy.ReadyTimeout
	s.mu.Unlock()
	if release != nil {
		close(release)
	}
	if err != nil {
		s.mu.Lock()
		if s.pid == 0 && !s.stopReq {
			s.state = StateCrashed
			s.ready = false
			s.message = err.Error()
		}
		s.mu.Unlock()
		return
	}
	readyCtx, stopWait := context.WithTimeout(context.Background(), timeout)
	err = s.waitReady(readyCtx)
	stopWait()
	if err != nil {
		s.noteReadyTimeout(genNow)
	}
}

func (s *Supervisor) waitReady(ctx context.Context) error {
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		state := s.state
		ready := s.ready
		msg := s.message
		s.mu.Unlock()
		if ready && state == StateRunning {
			return nil
		}
		if state == StateCrashed || state == StateStopped {
			if msg == "" {
				msg = "server exited before ready"
			}
			return errors.New(msg)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for ready: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func terminateProcess(cmd *exec.Cmd, killFn func() error) error {
	if killFn != nil {
		return killFn()
	}
	return killProcess(cmd)
}

func resolveContained(root, exe string) (string, error) {
	info, err := os.Stat(exe)
	if err != nil || info.IsDir() {
		if err != nil {
			return "", fmt.Errorf("server executable: %w", err)
		}
		return "", fmt.Errorf("server executable: %s is a directory", exe)
	}
	rootResolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("server directory: %w", err)
	}
	exeResolved, err := filepath.EvalSymlinks(exe)
	if err != nil {
		return "", fmt.Errorf("server executable: %w", err)
	}
	rel, err := filepath.Rel(rootResolved, exeResolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("server executable %s is outside %s", exe, root)
	}
	return exeResolved, nil
}

func (s *Supervisor) noteReadyTimeout(gen int) {
	s.mu.Lock()
	if s.gen != gen || s.ready || s.stopReq {
		s.mu.Unlock()
		return
	}
	cmd := s.cmd
	killFn := s.killFn
	alive := s.pid != 0
	s.mu.Unlock()
	if !alive {
		return
	}
	if err := terminateProcess(cmd, killFn); err != nil && s.log != nil {
		s.log("kill after ready timeout: " + err.Error())
	}
}

func (s *Supervisor) abandon() {
	s.mu.Lock()
	s.stopReq = true
	s.cancelRestartLocked()
	alive := s.cmd != nil && s.pid != 0
	s.mu.Unlock()
	if !alive {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil && s.log != nil {
		s.log("stop after ready timeout: " + err.Error())
	}
}

func (s *Supervisor) waitExit(ctx context.Context, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		pid := s.pid
		s.mu.Unlock()
		if pid == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return s.exited()
		case <-ticker.C:
		}
	}
}

func (s *Supervisor) exited() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pid == 0
}

func (s *Supervisor) backoffDelay(index int) time.Duration {
	steps := s.policy.Backoff
	if len(steps) == 0 {
		return time.Second
	}
	if index < 0 {
		index = 0
	}
	if index >= len(steps) {
		index = len(steps) - 1
	}
	d := steps[index]
	if d > maxBackoff {
		return maxBackoff
	}
	if d < 0 {
		return 0
	}
	return d
}

func (s *Supervisor) cancelRestartLocked() {
	if s.cancelBackoff != nil {
		close(s.cancelBackoff)
		s.cancelBackoff = nil
	}
}

func (s *Supervisor) markStoppedLocked() {
	if s.state == StateCrashed {
		return
	}
	s.state = StateStopped
	s.ready = false
	s.pid = 0
}

func (s *Supervisor) stopChoiceLocked() (core.StopMethod, time.Duration, bool) {
	method := s.override
	timeout := s.policy.StopTimeout
	thenKill := true
	if s.mod != nil {
		spec := s.mod.StopStrategy()
		thenKill = spec.ThenKill
		if s.override == "" {
			method = spec.Method
			if spec.Timeout > 0 {
				timeout = spec.Timeout
			}
		}
	}
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return method, timeout, thenKill
}
