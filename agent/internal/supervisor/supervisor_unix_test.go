//go:build unix

package supervisor

import (
	"context"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/internal/fakepop"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func TestGrandchildModeStaysAlive(t *testing.T) {
	cmd := exec.Command(fakepop.Binary(t), "--grandchild")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process == nil {
			return
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Errorf("kill grandchild: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			return
		}
	})
	time.Sleep(200 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("grandchild exited: %v", err)
	}
}

func TestStopInterruptExitsCleanly(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"20ms"}`)
	sup := New(testPolicy(), nil)
	sup.SetStopMethod(core.StopInterrupt)
	t.Cleanup(func() { stopNow(t, sup) })
	if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	if err := sup.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 800*time.Millisecond {
		t.Fatalf("interrupt stop took %s", time.Since(started))
	}
	time.Sleep(200 * time.Millisecond)
	st := sup.Status()
	if st.State != StateStopped || st.Restarts != 0 || st.PID != 0 {
		t.Fatalf("status = %+v", st)
	}
}
