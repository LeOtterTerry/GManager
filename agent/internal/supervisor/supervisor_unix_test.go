//go:build unix

package supervisor

import (
	"context"
	"testing"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

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
