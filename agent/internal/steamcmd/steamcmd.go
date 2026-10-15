// Package steamcmd runs anonymous SteamCMD app updates.
// Tests inject a downloader or a pre-seeded binary. The unit tests never
// contact Steam.
package steamcmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
)

const (
	// LinuxArchiveURL is the public SteamCMD Linux tarball.
	LinuxArchiveURL = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz"
	// WindowsArchiveURL is the public SteamCMD Windows zip.
	WindowsArchiveURL = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"
)

// Download fetches a URL body. Tests replace it. The default uses HTTP.
type Download func(ctx context.Context, url string) (io.ReadCloser, error)

// Client bootstraps SteamCMD and runs an anonymous app_update.
type Client struct {
	Dir      string
	Download Download
}

// New returns a client that stores SteamCMD in dir.
func New(dir string) *Client {
	return &Client{Dir: dir, Download: httpDownload}
}

// Args is the SteamCMD argument list for an anonymous validated install.
// force_install_dir is included so the server lands in the instance directory.
func Args(appID int, installDir string, validate bool) []string {
	args := []string{
		"+force_install_dir", installDir,
		"+login", "anonymous",
		"+app_update", strconv.Itoa(appID),
	}
	if validate {
		args = append(args, "validate")
	}
	args = append(args, "+quit")
	return args
}

// Executable is the SteamCMD entry point inside Dir.
func (c *Client) Executable() string {
	name := "steamcmd.sh"
	if runtime.GOOS == "windows" {
		name = "steamcmd.exe"
	}
	return filepath.Join(c.Dir, name)
}

func (c *Client) archiveURL() string {
	if runtime.GOOS == "windows" {
		return WindowsArchiveURL
	}
	return LinuxArchiveURL
}

// Ensure downloads and extracts SteamCMD when the binary is absent.
func (c *Client) Ensure(ctx context.Context) (exe string, err error) {
	if c.Dir == "" {
		return "", errors.New("steamcmd directory is required")
	}
	exe = c.Executable()
	if info, statErr := os.Stat(exe); statErr == nil && !info.IsDir() {
		return exe, nil
	}
	if err = os.MkdirAll(c.Dir, 0o755); err != nil {
		return "", err
	}
	download := c.Download
	if download == nil {
		download = httpDownload
	}
	body, err := download(ctx, c.archiveURL())
	if err != nil {
		return "", err
	}
	defer func() {
		closeErr := body.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	if runtime.GOOS == "windows" {
		err = ExtractZip(body, c.Dir)
	} else {
		err = ExtractTarGz(body, c.Dir)
	}
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(exe); statErr != nil {
		return "", fmt.Errorf("steamcmd archive did not contain %s", filepath.Base(exe))
	}
	return exe, nil
}

// AppUpdate runs `login anonymous` and `app_update <id> validate` when validate is set.
// A non-anonymous spec is refused. This client has no Steam credentials.
func (c *Client) AppUpdate(ctx context.Context, appID int, installDir string, validate, anonymous bool, log func(string)) error {
	if !anonymous {
		return errors.New("refusing a non-anonymous Steam login; no credentials are configured")
	}
	if appID <= 0 {
		return errors.New("steam app id is required")
	}
	if installDir == "" {
		return errors.New("install directory is required")
	}
	exe, err := c.Ensure(ctx)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, exe, Args(appID, installDir, validate)...)
	cmd.Dir = c.Dir
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	var scanMu sync.Mutex
	var scanErr error
	scan := func(r io.Reader) {
		defer wg.Done()
		sc := bufio.NewScanner(r)
		buf := make([]byte, 0, 64*1024)
		sc.Buffer(buf, 1024*1024)
		for sc.Scan() {
			if log != nil {
				log(sc.Text())
			}
		}
		if err := sc.Err(); err != nil {
			scanMu.Lock()
			if scanErr == nil {
				scanErr = err
			}
			scanMu.Unlock()
		}
	}
	wg.Add(2)
	go scan(stdout)
	go scan(stderr)
	wg.Wait()
	waitErr := cmd.Wait()
	if scanErr != nil {
		return scanErr
	}
	return waitErr
}

func httpDownload(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, fmt.Errorf("steamcmd download %s: %s", url, res.Status)
	}
	return res.Body, nil
}
