package disk

import (
	"testing"
)

func TestFreeReportsSpace(t *testing.T) {
	free, err := Free(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if free == 0 {
		t.Fatal("free space = 0")
	}
}
