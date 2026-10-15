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
	"github.com/LeOtterTerry/GManager/agent/internal/backup"
	"github.com/LeOtterTerry/GManager/agent/internal/config"
	"github.com/LeOtterTerry/GManager/agent/internal/core"
	"github.com/LeOtterTerry/GManager/agent/internal/steamcmd"
	"github.com/LeOtterTerry/GManager/agent/internal/supervisor"
	"github.com/LeOtterTerry/GManager/agent/modules/plainsofpain"
)

var stderr = log.New(os.Stderr, "", 0)

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "listen address (127.0.0.1 only)")
	instance := flag.String("instance", "default", "instance id")
	dataDir := flag.String("data-dir", "", "agent data directory (default %ProgramData%\\GManager on Windows, data otherwise)")
	serverDir := flag.String("server-dir", "", "game server directory (default <data-dir>/servers/<instance>)")
	uiPort := flag.Int("ui-port", api.DefaultUIPort, "dashboard origin port")
	stopMethod := flag.String("stop-method", "", "stop override: empty uses the module; interrupt is SIGTERM on Unix and is not a confirmed Plains of Pain clean stop")
	console := flag.Bool("console-commands", false, "pass -cc (UNVERIFIED on Windows; default false)")
	steamTimeout := flag.Duration("steamcmd-timeout", steamcmd.DefaultRunTimeout, "maximum time for one SteamCMD install or update")
	backupDir := flag.String("backup-dir", "", "backup directory (default <data-dir>/backups). Must be absolute, not a UNC path, and not inside the server directory")
	backupKeep := flag.Int("backup-keep", backup.DefaultKeep, "how many backups to keep; the oldest are deleted past this count")
	userDir := flag.String("user-dir", "", "save directory override; empty reads userdir from server.json. The game's default save path is unverified")
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
	if *steamTimeout <= 0 {
		stderr.Fatal("steamcmd-timeout must be positive")
	}
	if *backupKeep < 1 {
		stderr.Fatal("backup-keep must be at least 1")
	}
	switch core.StopMethod(*stopMethod) {
	case core.StopUnknown, core.StopInterrupt:
	default:
		stderr.Fatal("stop-method must be empty or interrupt")
	}

	dir := *serverDir
	if dir == "" {
		dir = filepath.Join(dirData, "servers", *instance)
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		stderr.Fatal(err)
	}
	dir = absDir
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
	logs := api.NewLogStore()
	sup := supervisor.New(supervisor.DefaultPolicy(), func(line string) {
		logs.Append(line)
	})
	sup.SetStopMethod(core.StopMethod(*stopMethod))
	steam := steamcmd.New(filepath.Join(dirData, "steamcmd"))
	steam.Timeout = *steamTimeout
	dirBackup := *backupDir
	if dirBackup == "" {
		dirBackup = backup.DefaultDir(dirData)
	}
	dirBackup, err = filepath.Abs(dirBackup)
	if err != nil {
		stderr.Fatal(err)
	}
	if err := backup.ValidateDir(dirBackup, dir); err != nil {
		stderr.Fatal(err)
	}
	inst := core.Instance{
		ID:                    *instance,
		Dir:                   dir,
		EnableConsoleCommands: *console,
	}
	if *userDir != "" {
		inst.UserDir = *userDir
	} else if raw, readErr := os.ReadFile(editor.ConfigPath()); readErr == nil {
		if save, ok := mod.ConfigSaveDir(raw); ok {
			inst.UserDir = save
		}
	}
	archives := &backup.Store{
		Dir:        dirBackup,
		Keep:       *backupKeep,
		KeyPath:    filepath.Join(dirData, "backup.hmac"),
		JournalDir: dirData,
	}
	if err := archives.Recover(inst, mod); err != nil {
		log.Printf("warning: restore journal: %v; restore is blocked until it is fixed", err)
	}
	holds, err := archives.StrayHolds(inst, mod)
	if err != nil {
		log.Printf("warning: restore journal: %v; restore is blocked until it is fixed", err)
		holds = nil
	}
	for _, hold := range holds {
		log.Printf("warning: unjournaled hold %s; restore is blocked until it is discarded", hold)
	}
	staging, serr := archives.StrayStaging(inst, mod)
	if serr != nil && err == nil {
		log.Printf("warning: restore journal: %v; restore is blocked until it is fixed", serr)
		staging = nil
	}
	for _, path := range staging {
		log.Printf("warning: unjournaled restore staging %s; discard-hold stays blocked until it is removed", path)
	}
	srv, err := api.New(api.Options{
		Token:      token,
		InstanceID: *instance,
		Instance:   inst,
		Module:     mod,
		Editor:     editor,
		UIPort:     *uiPort,
		Logs:       logs,
		Supervisor: sup,
		Steam:      steam,
		Archives:   archives,
		UserDir:    *userDir,
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
