//go:build windows

package supervisor

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func TestWindowsFakeServerLifecycle(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"30ms"}`)
	sup := New(testPolicy(), nil)
	t.Cleanup(func() { stopNow(t, sup) })
	if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	st := sup.Status()
	if st.State != StateRunning || !st.Ready || st.PID == 0 {
		t.Fatalf("status = %+v", st)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := sup.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		st = sup.Status()
		if st.State == StateStopped && st.PID == 0 && !st.Ready {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("after stop = %+v", sup.Status())
}

func TestWindowsJobKillsGrandchild(t *testing.T) {
	t.Run("stop", func(t *testing.T) {
		inst := newFake(t, `{"readyDelay":"30ms","grandchild":true}`)
		sup := New(testPolicy(), nil)
		t.Cleanup(func() { stopNow(t, sup) })
		if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
			t.Fatal(err)
		}
		pid := grandchildPID(t, inst.Dir)
		if !processAlive(pid) {
			t.Fatalf("grandchild %d was not running", pid)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := sup.Stop(ctx); err != nil {
			t.Fatal(err)
		}
		waitDead(t, pid)
	})

	t.Run("job close", func(t *testing.T) {
		inst := newFake(t, `{"readyDelay":"30ms","grandchild":true}`)
		sup := New(testPolicy(), nil)
		t.Cleanup(func() { stopNow(t, sup) })
		if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
			t.Fatal(err)
		}
		pid := grandchildPID(t, inst.Dir)
		if !processAlive(pid) {
			t.Fatalf("grandchild %d was not running", pid)
		}
		if err := sup.closeJobHandle(); err != nil {
			t.Fatal(err)
		}
		waitDead(t, pid)
		waitStopped(t, sup)
	})
}

func grandchildPID(t *testing.T, dir string) int {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(dir, "grandchild.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(body)))
	if err != nil || pid <= 0 {
		t.Fatalf("grandchild pid %q: %v", body, err)
	}
	return pid
}

func waitDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("process %d is still running", pid)
}

func waitStopped(t *testing.T, sup *Supervisor) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sup.Status().PID == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server still running: %+v", sup.Status())
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer func() {
		if closeErr := windows.CloseHandle(h); closeErr != nil {
			return
		}
	}()
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return false
	}
	const stillActive = 259
	return code == stillActive
}
