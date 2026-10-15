// Package fakepop builds the fakeserver test double.
package fakepop

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

var (
	buildOnce sync.Once
	built     string
	buildErr  error
)

// Binary compiles cmd/fakeserver once and returns the path.
// Tests copy it into a server directory. CI does not download Steam.
func Binary(t testing.TB) string {
	t.Helper()
	buildOnce.Do(func() {
		root, err := moduleRoot()
		if err != nil {
			buildErr = err
			return
		}
		dir, err := os.MkdirTemp("", "gmanager-fakeserver")
		if err != nil {
			buildErr = err
			return
		}
		name := "fakeserver"
		if runtime.GOOS == "windows" {
			name = "fakeserver.exe"
		}
		built = filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-o", built, "./cmd/fakeserver")
		cmd.Dir = root
		out, err := cmd.CombinedOutput()
		if err != nil {
			buildErr = fmt.Errorf("build fakeserver: %w: %s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return built
}

func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found from %s", dir)
		}
		dir = parent
	}
}
