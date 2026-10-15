package steamcmd

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/LeOtterTerry/GManager/agent/internal/fakepop"
)

func TestArgsAnonymousValidate(t *testing.T) {
	got := Args(2227360, "/srv/pop", true)
	want := []string{
		"+force_install_dir", "/srv/pop",
		"+login", "anonymous",
		"+app_update", "2227360",
		"validate",
		"+quit",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("args = %#v", got)
	}
	plain := Args(2227360, "/srv/pop", false)
	if strings.Contains(strings.Join(plain, " "), "validate") {
		t.Fatalf("validate was set: %#v", plain)
	}
}

func TestAppUpdateRefusesCredentials(t *testing.T) {
	c := New(t.TempDir())
	c.Download = func(context.Context, string) (io.ReadCloser, error) {
		t.Fatal("download was called")
		return nil, errors.New("network")
	}
	err := c.AppUpdate(context.Background(), 2227360, t.TempDir(), true, false, nil)
	if err == nil || !strings.Contains(err.Error(), "non-anonymous") {
		t.Fatalf("err = %v", err)
	}
}

func TestEnsureExtractsWithoutNetwork(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "steamcmd")
	var calls int
	c := New(dir)
	c.Verify = func(string) error { return nil }
	c.Download = func(_ context.Context, url string) (io.ReadCloser, error) {
		calls++
		if url != c.archiveURL() {
			t.Errorf("url = %s", url)
		}
		return io.NopCloser(bytes.NewReader(testArchive(t))), nil
	}
	exe, err := c.Ensure(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(exe) != filepath.Base(c.Executable()) {
		t.Fatalf("exe = %s", exe)
	}
	marker, err := os.ReadFile(filepath.Join(dir, readyMarkerName))
	if err != nil || string(marker) == "" {
		t.Fatalf("marker = %q %v", marker, err)
	}
	staging, err := filepath.Glob(filepath.Join(root, ".steamcmd-staging-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(staging) != 0 {
		t.Fatalf("staging left behind: %v", staging)
	}
	if _, err := c.Ensure(context.Background()); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("downloads = %d", calls)
	}
}

func TestAppUpdateUsesLocalBinary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("steamcmd.sh double is a Unix script")
	}
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "steamcmd.sh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, readyMarkerName), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(dir)
	c.Verify = func(string) error { return nil }
	c.Download = func(context.Context, string) (io.ReadCloser, error) {
		t.Fatal("download was called")
		return nil, errors.New("network")
	}
	var lines []string
	install := filepath.Join(t.TempDir(), "server")
	err := c.AppUpdate(context.Background(), 2227360, install, true, true, func(line string) {
		lines = append(lines, line)
	})
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Join(lines, "\n")
	for _, want := range []string{"+force_install_dir", install, "+login", "anonymous", "+app_update", "2227360", "validate", "+quit"} {
		if !strings.Contains(got, want) {
			t.Fatalf("log missing %q\n%s", want, got)
		}
	}
}

func TestExtractRejectsZipSlip(t *testing.T) {
	dest := t.TempDir()
	if err := ExtractTarGz(bytes.NewReader(tarGzWith(t, "../evil.txt", "nope")), dest); err == nil {
		t.Fatal("tar slip was accepted")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "evil.txt")); err == nil {
		t.Fatal("tar slip wrote outside dest")
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create("../evil.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("nope")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ExtractZip(bytes.NewReader(buf.Bytes()), dest); err == nil {
		t.Fatal("zip slip was accepted")
	}
}

func testArchive(t *testing.T) []byte {
	t.Helper()
	name := "steamcmd.sh"
	body := []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n")
	mode := int64(0o755)
	if runtime.GOOS == "windows" {
		name = "steamcmd.exe"
		body = []byte("MZ")
		mode = 0o644
	}
	if runtime.GOOS == "windows" {
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		h := &zip.FileHeader{Name: name, Method: zip.Store}
		h.SetMode(os.FileMode(mode))
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(body); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	return tarGzWith(t, name, string(body))
}

func tarGzWith(t *testing.T, name, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestSafeJoinRejectsUnsafeNames(t *testing.T) {
	dest := t.TempDir()
	for _, name := range []string{
		`\\server\share\steamcmd.exe`,
		`//server/share/steamcmd.exe`,
		`C:\steamcmd.exe`,
		`C:steamcmd.exe`,
		`steamcmd.exe:Zone.Identifier`,
		`../evil.txt`,
		"",
	} {
		if _, err := safeJoin(dest, name); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	got, err := safeJoin(dest, "steamcmd.exe")
	if err != nil || got != filepath.Join(dest, "steamcmd.exe") {
		t.Fatalf("got %s %v", got, err)
	}
}

func TestCopyLimitedCapsSize(t *testing.T) {
	_, err := copyLimited(bytes.NewReader(bytes.Repeat([]byte("a"), 9)), 8)
	if !errors.Is(err, errDownloadTooLarge) {
		t.Fatalf("err = %v", err)
	}
	rc, err := copyLimited(bytes.NewReader(bytes.Repeat([]byte("a"), 8)), 8)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(rc)
	closeErr := rc.Close()
	if err != nil || closeErr != nil || len(body) != 8 {
		t.Fatalf("body %d err %v close %v", len(body), err, closeErr)
	}
}

func TestArchiveURLAndRedirects(t *testing.T) {
	if err := validateArchiveURL("http://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip"); err == nil {
		t.Fatal("http download was accepted")
	}
	if err := validateArchiveURL(LinuxArchiveURL); err != nil {
		t.Fatal(err)
	}
	if err := validateArchiveURL(WindowsArchiveURL); err != nil {
		t.Fatal(err)
	}
	if err := validateRedirect(mustURL(t, "https://evil.example/steamcmd.zip")); err == nil {
		t.Fatal("redirect to a non-Valve host was accepted")
	}
	if err := validateRedirect(mustURL(t, "http://steampowered.com/steamcmd.zip")); err == nil {
		t.Fatal("http redirect was accepted")
	}
	for _, raw := range []string{
		"https://steamcdn-a.akamaihd.net/client/installer/steamcmd.zip",
		"https://steampowered.com/a",
		"https://cdn.steamstatic.com/a",
		"https://foo.steamcontent.com/a",
		"https://www.valvesoftware.com/a",
	} {
		if err := validateRedirect(mustURL(t, raw)); err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
	}
}

func TestAppUpdateHonorsCancel(t *testing.T) {
	dir := t.TempDir()
	src := fakepop.Binary(t)
	body, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, filepath.Base((&Client{}).Executable()))
	if err := os.WriteFile(exe, body, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fakeserver.json"), []byte(`{"hang":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, readyMarkerName), []byte("ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := New(dir)
	c.Verify = func(string) error { return nil }
	c.Download = func(context.Context, string) (io.ReadCloser, error) {
		t.Fatal("download was called")
		return nil, errors.New("network")
	}
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		errc <- c.AppUpdate(ctx, 2227360, t.TempDir(), true, true, nil)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if err == nil {
			t.Fatal("cancelled update returned nil")
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cancelled update did not return")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
