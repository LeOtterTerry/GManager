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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// LinuxArchiveURL is the public SteamCMD Linux tarball.
	LinuxArchiveURL = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd_linux.tar.gz"
	// WindowsArchiveURL is the public SteamCMD Windows zip.
	WindowsArchiveURL = "https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"
	// MaxDownloadBytes is the largest installer body that will be written.
	MaxDownloadBytes int64 = 64 << 20
	// DefaultRunTimeout bounds a SteamCMD install when Client.Timeout is unset.
	DefaultRunTimeout = 60 * time.Minute
	// ioWaitDelay bounds how long Wait blocks on pipes after the process exits.
	ioWaitDelay = 5 * time.Second

	readyMarkerName = ".gmanager-steamcmd-ready"
)

var errDownloadTooLarge = errors.New("steamcmd download exceeds the size cap")

// Download fetches a URL body. Tests replace it. The default uses HTTP.
type Download func(ctx context.Context, url string) (io.ReadCloser, error)

// Client bootstraps SteamCMD and runs an anonymous app_update.
type Client struct {
	Dir      string
	Download Download
	// Verify checks the extracted executable. Nil uses the platform default,
	// which is WinVerifyTrust on Windows. Tests that plant a fake binary set
	// this to a no-op.
	Verify func(path string) error
	// Timeout bounds one SteamCMD process. Zero uses DefaultRunTimeout.
	Timeout time.Duration
}

// New returns a client that stores SteamCMD in dir.
func New(dir string) *Client {
	return &Client{Dir: dir, Download: httpDownload, Verify: defaultVerify}
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

// Ensure downloads and extracts SteamCMD when the ready marker is absent.
// The archive is extracted into a sibling temp directory, that directory is
// renamed into place, and the marker is written last so a crash cannot leave
// a partial tree looking complete.
func (c *Client) Ensure(ctx context.Context) (exe string, err error) {
	if c.Dir == "" {
		return "", errors.New("steamcmd directory is required")
	}
	exe = c.Executable()
	marker := filepath.Join(c.Dir, readyMarkerName)
	if regularFile(exe) && regularFile(marker) {
		return exe, nil
	}
	parent := filepath.Dir(c.Dir)
	if parent == "" {
		parent = "."
	}
	if err = os.MkdirAll(parent, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(parent, ".steamcmd-staging-")
	if err != nil {
		return "", err
	}
	renamed := false
	defer func() {
		if renamed {
			return
		}
		if removeErr := os.RemoveAll(staging); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
			err = removeErr
		}
	}()
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
	limited, err := copyLimited(body, MaxDownloadBytes)
	if err != nil {
		return "", err
	}
	defer func() {
		closeErr := limited.Close()
		if err == nil && closeErr != nil {
			err = closeErr
		}
	}()
	if runtime.GOOS == "windows" {
		err = ExtractZip(limited, staging)
	} else {
		err = ExtractTarGz(limited, staging)
	}
	if err != nil {
		return "", err
	}
	stagedExe := filepath.Join(staging, filepath.Base(exe))
	if !regularFile(stagedExe) {
		return "", fmt.Errorf("steamcmd archive did not contain %s", filepath.Base(exe))
	}
	verify := c.Verify
	if verify == nil {
		verify = defaultVerify
	}
	if err = verify(stagedExe); err != nil {
		return "", fmt.Errorf("steamcmd signature: %w", err)
	}
	if _, statErr := os.Stat(c.Dir); statErr == nil {
		if err = os.RemoveAll(c.Dir); err != nil {
			return "", err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", statErr
	}
	if err = os.Rename(staging, c.Dir); err != nil {
		return "", err
	}
	renamed = true
	if err = os.WriteFile(marker, []byte("ok\n"), 0o644); err != nil {
		return "", err
	}
	return c.Executable(), nil
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
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.runLimit())
	defer cancel()
	exe, err := c.Ensure(ctx)
	if err != nil {
		return err
	}
	return runLogged(ctx, exe, Args(appID, installDir, validate), c.Dir, log)
}

func (c *Client) runLimit() time.Duration {
	if c.Timeout > 0 {
		return c.Timeout
	}
	return DefaultRunTimeout
}

func runLogged(ctx context.Context, exe string, args []string, dir string, log func(string)) error {
	stdoutR, stdoutW, err := os.Pipe()
	if err != nil {
		return err
	}
	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		closeAll(stdoutR, stdoutW)
		return err
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	cmd.WaitDelay = ioWaitDelay
	cmd.Stdout = stdoutW
	cmd.Stderr = stderrW
	if err := cmd.Start(); err != nil {
		closeAll(stdoutR, stdoutW, stderrR, stderrW)
		return err
	}
	if err := stdoutW.Close(); err != nil {
		return stopLogged(cmd, err, stdoutR, stderrW, stderrR)
	}
	if err := stderrW.Close(); err != nil {
		return stopLogged(cmd, err, stdoutR, stderrR)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		scanOutput(stdoutR, log)
	}()
	go func() {
		defer wg.Done()
		scanOutput(stderrR, log)
	}()
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	waitErr := <-done
	drainThenClose(&wg, stdoutR, stderrR)
	return waitErr
}

func stopLogged(cmd *exec.Cmd, cause error, pipes ...io.Closer) error {
	var killErr, waitErr error
	if cmd.Process != nil {
		killErr = cmd.Process.Kill()
		waitErr = cmd.Wait()
	}
	closeAll(pipes...)
	if killErr != nil || waitErr != nil {
		return fmt.Errorf("%w (kill: %v, wait: %v)", cause, killErr, waitErr)
	}
	return cause
}

func drainThenClose(wg *sync.WaitGroup, pipes ...io.Closer) {
	drained := make(chan struct{})
	go func() {
		wg.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(ioWaitDelay):
	}
	closeAll(pipes...)
	<-drained
}

func closeAll(closers ...io.Closer) {
	for _, c := range closers {
		if c == nil {
			continue
		}
		if err := c.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
			continue
		}
	}
}

