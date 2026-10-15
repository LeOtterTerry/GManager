package supervisor

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

func TestRunStoppedBacksUpWhileStoppedAndRestarts(t *testing.T) {
	inst := newFake(t, `{"readyDelay":"20ms"}`)
	original := readConfig(t, inst)
	sup := New(testPolicy(), nil)
	t.Cleanup(func() { stopNow(t, sup) })
	sup.Attach(inst, plainsofpain.New())
	if err := sup.Start(context.Background(), inst, plainsofpain.New()); err != nil {
		t.Fatal(err)
	}
	var sawStopped bool
	restarted, err := sup.RunStopped(context.Background(), func(context.Context) error {
		st := sup.Status()
		sawStopped = st.PID == 0 && st.State == StateStopped
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restarted || !sawStopped {
		t.Fatalf("restarted=%v sawStopped=%v", restarted, sawStopped)
	}
	st := sup.Status()
	if st.State != StateRunning || !st.Ready || st.PID == 0 {
		t.Fatalf("after backup = %+v", st)
	}
	if !bytes.Equal(readConfig(t, inst), original) {
		t.Fatal("config changed")
	}

	_, err = sup.RunStopped(context.Background(), func(context.Context) error {
		return errors.New("disk full")
	})
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("disk full")) {
		t.Fatalf("err = %v", err)
	}
	st = sup.Status()
	if st.State != StateRunning || !st.Ready {
		t.Fatalf("after failed backup = %+v", st)
	}

	if err := sup.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	restarted, err = sup.RunStopped(context.Background(), func(context.Context) error { return nil })
	if err != nil || restarted {
		t.Fatalf("stopped restart=%v err=%v", restarted, err)
	}
	if sup.Status().State != StateStopped || sup.Status().PID != 0 {
		t.Fatalf("stayed down = %+v", sup.Status())
	}
}

func TestOperationsShareAHold(t *testing.T) {
	sup := New(testPolicy(), nil)
	release, err := sup.Hold()
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err := sup.RunStopped(context.Background(), func(context.Context) error { return nil }); !errors.Is(err, ErrBusy) {
		t.Fatalf("backup err = %v", err)
	}
	err = sup.Update(context.Background(), &scriptedSteam{}, func(context.Context, core.Instance) error { return nil })
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("update err = %v", err)
	}
	if err := sup.Install(context.Background(), &scriptedSteam{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("install err = %v", err)
	}
}
