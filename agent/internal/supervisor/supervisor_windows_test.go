//go:build windows

package supervisor

import (
	"context"
	"testing"
	"time"

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
