// Command gmanager is the local agent.
//
// This slice runs it in the foreground. Installing it as a Windows service
// belongs to the installer. The process binds 127.0.0.1 only. The bearer
// token is GMANAGER_TOKEN when that value is at least 32 characters, or an
// owner-only file under the data directory. The process does not print the token.
package main

import (
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/api"
	"github.com/LeOtterTerry/GManager/agent/internal/config"
	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

var stderr = log.New(os.Stderr, "", 0)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (127.0.0.1 only)")
	instance := flag.String("instance", "default", "instance id")
	dataDir := flag.String("data-dir", "", "agent data directory (default %ProgramData%\\GManager on Windows, data otherwise)")
	serverDir := flag.String("server-dir", "", "game server directory (default <data-dir>/servers/<instance>)")
	uiPort := flag.Int("ui-port", api.DefaultUIPort, "dashboard origin port")
	flag.Parse()

	dirData := *dataDir
	if dirData == "" {
		dirData = defaultDataDir()
	}
	if err := api.ValidateInstanceID(*instance); err != nil {
		stderr.Fatal(err)
	}
	if *uiPort < 1 || *uiPort > 65535 {
		stderr.Fatal("ui-port must be from 1 to 65535")
	}
	token, err := ResolveToken(dirData)
	if err != nil {
		stderr.Fatal(err)
	}
	if err := api.ValidateAddr(*addr); err != nil {
		stderr.Fatal(err)
	}
	apiPort, err := portFromAddr(*addr)
	if err != nil {
		stderr.Fatal(err)
	}

	dir := *serverDir
	if dir == "" {
		dir = filepath.Join(dirData, "servers", *instance)
	}
	mod := plainsofpain.New()
	mod.ManagerAPIPort = apiPort
	mod.ManagerUIPort = *uiPort
	editor, err := config.NewEditor(
		filepath.Join(dir, plainsofpain.ConfigRelPath),
		filepath.Join(dirData, "state", *instance, "server.json.meta"),
	)
	if err != nil {
		stderr.Fatal(err)
	}
	srv, err := api.New(api.Options{
		Token:      token,
		InstanceID: *instance,
		Instance:   core.Instance{ID: *instance, Dir: dir},
		Module:     mod,
		Editor:     editor,
		UIPort:     *uiPort,
	})
	if err != nil {
		stderr.Fatal(err)
	}
	ln, err := api.Listen(*addr)
	if err != nil {
		stderr.Fatal(err)
	}
	log.Printf("gmanager listening on http://%s (instance %s)", *addr, *instance)
	httpSrv := &http.Server{
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	if err := httpSrv.Serve(ln); err != nil {
		stderr.Fatal(err)
	}
}

func portFromAddr(addr string) (int, error) {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(port)
}
