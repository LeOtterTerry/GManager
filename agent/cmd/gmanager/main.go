// Command gmanager is the local agent.
//
// This slice runs it in the foreground. Installing it as a Windows service
// belongs to the installer. The process binds 127.0.0.1 only and requires
// GMANAGER_TOKEN. It does not print the token.
package main

import (
	"flag"
	"log"
	"net/http"
	"os"
	"path/filepath"

	"github.com/LeOtterTerry/GManager/agent/internal/api"
	"github.com/LeOtterTerry/GManager/agent/internal/config"
	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

var stderr = log.New(os.Stderr, "", 0)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (127.0.0.1 only)")
	instance := flag.String("instance", "default", "instance id")
	dataDir := flag.String("data-dir", "data", "agent data directory")
	serverDir := flag.String("server-dir", "", "game server directory (default <data-dir>/servers/<instance>)")
	flag.Parse()

	token := os.Getenv("GMANAGER_TOKEN")
	if token == "" {
		stderr.Fatal("GMANAGER_TOKEN is required")
	}
	if err := api.ValidateAddr(*addr); err != nil {
		stderr.Fatal(err)
	}

	dir := *serverDir
	if dir == "" {
		dir = filepath.Join(*dataDir, "servers", *instance)
	}
	mod := plainsofpain.New()
	editor, err := config.NewEditor(
		filepath.Join(dir, plainsofpain.ConfigRelPath),
		filepath.Join(*dataDir, "state", *instance, "server.json.meta"),
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
	})
	if err != nil {
		stderr.Fatal(err)
	}
	ln, err := api.Listen(*addr)
	if err != nil {
		stderr.Fatal(err)
	}
	log.Printf("gmanager listening on http://%s (instance %s)", *addr, *instance)
	if err := http.Serve(ln, srv.Handler()); err != nil {
		stderr.Fatal(err)
	}
}
