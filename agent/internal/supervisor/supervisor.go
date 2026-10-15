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
}

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
	if err := killProcess(cmd); err != nil {
		return err
	}
	if !s.waitExit(ctx, timeout) {
		return errors.New("process did not exit")
	}
	return nil
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
	info, err := os.Stat(exe)
	if err != nil || info.IsDir() {
		if s.state != StateUpdating {
			s.state = StateStopped
		}
		if err != nil {
			return nil, fmt.Errorf("server executable: %w", err)
		}
		return nil, fmt.Errorf("server executable: %s is a directory", exe)
	}
	cmd := exec.Command(exe, s.mod.LaunchArgs(s.inst)...)
	cmd.Dir = s.inst.Dir
	prepareCommand(cmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		if s.state != StateUpdating {
			s.state = StateCrashed
		}
		s.message = err.Error()
		return nil, err
	}
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
		s.watch(stdout, stderr, cmd, gen)
	}()
	return release, nil
}

func (s *Supervisor) watch(stdout, stderr io.Reader, cmd *exec.Cmd, gen int) {
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
	wg.Wait()
	waitErr := cmd.Wait()
	s.onExit(gen, waitErr)
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
	if gen != s.gen || s.mod == nil {
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