func scanOutput(r io.Reader, log func(string)) {
	sc := bufio.NewScanner(r)
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)
	for sc.Scan() {
		if log != nil {
			log(sc.Text())
		}
	}
}

func httpDownload(ctx context.Context, rawURL string) (io.ReadCloser, error) {
	if err := validateArchiveURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("steamcmd download stopped after 10 redirects")
		}
		return validateRedirect(req.URL)
	}}
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode != http.StatusOK {
		closeErr := res.Body.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("steamcmd download %s: %s (%v)", rawURL, res.Status, closeErr)
		}
		return nil, fmt.Errorf("steamcmd download %s: %s", rawURL, res.Status)
	}
	return res.Body, nil
}

func validateArchiveURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return errors.New("steamcmd download must use https")
	}
	return nil
}

func validateRedirect(u *url.URL) error {
	if u == nil || u.Scheme != "https" {
		return errors.New("steamcmd redirect must use https")
	}
	if !valveHost(u.Hostname()) {
		return fmt.Errorf("steamcmd redirect host %s is not allowed", u.Hostname())
	}
	return nil
}

func valveHost(host string) bool {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "steamcdn-a.akamaihd.net" {
		return true
	}
	for _, suffix := range []string{"steampowered.com", "steamstatic.com", "steamcontent.com", "valvesoftware.com"} {
		if host == suffix || strings.HasSuffix(host, "."+suffix) {
			return true
		}
	}
	return false
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func copyLimited(r io.Reader, max int64) (io.ReadCloser, error) {
	f, err := os.CreateTemp("", "steamcmd-body-")
	if err != nil {
		return nil, err
	}
	name := f.Name()
	if _, err := io.Copy(f, &tooBigReader{r: r, left: max}); err != nil {
		return nil, discardTemp(f, name, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, discardTemp(f, name, err)
	}
	return &tempBody{File: f}, nil
}

func discardTemp(f *os.File, name string, cause error) error {
	closeErr := f.Close()
	removeErr := os.Remove(name)
	if removeErr != nil && errors.Is(removeErr, os.ErrNotExist) {
		removeErr = nil
	}
	if cause == nil {
		if closeErr != nil {
			return closeErr
		}
		return removeErr
	}
	if closeErr != nil || removeErr != nil {
		return fmt.Errorf("%w (close: %v, remove: %v)", cause, closeErr, removeErr)
	}
	return cause
}

type tempBody struct{ *os.File }

func (t *tempBody) Close() error {
	name := t.Name()
	err := t.File.Close()
	if removeErr := os.Remove(name); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
		err = removeErr
	}
	return err
}

type tooBigReader struct {
	r    io.Reader
	left int64
}

func (t *tooBigReader) Read(p []byte) (int, error) {
	if t.left <= 0 {
		return 0, errDownloadTooLarge
	}
	if int64(len(p)) > t.left {
		p = p[:t.left]
	}
	n, err := t.r.Read(p)
	t.left -= int64(n)
	if t.left == 0 && err == nil {
		var b [1]byte
		extra, peekErr := t.r.Read(b[:])
		if extra > 0 {
			return n, errDownloadTooLarge
		}
		if peekErr != nil {
			return n, peekErr
		}
	}
	return n, err
}
