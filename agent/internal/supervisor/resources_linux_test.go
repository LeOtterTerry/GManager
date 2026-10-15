//go:build linux

package supervisor

import (
	"os"
	"testing"
	"time"
)

func TestSampleSelf(t *testing.T) {
	mem, ok, _, _, next := sampleResources(os.Getpid(), resourcePoint{})
	if !ok || mem == 0 || !next.ok {
		t.Fatalf("mem=%d ok=%v next=%+v", mem, ok, next)
	}
	time.Sleep(20 * time.Millisecond)
	_, _, _, cpuOK, again := sampleResources(os.Getpid(), next)
	if !again.ok {
		t.Fatal("second sample failed")
	}
	if !cpuOK && again.total == next.total {
		return
	}
	if !cpuOK {
		t.Fatalf("cpu unknown with ticks %d -> %d", next.total, again.total)
	}
}
